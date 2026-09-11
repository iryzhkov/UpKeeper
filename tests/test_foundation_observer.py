from __future__ import annotations

import hashlib
import json
from pathlib import Path

from dev_fleet.adoption import AdoptionSpec, FileIdentity, RollbackUnit
from dev_fleet.foundation_observer import FoundationObserver
from dev_fleet.upkeeper import BackendSpec, DeploymentSpec


def identity(path: Path) -> FileIdentity:
    data = path.read_bytes()
    return FileIdentity(str(path), hashlib.sha256(data).hexdigest(), len(data))


def test_observer_adopts_exact_running_bytes(tmp_path: Path) -> None:
    executable = tmp_path / "component"
    executable.write_text("#!/bin/sh\nexit 0\n")
    executable.chmod(0o755)
    executor = tmp_path / "rollback"
    executor.write_text("#!/bin/sh\nexit 0\n")
    runbook = tmp_path / "rollback.md"
    runbook.write_text("rollback")
    adoption = AdoptionSpec(
        1,
        "adopt-running",
        "https://example.test/component.git",
        "2" * 40,
        identity(executable),
        str(executable),
        None,
        (str(executable),),
        "state-v1",
        RollbackUnit(identity(executor), identity(executor), None, identity(runbook)),
    )
    spec = DeploymentSpec(
        1,
        "deployment-" + "d" * 32,
        "20260911T180000Z-abcd1234",
        "laptop",
        "component",
        ("laptop",),
        "2" * 40,
        "2" * 40,
        "authority",
        "approval",
        BackendSpec(
            "homelab-ansible-v1",
            "https://example.test/homelab.git",
            "3" * 40,
            "inventory/hosts.yml",
            "playbooks/citadel-foundations.yml",
        ),
        adoption,
    )

    observed = FoundationObserver()(spec)
    assert observed.healthy is True
    assert observed.running_revision == spec.desired_revision
    assert observed.installed_artifact_sha256 == adoption.artifact.sha256


def test_observer_reports_absence(tmp_path: Path) -> None:
    raw = json.loads(
        (Path(__file__).parent / "fixtures/upkeeper-self-adoption-v1.json").read_text()
    )
    raw["artifact"]["path"] = str(tmp_path / "staged")
    raw["install_path"] = str(tmp_path / "missing")
    raw["health_command"] = ["/bin/true"]
    raw["rollback"]["executor"]["path"] = str(tmp_path / "rollback")
    raw["rollback"]["runbook"]["path"] = str(tmp_path / "rollback.md")
    from dev_fleet.adoption import adoption_from_dict

    adoption = adoption_from_dict(raw)
    spec = DeploymentSpec(
        1,
        "deployment-" + "d" * 32,
        "20260911T180000Z-abcd1234",
        "laptop",
        "upkeeper",
        ("laptop",),
        "2" * 40,
        None,
        "authority",
        "approval",
        BackendSpec(
            "homelab-ansible-v1",
            "https://example.test/homelab.git",
            "3" * 40,
            "inventory/hosts.yml",
            "playbooks/citadel-foundations.yml",
        ),
        AdoptionSpec(
            **{
                **adoption.__dict__,
                "mode": "bootstrap-absent",
                "rollback": RollbackUnit(
                    adoption.rollback.executor, None, None, adoption.rollback.runbook
                ),
            }
        ),
    )
    assert FoundationObserver()(spec).evidence["probe"] == "absent"
