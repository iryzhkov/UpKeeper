from __future__ import annotations

import subprocess
from pathlib import Path

import pytest

from dev_fleet.ansible_backend import AnsibleBackend, CommandResult
from dev_fleet.upkeeper import (
    BackendFailure,
    BackendSpec,
    DeploymentSpec,
    Observation,
    Phase,
)

OLD = "1" * 40
NEW = "2" * 40


def git(root: Path, *args: str) -> str:
    return subprocess.run(
        ["git", "-C", str(root), *args],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()


def repository(tmp_path: Path) -> tuple[Path, str, str]:
    root = tmp_path / "homelab-infra"
    root.mkdir()
    git(root, "init")
    git(root, "config", "user.email", "test@example.invalid")
    git(root, "config", "user.name", "Test")
    (root / "inventory").mkdir()
    (root / "playbooks").mkdir()
    (root / "inventory" / "disposable.yml").write_text(
        "all:\n  hosts:\n    normandy:\n", encoding="utf-8"
    )
    (root / "playbooks" / "disposable.yml").write_text(
        "- hosts: all\n  gather_facts: false\n  tasks: []\n", encoding="utf-8"
    )
    git(root, "add", ".")
    git(root, "commit", "-m", "fixture")
    remote = "ssh://git@git.ryzhkov.dev/igor/homelab-infra.git"
    git(root, "remote", "add", "gitea", remote)
    return root, git(root, "rev-parse", "HEAD"), remote


def spec(revision: str, remote: str) -> DeploymentSpec:
    return DeploymentSpec(
        schema_version=1,
        deployment_id="deployment-" + "a" * 32,
        run_id="20260911T130000Z-1234abcd",
        controller="normandy",
        component="disposable-service",
        hosts=("normandy",),
        desired_revision=NEW,
        previous_revision=OLD,
        authority_ref="authority-fixture",
        policy_evaluation_ref="policy-fixture",
        backend=BackendSpec(
            "homelab-ansible-v1",
            remote,
            revision,
            "inventory/disposable.yml",
            "playbooks/disposable.yml",
        ),
    )


def test_backend_builds_only_bounded_argument_vector(tmp_path: Path) -> None:
    root, revision, remote = repository(tmp_path)
    current = {"value": Observation(OLD, OLD, OLD, True, {"producer": "fixture"})}
    calls: list[tuple[list[str], Path, int]] = []

    def runner(argv, cwd, timeout):
        calls.append((list(argv), cwd, timeout))
        current["value"] = Observation(NEW, OLD, OLD, True, {"producer": "fixture"})
        return CommandResult(0, b"ok", b"")

    backend = AnsibleBackend(root, observer=lambda _: current["value"], runner=runner)
    result = backend.execute(spec(revision, remote), Phase.INSTALL, "effect-" + "a" * 64)

    assert result.observation.installed_revision == NEW
    assert calls == [
        (
            [
                "ansible-playbook",
                "-i",
                str(root / "inventory" / "disposable.yml"),
                str(root / "playbooks" / "disposable.yml"),
                "--limit",
                "normandy",
                "--tags",
                "upkeeper_install",
                "--extra-vars",
                "upkeeper_effect_id=effect-" + "a" * 64,
            ],
            root,
            300,
        )
    ]
    assert set(result.receipt) == {
        "backend",
        "returncode",
        "stdout_sha256",
        "stderr_sha256",
    }


def test_backend_rejects_dirty_or_wrong_repository_identity(tmp_path: Path) -> None:
    root, revision, remote = repository(tmp_path)
    observation = Observation(OLD, OLD, OLD, True, {})
    backend = AnsibleBackend(root, observer=lambda _: observation)

    (root / "untracked").write_text("dirty", encoding="utf-8")
    with pytest.raises(BackendFailure, match="not clean"):
        backend.check(spec(revision, remote))
    (root / "untracked").unlink()

    with pytest.raises(BackendFailure, match="identity"):
        backend.check(spec(revision, "ssh://git@git.ryzhkov.dev/igor/other.git"))


def test_backend_reconcile_uses_independent_observation(tmp_path: Path) -> None:
    root, revision, remote = repository(tmp_path)
    desired = Observation(NEW, NEW, NEW, True, {"probe": "healthy"})
    backend = AnsibleBackend(root, observer=lambda _: desired)

    reconciled = backend.reconcile(spec(revision, remote), Phase.ACTIVATE, "effect-" + "b" * 64)

    assert reconciled.applied is True
    assert reconciled.evidence["source"] == "independent-observer"


def test_nonzero_ansible_result_is_known_failure(tmp_path: Path) -> None:
    root, revision, remote = repository(tmp_path)
    backend = AnsibleBackend(
        root,
        observer=lambda _: Observation(OLD, OLD, OLD, True, {}),
        runner=lambda _argv, _cwd, _timeout: CommandResult(2, b"", b"failed"),
    )

    with pytest.raises(BackendFailure, match="exited 2"):
        backend.execute(spec(revision, remote), Phase.INSTALL, "effect-" + "c" * 64)
