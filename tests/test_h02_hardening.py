from __future__ import annotations

import hashlib
import json
from collections import Counter
from pathlib import Path

import pytest

from dev_fleet.adoption import AdoptionSpec, FileIdentity, RollbackUnit
from dev_fleet.locks import LocalLockStore, LockContentionError, LockMetadata
from dev_fleet.upkeeper import (
    AmbiguousOutcome,
    BackendResult,
    BackendSpec,
    DeploymentSpec,
    EffectState,
    Observation,
    Phase,
    Reconciliation,
    RecoveryRequired,
    RunStore,
    UpKeeper,
)

OLD = "1" * 40
NEW = "2" * 40
INFRA = "3" * 40
CANDIDATE = "a" * 64
PRIOR = "b" * 64


def identity(path: str, digest: str) -> FileIdentity:
    return FileIdentity(path, digest, 100)


def adoption(component: str = "upkeeper") -> AdoptionSpec:
    return AdoptionSpec(
        1,
        "upgrade",
        f"https://github.com/iryzhkov/{component}.git",
        NEW,
        identity(f"/var/lib/upkeeper-staging/{component}", CANDIDATE),
        f"/home/igor/.local/bin/{component}",
        identity(f"/home/igor/.config/{component}/config.json", CANDIDATE),
        (f"/home/igor/.local/bin/{component}", "status", "--json"),
        f"{component}-state-v1",
        RollbackUnit(
            identity("/home/igor/.local/libexec/upkeeper-external-rollback", PRIOR),
            identity(f"/home/igor/.local/libexec/{component}.rollback", PRIOR),
            identity(f"/home/igor/.local/libexec/{component}.config.rollback", PRIOR),
            identity(f"/home/igor/.local/share/upkeeper/rollback-{component}.md", PRIOR),
        ),
    )


def spec(component: str = "disposable-service", with_adoption: bool = False) -> DeploymentSpec:
    return DeploymentSpec(
        1,
        "deployment-" + "e" * 32,
        "20260911T210000Z-facefeed",
        "normandy",
        component,
        ("normandy",),
        NEW,
        OLD,
        "authority-h02",
        "policy-h02-review",
        BackendSpec(
            "homelab-ansible-v1",
            "ssh://git@git.ryzhkov.dev/igor/homelab-infra.git",
            INFRA,
            "inventory/hosts.yml",
            "playbooks/citadel-foundations.yml",
        ),
        adoption(component) if with_adoption else None,
    )


class BoundaryBackend:
    def __init__(
        self,
        *,
        adoption_spec: AdoptionSpec | None = None,
        ambiguous: Phase | None = None,
        unavailable_health: bool = False,
        corrupt_install: bool = False,
        missing_install: bool = False,
        unexplained_rollback: bool = False,
    ) -> None:
        self.adoption = adoption_spec
        self.installed = OLD
        self.activated = OLD
        self.running = OLD
        self.healthy: bool | None = True
        self.installed_digest = PRIOR if adoption_spec else None
        self.activated_digest = PRIOR if adoption_spec else None
        self.configuration_digest = PRIOR if adoption_spec else None
        self.ambiguous = ambiguous
        self.unavailable_health = unavailable_health
        self.corrupt_install = corrupt_install
        self.missing_install = missing_install
        self.unexplained_rollback = unexplained_rollback
        self.calls: Counter[Phase] = Counter()

    def observation(self) -> Observation:
        return Observation(
            self.installed,
            self.activated,
            self.running,
            self.healthy,
            {"producer": "h02-boundary-fixture"},
            self.installed_digest,
            self.activated_digest,
            self.configuration_digest,
        )

    def check(self, spec: DeploymentSpec) -> Observation:
        return self.observation()

    def execute(
        self, spec: DeploymentSpec, phase: Phase, effect_id: str
    ) -> BackendResult:
        self.calls[phase] += 1
        if phase is Phase.INSTALL:
            self.installed = None if self.missing_install else NEW
            if self.adoption:
                self.installed_digest = "f" * 64 if self.corrupt_install else CANDIDATE
                self.configuration_digest = CANDIDATE
        elif phase is Phase.ACTIVATE:
            self.activated = NEW
            if self.adoption:
                self.activated_digest = CANDIDATE
        elif phase is Phase.RUN:
            self.running = NEW
        elif phase is Phase.OBSERVE:
            self.healthy = None if self.unavailable_health else True
        elif self.unexplained_rollback:
            self.installed = self.activated = self.running = "f" * 40
            self.healthy = True
        else:
            self.installed = self.activated = self.running = OLD
            self.healthy = True
            if self.adoption:
                self.installed_digest = self.activated_digest = PRIOR
                self.configuration_digest = PRIOR
        if self.ambiguous is phase:
            self.ambiguous = None
            raise AmbiguousOutcome("lost fixture response after target invocation")
        return BackendResult(self.observation(), {"effect_id": effect_id, "fixture": "h02"})

    def reconcile(
        self, spec: DeploymentSpec, phase: Phase, effect_id: str
    ) -> Reconciliation:
        if phase is Phase.INSTALL:
            applied = self.installed == NEW
        elif phase is Phase.ACTIVATE:
            applied = self.activated == NEW
        elif phase is Phase.RUN:
            applied = self.running == NEW
        elif phase is Phase.OBSERVE:
            applied = self.running == NEW and self.healthy is True
        else:
            applied = (
                self.installed == OLD
                and self.activated == OLD
                and self.running == OLD
                and self.healthy is True
            )
        return Reconciliation(
            applied,
            self.observation(),
            {"effect_id": effect_id, "producer": "independent-fixture"},
        )


