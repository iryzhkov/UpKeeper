from __future__ import annotations

import json
from collections import Counter
from pathlib import Path

import pytest

from dev_fleet.locks import LocalLockStore
from dev_fleet.upkeeper import (
    AmbiguousOutcome,
    BackendFailure,
    BackendResult,
    BackendSpec,
    DeploymentSpec,
    EffectState,
    Observation,
    Phase,
    Reconciliation,
    RecoveryRequired,
    RevisionConflict,
    RunStore,
    UpKeeper,
)

OLD = "1" * 40
NEW = "2" * 40
INFRA = "3" * 40


class DisposableServiceBackend:
    """In-memory disposable service with independently observable state."""

    def __init__(
        self,
        *,
        fail_health: bool = False,
        ambiguous_once: Phase | None = None,
    ) -> None:
        self.installed = OLD
        self.activated = OLD
        self.running = OLD
        self.healthy = True
        self.fail_health = fail_health
        self.ambiguous_once = ambiguous_once
        self.calls: Counter[Phase] = Counter()
        self.effect_calls: Counter[str] = Counter()

    def observation(self) -> Observation:
        return Observation(
            self.installed,
            self.activated,
            self.running,
            self.healthy,
            {"producer": "disposable-service-probe"},
        )

    def check(self, spec: DeploymentSpec) -> Observation:
        return self.observation()

    def execute(self, spec: DeploymentSpec, phase: Phase, effect_id: str) -> BackendResult:
        self.calls[phase] += 1
        self.effect_calls[effect_id] += 1
        if phase is Phase.INSTALL:
            self.installed = spec.desired_revision
        elif phase is Phase.ACTIVATE:
            self.activated = spec.desired_revision
        elif phase is Phase.RUN:
            self.running = spec.desired_revision
        elif phase is Phase.OBSERVE:
            self.healthy = not self.fail_health
        elif phase is Phase.ROLLBACK:
            self.installed = spec.previous_revision
            self.activated = spec.previous_revision
            self.running = spec.previous_revision
            self.healthy = True
        if self.ambiguous_once is phase:
            self.ambiguous_once = None
            raise AmbiguousOutcome("fixture lost response after applying")
        return BackendResult(
            self.observation(),
            {"effect_id": effect_id, "fixture": "disposable-service"},
        )

    def reconcile(self, spec: DeploymentSpec, phase: Phase, effect_id: str) -> Reconciliation:
        if phase is Phase.INSTALL:
            applied = self.installed == spec.desired_revision
        elif phase is Phase.ACTIVATE:
            applied = self.activated == spec.desired_revision
        elif phase is Phase.RUN:
            applied = self.running == spec.desired_revision
        elif phase is Phase.OBSERVE:
            applied = self.running == spec.desired_revision and self.healthy
        else:
            applied = (
                self.installed == spec.previous_revision
                and self.activated == spec.previous_revision
                and self.running == spec.previous_revision
                and self.healthy
            )
        return Reconciliation(
            applied,
            self.observation(),
            {"effect_id": effect_id, "producer": "disposable-service-probe"},
        )


def make_spec(run_id: str = "20260911T120000Z-1234abcd") -> DeploymentSpec:
    return DeploymentSpec(
        schema_version=1,
        deployment_id="deployment-" + "a" * 32,
        run_id=run_id,
        controller="normandy",
        component="disposable-service",
        hosts=("normandy",),
        desired_revision=NEW,
        previous_revision=OLD,
        authority_ref="authority-fixture",
        policy_evaluation_ref="policy-fixture",
        backend=BackendSpec(
            kind="homelab-ansible-v1",
            repository="ssh://git@git.ryzhkov.dev/igor/homelab-infra.git",
            repository_revision=INFRA,
            inventory="inventory/disposable.yml",
            playbook="playbooks/disposable.yml",
        ),
    )


def test_deployment_fixture_validates() -> None:
    raw = json.loads((Path(__file__).parent / "fixtures" / "upkeeper-plan-v1.json").read_text())
    raw["hosts"] = tuple(raw["hosts"])
    raw["backend"] = BackendSpec(**raw["backend"])
    assert DeploymentSpec(**raw) == make_spec()


def make_upkeeper(
    tmp_path: Path, backend: DisposableServiceBackend
) -> tuple[UpKeeper, RunStore, LocalLockStore]:
    store = RunStore(tmp_path / "runs")
    locks = LocalLockStore(tmp_path / "locks")
    return UpKeeper(store, locks, backend), store, locks


