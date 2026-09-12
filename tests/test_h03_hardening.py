from __future__ import annotations

import json
from collections import Counter
from pathlib import Path

import pytest

from dev_fleet.environment import ProbeResult, environment_from_dict
from dev_fleet.locks import LocalLockStore
from dev_fleet.upkeeper import (
    BackendResult,
    BackendSpec,
    DeploymentSpec,
    EffectState,
    Observation,
    Phase,
    RecoveryRequired,
    RevisionConflict,
    RunStore,
    UpKeeper,
)

ENVIRONMENT_FIXTURE = Path(__file__).parent / "fixtures" / "agent-environment-v1.json"
FAULT_FIXTURE = Path(__file__).parent / "fixtures" / "h03-environment-faults-v1.json"
OLD = "1" * 40
NEW = "2" * 40
INFRA = "3" * 40
EVIDENCE = "a" * 64


def load_environment():
    return environment_from_dict(json.loads(ENVIRONMENT_FIXTURE.read_text(encoding="utf-8")))


def deployment(run_suffix: str = "facefeed") -> DeploymentSpec:
    environment = load_environment()
    return DeploymentSpec(
        schema_version=1,
        deployment_id="deployment-" + "e" * 32,
        run_id=f"20260911T220000Z-{run_suffix}",
        controller="normandy",
        component="agent-environment",
        hosts=("normandy",),
        desired_revision=NEW,
        previous_revision=OLD,
        authority_ref="authority-h03-source-only",
        policy_evaluation_ref="policy-h03-fixture",
        backend=BackendSpec(
            "homelab-ansible-v1",
            "ssh://git@git.ryzhkov.dev/igor/homelab-infra.git",
            INFRA,
            "inventory/agent-environment.yml",
            "playbooks/agent-environment.yml",
        ),
        environment=environment,
    )


class EnvironmentBackend:
    """Deterministic non-executing target model for the frozen H03 faults."""

    def __init__(self, store: RunStore, value: DeploymentSpec, fault: str | None = None) -> None:
        assert value.environment is not None
        self.store = store
        self.spec = value.environment
        self.fault = fault
        self.installed = self.activated = self.running = OLD
        self.installed_environment = self.active_environment = (
            self.spec.previous_environment_revision
        )
        self.installed_assets = self._prior_assets()
        self.active_assets = self._prior_assets()
        self.worker_ready = True
        self.local_changes: set[str] = set()
        self.calls: Counter[Phase] = Counter()
        self.started_before_execute: list[Phase] = []

    def _desired_assets(self) -> dict[str, str | None]:
        return {item.asset_id: item.sha256 for item in self.spec.assets}

    def _prior_assets(self) -> dict[str, str | None]:
        return {item.asset_id: item.sha256 for item in self.spec.prior_assets}

    def _probe_payload(self, *, candidate: bool) -> list[dict[str, object]]:
        payload = []
        for probe in self.spec.probes:
            claims = probe.safety_claims
            if (
                candidate
                and self.fault == "weakened-safety"
                and probe.probe_id == "skill-safety-policy"
            ):
                claims = ()
            result = ProbeResult(probe.probe_id, True, claims, EVIDENCE)
            payload.append(
                {
                    "probe_id": result.probe_id,
                    "passed": result.passed,
                    "safety_claims": list(result.safety_claims),
                    "evidence_sha256": result.evidence_sha256,
                }
            )
        return payload

    def observation(self) -> Observation:
        instructions = {
            f"{item.repository}@{item.repository_revision}:{item.path}": item.sha256
            for item in self.spec.project_instructions
        }
        environment = {
            "schema_version": 1,
            "host_profile": self.spec.host_profile,
            "worker_id": self.spec.worker_ids[0],
            "installed_environment_revision": self.installed_environment,
            "active_environment_revision": self.active_environment,
            "worker_ready": self.worker_ready,
            "installed_assets": dict(self.installed_assets),
            "active_assets": dict(self.active_assets),
            "project_instructions": instructions,
            "baseline_probes": self._probe_payload(candidate=False),
            "candidate_probes": self._probe_payload(candidate=True),
            "unexplained_paths": sorted(self.local_changes),
            "preserved_unexplained_paths": (
                sorted(self.local_changes)
                if self.installed_environment == self.spec.previous_environment_revision
                and self.active_environment == self.spec.previous_environment_revision
                else []
            ),
        }
        return Observation(
            self.installed,
            self.activated,
            self.running,
            True,
            {"producer": "h03-deterministic-fixture", "agent_environment": environment},
        )

    def check(self, spec: DeploymentSpec) -> Observation:
        return self.observation()

    def execute(self, spec: DeploymentSpec, phase: Phase, effect_id: str) -> BackendResult:
        current = self.store.load(spec.run_id)
        latest = current.events[-1]
        assert latest.data.get("phase") == phase.value
        assert latest.data.get("state") == EffectState.STARTED.value
        self.started_before_execute.append(phase)
        self.calls[phase] += 1
        if phase is Phase.INSTALL:
            self.installed = NEW
            self.installed_environment = self.spec.revision
            self.installed_assets = self._desired_assets()
            if self.fault == "partial-install":
                self.installed_assets[next(iter(self.installed_assets))] = None
        elif phase is Phase.ACTIVATE:
            self.activated = NEW
            self.active_environment = self.spec.revision
            self.active_assets = self._desired_assets()
            self.worker_ready = False
            if self.fault == "bad-environment-revision":
                self.active_environment = "f" * 64
        elif phase is Phase.RUN:
            self.running = NEW
            self.worker_ready = True
        elif phase is Phase.OBSERVE:
            if self.fault == "managed-drift":
                self.active_assets[next(iter(self.active_assets))] = "f" * 64
            elif self.fault == "unexplained-drift":
                self.local_changes.add("/home/igor/.config/local-unmanaged-change")
        else:
            self.installed = self.activated = self.running = OLD
            self.installed_environment = self.active_environment = (
                self.spec.previous_environment_revision
            )
            self.installed_assets = self._prior_assets()
            self.active_assets = self._prior_assets()
            self.worker_ready = True
        return BackendResult(self.observation(), {"effect_id": effect_id, "fixture": "h03"})

    def reconcile(self, spec: DeploymentSpec, phase: Phase, effect_id: str):
        raise AssertionError("H03 fixture reconciliation is not used")