def prepared(tmp_path: Path, backend: BoundaryBackend, value: DeploymentSpec | None = None):
    value = value or spec()
    store = RunStore(tmp_path / "runs")
    locks = LocalLockStore(tmp_path / "locks")
    upkeeper = UpKeeper(store, locks, backend)
    planned = upkeeper.plan(value)
    checked = upkeeper.check(value.run_id, expected_revision=planned.revision)
    return upkeeper, store, locks, checked


def advance(upkeeper: UpKeeper, snapshot, phase: Phase):
    method = {
        Phase.INSTALL: upkeeper.install,
        Phase.ACTIVATE: upkeeper.activate,
        Phase.RUN: upkeeper.run,
        Phase.OBSERVE: upkeeper.observe,
    }[phase]
    return method(
        snapshot.spec.run_id,
        expected_revision=snapshot.revision,
        approved_plan_hash=snapshot.plan_hash,
    )


def test_install_activate_run_and_observe_are_independently_fenced(tmp_path: Path) -> None:
    backend = BoundaryBackend()
    upkeeper, _, locks, snapshot = prepared(tmp_path, backend)

    snapshot = advance(upkeeper, snapshot, Phase.INSTALL)
    assert backend.calls == Counter({Phase.INSTALL: 1})
    assert not snapshot.terminal
    assert locks.inspect("normandy").state == "held"

    for phase in (Phase.ACTIVATE, Phase.RUN, Phase.OBSERVE):
        snapshot = advance(upkeeper, snapshot, phase)

    assert snapshot.events[-1].kind == "COMPLETED"
    assert locks.inspect("normandy").state == "absent"


@pytest.mark.parametrize("lost_phase", list(Phase)[:-1])
def test_every_forward_lost_response_reconciles_before_retry(
    tmp_path: Path, lost_phase: Phase
) -> None:
    backend = BoundaryBackend(ambiguous=lost_phase)
    upkeeper, store, locks, snapshot = prepared(tmp_path, backend)

    for phase in (Phase.INSTALL, Phase.ACTIVATE, Phase.RUN, Phase.OBSERVE):
        if phase is lost_phase:
            with pytest.raises(RecoveryRequired, match="outcome is unknown"):
                advance(upkeeper, snapshot, phase)
            unknown = store.load(snapshot.spec.run_id)
            assert unknown.events[-1].data["state"] == EffectState.UNKNOWN.value
            calls = backend.calls[phase]
            with pytest.raises(RecoveryRequired, match="requires observation"):
                advance(upkeeper, unknown, phase)
            assert backend.calls[phase] == calls
            snapshot = upkeeper.reconcile(
                unknown.spec.run_id, expected_revision=unknown.revision
            )
        snapshot = advance(upkeeper, snapshot, phase)

    assert snapshot.events[-1].kind == "COMPLETED"
    assert backend.calls[lost_phase] == 1
    assert locks.inspect("normandy").state == "absent"


def test_unavailable_health_is_unknown_and_blocks_progression(tmp_path: Path) -> None:
    backend = BoundaryBackend(unavailable_health=True)
    upkeeper, store, locks, snapshot = prepared(tmp_path, backend)
    for phase in (Phase.INSTALL, Phase.ACTIVATE, Phase.RUN):
        snapshot = advance(upkeeper, snapshot, phase)

    with pytest.raises(RecoveryRequired, match="outcome is unknown"):
        advance(upkeeper, snapshot, Phase.OBSERVE)

    unknown = store.load(snapshot.spec.run_id)
    assert unknown.events[-1].data["state"] == EffectState.UNKNOWN.value
    assert not unknown.terminal
    assert locks.inspect("normandy").state == "held"


