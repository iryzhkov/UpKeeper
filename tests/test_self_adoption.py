from __future__ import annotations

import json
from pathlib import Path

import pytest

from dev_fleet.adoption import AdoptionSpec, FileIdentity, RollbackUnit
from dev_fleet.locks import LocalLockStore
from dev_fleet.upkeeper import (
    BackendResult,
    BackendSpec,
    DeploymentSpec,
    Observation,
    Phase,
    Reconciliation,
    RunStore,
    UpKeeper,
)

OLD, NEW, DIGEST, ROLLBACK_DIGEST = "1" * 40, "2" * 40, "a" * 64, "b" * 64


def file(path: str, digest: str = DIGEST) -> FileIdentity:
    return FileIdentity(path, digest, 123)


def adoption(mode: str = "upgrade") -> AdoptionSpec:
    return AdoptionSpec(
        1,
        mode,
        "https://github.com/iryzhkov/dev-fleet.git",
        NEW,
        file("/tmp/upkeeper-candidate.whl"),
        "/opt/upkeeper/releases/new/upkeeper",
        file("/etc/upkeeper/config.json"),
        ("/opt/upkeeper/releases/new/upkeeper", "doctor"),
        "upkeeper-run-v1",
        RollbackUnit(
            file("/usr/local/libexec/upkeeper-external-rollback", ROLLBACK_DIGEST),
            file("/var/lib/upkeeper-rollback/previous.whl", ROLLBACK_DIGEST),
            file("/var/lib/upkeeper-rollback/config.json", ROLLBACK_DIGEST),
            file("/var/lib/upkeeper-rollback/README.md", ROLLBACK_DIGEST),
        ),
    )


def spec(mode: str = "upgrade", previous: str | None = OLD) -> DeploymentSpec:
    return DeploymentSpec(
        1,
        "deployment-" + "d" * 32,
        "20260911T180000Z-abcd1234",
        "laptop",
        "upkeeper",
        ("laptop",),
        NEW,
        previous,
        "authority-f04",
        "policy-f04-review",
        BackendSpec(
            "homelab-ansible-v1",
            "https://github.com/iryzhkov/homelab-infra.git",
            "3" * 40,
            "inventory/hosts.yml",
            "playbooks/citadel-foundations.yml",
        ),
        adoption(mode),
    )


class Backend:
    def __init__(self, current: Observation) -> None:
        self.current = current

    def check(self, _spec: DeploymentSpec) -> Observation:
        return self.current

    def execute(self, deployment: DeploymentSpec, phase: Phase, effect_id: str) -> BackendResult:
        if phase is Phase.ROLLBACK:
            if deployment.previous_revision is None:
                self.current = Observation(None, None, None, False, {"probe": "absent"})
            else:
                rollback = deployment.adoption.rollback
                self.current = Observation(
                    deployment.previous_revision,
                    deployment.previous_revision,
                    deployment.previous_revision,
                    True,
                    {"probe": "rollback"},
                    rollback.artifact.sha256,
                    rollback.artifact.sha256,
                    rollback.configuration.sha256,
                )
        elif phase is Phase.OBSERVE:
            release = deployment.adoption
            self.current = Observation(
                deployment.desired_revision,
                deployment.desired_revision,
                deployment.desired_revision,
                True,
                {"probe": "healthy"},
                release.artifact.sha256,
                release.artifact.sha256,
                release.configuration.sha256,
            )
        return BackendResult(self.current, {"effect_id": effect_id})

    def reconcile(self, _spec: DeploymentSpec, _phase: Phase, effect_id: str) -> Reconciliation:
        return Reconciliation(True, self.current, {"effect_id": effect_id})


def test_self_adoption_is_part_of_frozen_intent(tmp_path: Path) -> None:
    planned = RunStore(tmp_path).create(spec())
    raw = json.loads((tmp_path / planned.spec.run_id / "intent.json").read_text())
    assert raw["adoption"]["rollback"]["executor"]["sha256"] == ROLLBACK_DIGEST
    assert RunStore(tmp_path).load(planned.spec.run_id) == planned


def test_current_state_adoption_may_keep_code_revision(tmp_path: Path) -> None:
    value = spec("adopt-running", NEW)
    current = Observation(NEW, NEW, NEW, True, {}, DIGEST, DIGEST, DIGEST)
    upkeeper = UpKeeper(
        RunStore(tmp_path / "runs"), LocalLockStore(tmp_path / "locks"), Backend(current)
    )
    assert (
        upkeeper.check(value.run_id, expected_revision=upkeeper.plan(value).revision)
        .events[-1]
        .kind
        == "CHECKED"
    )


def test_first_install_requires_explicit_absent_mode(tmp_path: Path) -> None:
    value = spec("bootstrap-absent", None)
    current = Observation(None, None, None, False, {"probe": "absent"})
    upkeeper = UpKeeper(
        RunStore(tmp_path / "runs"), LocalLockStore(tmp_path / "locks"), Backend(current)
    )
    assert (
        upkeeper.check(value.run_id, expected_revision=upkeeper.plan(value).revision)
        .events[-1]
        .kind
        == "CHECKED"
    )
    with pytest.raises(ValueError, match="bootstrap-absent"):
        spec("upgrade", None)


def test_desired_health_requires_exact_artifact_and_configuration(tmp_path: Path) -> None:
    class WrongArtifact(Backend):
        def execute(
            self, deployment: DeploymentSpec, phase: Phase, effect_id: str
        ) -> BackendResult:
            result = super().execute(deployment, phase, effect_id)
            if phase is Phase.OBSERVE:
                self.current = Observation(NEW, NEW, NEW, True, {}, "f" * 64, DIGEST, DIGEST)
                return BackendResult(self.current, result.receipt)
            return result

    value = spec()
    rollback = value.adoption.rollback
    previous = Observation(
        OLD,
        OLD,
        OLD,
        True,
        {},
        rollback.artifact.sha256,
        rollback.artifact.sha256,
        rollback.configuration.sha256,
    )
    upkeeper = UpKeeper(
        RunStore(tmp_path / "runs"), LocalLockStore(tmp_path / "locks"), WrongArtifact(previous)
    )
    checked = upkeeper.check(value.run_id, expected_revision=upkeeper.plan(value).revision)
    settled = upkeeper.apply(
        value.run_id, expected_revision=checked.revision, approved_plan_hash=checked.plan_hash
    )
    assert settled.events[-1].kind == "ROLLED_BACK"


def test_rollback_executor_cannot_be_managed_by_upkeeper() -> None:
    value = adoption()
    with pytest.raises(ValueError, match="outside managed"):
        AdoptionSpec(
            **{
                **value.__dict__,
                "rollback": RollbackUnit(
                    file("/opt/upkeeper/releases/new/upkeeper"),
                    value.rollback.artifact,
                    value.rollback.configuration,
                    value.rollback.runbook,
                ),
            }
        )


def test_fixture_validates_and_bootstrap_can_restore_absence() -> None:
    from dev_fleet.adoption import adoption_from_dict

    raw = json.loads(
        (Path(__file__).parent / "fixtures/upkeeper-self-adoption-v1.json").read_text()
    )
    parsed = adoption_from_dict(raw)
    assert parsed.mode == "adopt-running"
    assert parsed.health_command[-2:] == ("status", "--json")

    bootstrap = AdoptionSpec(
        **{
            **parsed.__dict__,
            "mode": "bootstrap-absent",
            "rollback": RollbackUnit(
                parsed.rollback.executor,
                None,
                None,
                parsed.rollback.runbook,
            ),
        }
    )
    assert bootstrap.rollback.artifact is None