def prepare(upkeeper: UpKeeper, spec: DeploymentSpec):
    planned = upkeeper.plan(spec)
    assert planned.revision == 0
    return upkeeper.check(spec.run_id, expected_revision=planned.revision)


def test_successful_plan_check_apply_is_fenced_and_cleans_lock(tmp_path: Path) -> None:
    backend = DisposableServiceBackend()
    upkeeper, store, locks = make_upkeeper(tmp_path, backend)
    checked = prepare(upkeeper, make_spec())

    with pytest.raises(RevisionConflict, match="approval"):
        upkeeper.apply(
            checked.spec.run_id,
            expected_revision=checked.revision,
            approved_plan_hash="0" * 64,
        )

    completed = upkeeper.apply(
        checked.spec.run_id,
        expected_revision=checked.revision,
        approved_plan_hash=checked.plan_hash,
    )

    assert completed.terminal
    assert completed.events[-1].kind == "COMPLETED"
    assert backend.observation() == Observation(
        NEW,
        NEW,
        NEW,
        True,
        {"producer": "disposable-service-probe"},
    )
    assert backend.calls == Counter(
        {Phase.INSTALL: 1, Phase.ACTIVATE: 1, Phase.RUN: 1, Phase.OBSERVE: 1}
    )
    assert locks.inspect("normandy").state == "absent"
    assert store.load(checked.spec.run_id) == completed


def test_deliberate_health_failure_compensates_to_previous_revision(tmp_path: Path) -> None:
    backend = DisposableServiceBackend(fail_health=True)
    upkeeper, _, locks = make_upkeeper(tmp_path, backend)
    checked = prepare(upkeeper, make_spec())

    rolled_back = upkeeper.apply(
        checked.spec.run_id,
        expected_revision=checked.revision,
        approved_plan_hash=checked.plan_hash,
    )

    assert rolled_back.events[-1].kind == "ROLLED_BACK"
    assert backend.calls[Phase.ROLLBACK] == 1
    assert backend.observation().healthy is True
    assert backend.observation().running_revision == OLD
    assert locks.inspect("normandy").state == "absent"
    rollback = next(
        event
        for event in reversed(rolled_back.events)
        if event.data.get("phase") == Phase.ROLLBACK.value
    )
    assert rollback.data["state"] == EffectState.COMPENSATED.value


def test_lost_rollback_response_reconciles_without_duplicate_compensation(
    tmp_path: Path,
) -> None:
    backend = DisposableServiceBackend(
        fail_health=True,
        ambiguous_once=Phase.ROLLBACK,
    )
    upkeeper, store, locks = make_upkeeper(tmp_path, backend)
    checked = prepare(upkeeper, make_spec())

    with pytest.raises(RecoveryRequired, match="rollback"):
        upkeeper.apply(
            checked.spec.run_id,
            expected_revision=checked.revision,
            approved_plan_hash=checked.plan_hash,
        )

    unknown = store.load(checked.spec.run_id)
    assert backend.calls[Phase.ROLLBACK] == 1
    assert locks.inspect("normandy").state == "held"

    reconciled = upkeeper.reconcile(
        unknown.spec.run_id,
        expected_revision=unknown.revision,
    )
    rolled_back = upkeeper.apply(
        reconciled.spec.run_id,
        expected_revision=reconciled.revision,
        approved_plan_hash=reconciled.plan_hash,
    )

    assert rolled_back.events[-1].kind == "ROLLED_BACK"
    assert backend.calls[Phase.ROLLBACK] == 1
    assert locks.inspect("normandy").state == "absent"