@pytest.mark.parametrize("fault", ["missing_install", "corrupt_install"])
def test_missing_or_corrupt_candidate_restores_exact_prior_state(
    tmp_path: Path, fault: str
) -> None:
    value = spec("upkeeper", with_adoption=True)
    backend = BoundaryBackend(
        adoption_spec=value.adoption,
        missing_install=fault == "missing_install",
        corrupt_install=fault == "corrupt_install",
    )
    upkeeper, _, locks, snapshot = prepared(tmp_path, backend, value)

    rolled_back = advance(upkeeper, snapshot, Phase.INSTALL)

    assert rolled_back.events[-1].kind == "ROLLED_BACK"
    assert backend.observation() == Observation(
        OLD,
        OLD,
        OLD,
        True,
        {"producer": "h02-boundary-fixture"},
        PRIOR,
        PRIOR,
        PRIOR,
    )
    assert locks.inspect("normandy").state == "absent"


@pytest.mark.parametrize("component", ["upkeeper", "t3-steward"])
def test_external_rollback_survives_self_update_candidate(
    tmp_path: Path, component: str
) -> None:
    value = spec(component, with_adoption=True)
    backend = BoundaryBackend(adoption_spec=value.adoption)
    upkeeper, _, _, snapshot = prepared(tmp_path, backend, value)
    installed = advance(upkeeper, snapshot, Phase.INSTALL)

    rolled_back = upkeeper.rollback(
        value.run_id,
        expected_revision=installed.revision,
        approved_plan_hash=installed.plan_hash,
        reason="candidate recovery fixture",
    )

    assert rolled_back.events[-1].kind == "ROLLED_BACK"
    assert backend.calls[Phase.ROLLBACK] == 1
    assert backend.observation().installed_artifact_sha256 == PRIOR
    assert backend.observation().configuration_sha256 == PRIOR


def test_unexplained_rollback_result_is_preserved_as_unknown(tmp_path: Path) -> None:
    value = spec("upkeeper", with_adoption=True)
    backend = BoundaryBackend(
        adoption_spec=value.adoption,
        corrupt_install=True,
        unexplained_rollback=True,
    )
    upkeeper, store, locks, snapshot = prepared(tmp_path, backend, value)

    with pytest.raises(RecoveryRequired, match="not independently confirmed"):
        advance(upkeeper, snapshot, Phase.INSTALL)

    unknown = store.load(value.run_id)
    assert unknown.events[-1].data["state"] == EffectState.UNKNOWN.value
    assert backend.observation().installed_revision == "f" * 40
    assert locks.inspect("normandy").state == "held"


def test_lock_contention_preserves_owner_and_run_revision(tmp_path: Path) -> None:
    backend = BoundaryBackend()
    upkeeper, store, locks, snapshot = prepared(tmp_path, backend)
    foreign = LockMetadata.new(
        run_id="20260911T210001Z-deadbeef",
        controller="normandy",
        controller_pid=123,
        desired_manifest_commit=INFRA,
        components=["upkeeper"],
    )
    locks.acquire("normandy", foreign)

    with pytest.raises(LockContentionError):
        advance(upkeeper, snapshot, Phase.INSTALL)

    assert store.load(snapshot.spec.run_id).revision == snapshot.revision
    assert locks.inspect("normandy").metadata == foreign
    assert backend.calls == Counter()


def transition_record(run_id: str, expected: int, kind: str, data: dict) -> bytes:
    encoded = (
        json.dumps(data, sort_keys=True, separators=(",", ":")) + "\n"
    ).encode()
    record = {
        "schema_version": 1,
        "run_id": run_id,
        "expected_revision": expected,
        "target_revision": expected + 1,
        "kind": kind,
        "data_sha256": hashlib.sha256(encoded).hexdigest(),
        "controller_pid": 999999,
        "created_at": "2026-09-11T21:00:00Z",
    }
    return (json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n").encode()


def test_local_journal_crash_recovery_is_revision_and_liveness_fenced(
    tmp_path: Path,
) -> None:
    store = RunStore(tmp_path / "runs")
    planned = store.create(spec())
    run_root = store.root / planned.spec.run_id
    lock = run_root / ".transition.lock"
    data = {"probe": "ok"}

    lock.write_bytes(transition_record(planned.spec.run_id, 0, "PROBED", data))
    with pytest.raises(RecoveryRequired, match="live or unknown"):
        store.recover_transition(
            planned.spec.run_id,
            expected_revision=0,
            controller_is_alive=lambda _pid: None,
        )
    recovered = store.recover_transition(
        planned.spec.run_id,
        expected_revision=0,
        controller_is_alive=lambda _pid: False,
    )
    assert recovered.revision == 0
    assert not lock.exists()

    committed = store.append(
        planned.spec.run_id,
        expected_revision=0,
        kind="PROBED",
        data=data,
    )
    lock.write_bytes(transition_record(planned.spec.run_id, 0, "PROBED", data))
    recovered = store.recover_transition(
        planned.spec.run_id,
        expected_revision=0,
        controller_is_alive=lambda _pid: False,
    )
    assert recovered == committed
    assert not lock.exists()