def prepared(tmp_path: Path, *, fault: str | None = None, suffix: str = "facefeed"):
    value = deployment(suffix)
    store = RunStore(tmp_path / "runs")
    locks = LocalLockStore(tmp_path / "locks")
    backend = EnvironmentBackend(store, value, fault)
    upkeeper = UpKeeper(store, locks, backend)
    snapshot = upkeeper.plan(value)
    snapshot = upkeeper.check(value.run_id, expected_revision=snapshot.revision)
    return upkeeper, store, locks, backend, snapshot


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


def test_fixture_covers_every_frozen_worker_profile() -> None:
    fixture = json.loads(FAULT_FIXTURE.read_text(encoding="utf-8"))
    environment = load_environment()
    assert fixture["schema_version"] == 1
    assert fixture["profiles"] == [
        {
            "host_profile": environment.host_profile,
            "worker_id": environment.worker_ids[0],
            "host": "normandy",
        }
    ]
    assert {item["fault_id"] for item in fixture["faults"]} == {
        "partial-install",
        "weakened-safety",
        "bad-environment-revision",
        "managed-drift",
        "unexplained-drift",
    }


def test_install_is_inert_and_activation_is_revision_fenced(tmp_path: Path) -> None:
    upkeeper, _, locks, backend, snapshot = prepared(tmp_path)
    installed = advance(upkeeper, snapshot, Phase.INSTALL)
    assert backend.active_environment == backend.spec.previous_environment_revision
    assert backend.worker_ready is True
    assert locks.inspect("normandy").state == "held"

    with pytest.raises(RevisionConflict, match="expected run revision"):
        upkeeper.activate(
            installed.spec.run_id,
            expected_revision=snapshot.revision,
            approved_plan_hash=installed.plan_hash,
        )
    activated = advance(upkeeper, installed, Phase.ACTIVATE)
    assert backend.worker_ready is False
    running = advance(upkeeper, activated, Phase.RUN)
    assert backend.worker_ready is True
    completed = advance(upkeeper, running, Phase.OBSERVE)
    assert completed.events[-1].kind == "COMPLETED"
    assert backend.started_before_execute == [
        Phase.INSTALL,
        Phase.ACTIVATE,
        Phase.RUN,
        Phase.OBSERVE,
    ]


@pytest.mark.parametrize(
    "fault",
    ["partial-install", "weakened-safety", "bad-environment-revision", "managed-drift"],
)
def test_explained_environment_faults_restore_the_complete_prior_revision(
    tmp_path: Path, fault: str
) -> None:
    upkeeper, _, locks, backend, snapshot = prepared(tmp_path, fault=fault)
    for phase in (Phase.INSTALL, Phase.ACTIVATE, Phase.RUN, Phase.OBSERVE):
        snapshot = advance(upkeeper, snapshot, phase)
        if snapshot.terminal:
            break
    assert snapshot.events[-1].kind == "ROLLED_BACK"
    assert backend.installed_environment == backend.spec.previous_environment_revision
    assert backend.active_environment == backend.spec.previous_environment_revision
    assert backend.installed_assets == backend._prior_assets()
    assert backend.active_assets == backend._prior_assets()
    assert backend.worker_ready is True
    assert backend.calls[Phase.ROLLBACK] == 1
    assert locks.inspect("normandy").state == "absent"
    if fault == "weakened-safety":
        assert backend.calls[Phase.ACTIVATE] == 0


def test_unexplained_drift_is_preserved_until_explicit_rollback(tmp_path: Path) -> None:
    upkeeper, store, locks, backend, snapshot = prepared(tmp_path, fault="unexplained-drift")
    for phase in (Phase.INSTALL, Phase.ACTIVATE, Phase.RUN):
        snapshot = advance(upkeeper, snapshot, phase)
    with pytest.raises(RecoveryRequired, match="outcome is unknown"):
        advance(upkeeper, snapshot, Phase.OBSERVE)
    unknown = store.load(snapshot.spec.run_id)
    assert unknown.events[-1].data["state"] == EffectState.UNKNOWN.value
    assert backend.calls[Phase.ROLLBACK] == 0
    assert backend.local_changes == {"/home/igor/.config/local-unmanaged-change"}
    assert locks.inspect("normandy").state == "held"

    restored = upkeeper.rollback(
        unknown.spec.run_id,
        expected_revision=unknown.revision,
        approved_plan_hash=unknown.plan_hash,
        reason="operator-selected exact prior environment",
    )
    assert restored.events[-1].kind == "ROLLED_BACK"
    assert backend.active_environment == backend.spec.previous_environment_revision
    assert backend.local_changes == {"/home/igor/.config/local-unmanaged-change"}
    assert locks.inspect("normandy").state == "absent"