def test_lost_apply_response_requires_observation_and_never_duplicates(tmp_path: Path) -> None:
    backend = DisposableServiceBackend(ambiguous_once=Phase.INSTALL)
    upkeeper, store, locks = make_upkeeper(tmp_path, backend)
    checked = prepare(upkeeper, make_spec())

    with pytest.raises(RecoveryRequired, match="outcome is unknown"):
        upkeeper.apply(
            checked.spec.run_id,
            expected_revision=checked.revision,
            approved_plan_hash=checked.plan_hash,
        )

    unknown = store.load(checked.spec.run_id)
    assert locks.inspect("normandy").state == "held"
    assert backend.calls[Phase.INSTALL] == 1

    with pytest.raises(RecoveryRequired, match="requires observation"):
        upkeeper.apply(
            unknown.spec.run_id,
            expected_revision=unknown.revision,
            approved_plan_hash=unknown.plan_hash,
        )
    assert backend.calls[Phase.INSTALL] == 1

    reconciled = upkeeper.reconcile(unknown.spec.run_id, expected_revision=unknown.revision)
    completed = upkeeper.apply(
        reconciled.spec.run_id,
        expected_revision=reconciled.revision,
        approved_plan_hash=reconciled.plan_hash,
    )

    assert completed.events[-1].kind == "COMPLETED"
    assert backend.calls[Phase.INSTALL] == 1
    assert locks.inspect("normandy").state == "absent"


def test_plan_is_idempotent_but_run_id_cannot_change_identity(tmp_path: Path) -> None:
    backend = DisposableServiceBackend()
    upkeeper, _, _ = make_upkeeper(tmp_path, backend)
    spec = make_spec()
    first = upkeeper.plan(spec)
    assert upkeeper.plan(spec) == first

    changed = DeploymentSpec(
        **{
            **spec.__dict__,
            "deployment_id": "deployment-" + "b" * 32,
        }
    )
    with pytest.raises(RevisionConflict, match="different plan"):
        upkeeper.plan(changed)


def test_stale_revision_and_tampered_event_fail_closed(tmp_path: Path) -> None:
    backend = DisposableServiceBackend()
    upkeeper, store, _ = make_upkeeper(tmp_path, backend)
    spec = make_spec()
    checked = prepare(upkeeper, spec)

    with pytest.raises(RevisionConflict, match="expected run revision"):
        upkeeper.check(spec.run_id, expected_revision=0)

    event_path = tmp_path / "runs" / spec.run_id / "000001.json"
    event = json.loads(event_path.read_text(encoding="utf-8"))
    event["data"]["observation"]["healthy"] = False
    event_path.write_text(json.dumps(event), encoding="utf-8")
    with pytest.raises(RecoveryRequired, match="event chain failed"):
        store.load(spec.run_id)
    assert checked.revision == 1


def test_unknown_reconciliation_stays_fenced(tmp_path: Path) -> None:
    class UnknownBackend(DisposableServiceBackend):
        def reconcile(self, spec: DeploymentSpec, phase: Phase, effect_id: str) -> Reconciliation:
            return Reconciliation(None, self.observation(), {"producer": "uncertain"})

    backend = UnknownBackend(ambiguous_once=Phase.INSTALL)
    upkeeper, store, locks = make_upkeeper(tmp_path, backend)
    checked = prepare(upkeeper, make_spec())
    with pytest.raises(RecoveryRequired):
        upkeeper.apply(
            checked.spec.run_id,
            expected_revision=checked.revision,
            approved_plan_hash=checked.plan_hash,
        )
    unknown = store.load(checked.spec.run_id)
    with pytest.raises(RecoveryRequired, match="cannot determine"):
        upkeeper.reconcile(unknown.spec.run_id, expected_revision=unknown.revision)
    assert locks.inspect("normandy").state == "held"


def test_known_preflight_mismatch_does_not_lock_or_mutate(tmp_path: Path) -> None:
    backend = DisposableServiceBackend()
    backend.healthy = False
    upkeeper, _, locks = make_upkeeper(tmp_path, backend)
    planned = upkeeper.plan(make_spec())

    with pytest.raises(BackendFailure, match="preflight"):
        upkeeper.check(planned.spec.run_id, expected_revision=planned.revision)

    assert backend.calls == Counter()
    assert locks.inspect("normandy").state == "absent"


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("repository_revision", "main"),
        ("inventory", "../inventory.yml"),
        ("playbook", "/tmp/playbook.yml"),
    ],
)
def test_backend_spec_rejects_unfenced_or_unbounded_inputs(field: str, value: str) -> None:
    values = {
        "kind": "homelab-ansible-v1",
        "repository": "ssh://git@git.ryzhkov.dev/igor/homelab-infra.git",
        "repository_revision": INFRA,
        "inventory": "inventory/disposable.yml",
        "playbook": "playbooks/disposable.yml",
    }
    values[field] = value
    with pytest.raises(ValueError):
        BackendSpec(**values)
