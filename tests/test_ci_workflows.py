"""Local/hosted gate parity and exact-commit release authorization."""

import importlib.util
import os
import re
import subprocess
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[1]


def load_script(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts" / f"{name}.py")
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def workflow(name):
    return yaml.safe_load((ROOT / ".github/workflows" / name).read_text())


def test_ci_run_steps_are_single_existing_make_targets():
    ci = workflow("ci.yml")
    targets = set(re.findall(r"^([a-z][a-z0-9-]*):", (ROOT / "Makefile").read_text(), re.M))
    invoked = []
    for job in ci["jobs"].values():
        for step in job["steps"]:
            if "run" not in step:
                continue
            command = step["run"]
            match = re.fullmatch(
                r'make ([a-z][a-z0-9-]*)(?: GROUP="\$\{\{ matrix.group \}\}")?', command
            )
            assert match, command
            assert match[1] in targets
            invoked.append(match[1])
    assert sorted(invoked) == sorted(
        ["ci-manifest", "ci-go-checks", "ci-race", "ci-go-gate", "ci-python"]
    )


def test_ci_job_names_dependencies_and_local_coverage():
    ci = workflow("ci.yml")
    assert ci["name"] == "phase-1-gate"
    assert set(ci["jobs"]) == {"manifest-gate", "go-checks", "go-race", "go-gate", "gate"}
    assert ci["jobs"]["go-gate"]["needs"] == ["manifest-gate", "go-checks", "go-race"]
    assert ci["jobs"]["go-gate"]["if"] == "${{ always() }}"
    assert ci["jobs"]["go-checks"]["needs"] == "manifest-gate"
    assert ci["jobs"]["go-race"]["needs"] == "manifest-gate"
    local = load_script("ci-local")
    race = ci["jobs"]["go-race"]
    assert race["name"] == "go-race (${{ matrix.group }})"
    assert race["strategy"]["fail-fast"] is False
    groups = race["strategy"]["matrix"]["group"]
    assert groups == ["author-baseline", "author-catalog", "author-other", "other-packages"]
    assert list(local.TARGETS) == [
        ("ci-manifest",), ("ci-go-checks",),
        *[("ci-race", f"GROUP={group}") for group in groups],
        ("ci-go-gate",), ("ci-python",),
    ]


@pytest.mark.parametrize("path", sorted([
    *ROOT.glob(".github/workflows/*.yml"),
    *ROOT.glob(".github/workflows/*.yaml"),
    *ROOT.glob(".forgejo/workflows/*.yml"),
    *ROOT.glob(".forgejo/workflows/*.yaml"),
]), ids=lambda path: str(path.relative_to(ROOT)))
def test_every_workflow_action_uses_a_full_commit_sha(path):
    document = yaml.safe_load(path.read_text())
    for job in document["jobs"].values():
        for step in job.get("steps", []):
            if "uses" in step:
                assert re.fullmatch(r"[^@]+@[0-9a-fA-F]{40}", step["uses"]), step["uses"]


def test_workflows_derive_exact_compiler_from_go_mod():
    assert re.search(r"^toolchain go1\.26\.8$", (ROOT / "go.mod").read_text(), re.M)
    for name in ("ci.yml", "release.yml"):
        setups = [step["with"] for job in workflow(name)["jobs"].values()
                  for step in job["steps"] if step.get("uses", "").startswith("actions/setup-go@")]
        assert setups
        for job in workflow(name)["jobs"].values():
            for step in job["steps"]:
                if step.get("uses", "").startswith("actions/setup-go@"):
                    # This immutable version reads toolchain before the go directive.
                    assert step["uses"] == (
                        "actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16"
                    )
        for setup in setups:
            assert setup["go-version-file"] == "go.mod"
            assert setup["check-latest"] is False
            assert "go-version" not in setup


@pytest.mark.parametrize("status,conclusion,sha,event,expected", [
    ("completed", "success", "abc", "push", True),
    ("in_progress", None, "abc", "push", False),
    ("completed", "failure", "abc", "push", False),
    ("completed", "cancelled", "abc", "push", False),
    ("completed", "success", "other", "push", False),
    ("completed", "success", "abc", "pull_request", False),
])
def test_release_gate_requires_exact_commit_success(status, conclusion, sha, event, expected):
    gate = load_script("check-release-ci")
    assert gate.passed([{"headSha": sha, "status": status, "conclusion": conclusion,
                         "event": event}], "abc") is expected
    assert not gate.passed([], "abc")


def test_release_gate_does_not_use_an_older_success():
    gate = load_script("check-release-ci")
    assert not gate.passed([
        {"headSha": "abc", "status": "completed", "conclusion": "failure", "event": "push"},
        {"headSha": "abc", "status": "completed", "conclusion": "success", "event": "push"},
    ], "abc")


@pytest.mark.parametrize("private", [False, True], ids=["public", "private"])
def test_release_authorizes_before_build_and_publish_with_optional_attestation(private):
    release = workflow("release.yml")
    assert release["name"] == "release"
    assert set(release["jobs"]) == {"release"}
    steps = release["jobs"]["release"]["steps"]
    runs = [step.get("run") for step in steps]
    gate_index = runs.index("python3 scripts/check-release-ci.py")
    build_index = runs.index("make go-cross")
    attest_index = next(i for i, step in enumerate(steps)
                        if step.get("uses", "").startswith("actions/attest-build-provenance@"))
    assert steps[attest_index]["uses"] == (
        "actions/attest-build-provenance@96278af6caaf10aea03fd8d33a09a777ca52d62f"
    )
    publish_index = next(i for i, step in enumerate(steps)
                         if step["name"] == "Publish the release")
    checksum_index = next(i for i, step in enumerate(steps)
                          if step["name"] == "Record the toolchain and the checksums")
    notes_index = next(i for i, step in enumerate(steps)
                      if step["name"] == "Write the release notes")
    assert gate_index < build_index < attest_index < checksum_index < notes_index < publish_index
    assert steps[attest_index]["if"] == "${{ !github.event.repository.private }}"
    # Only attestation is conditional. Every other step uses success() by
    # default, so skipping attestation cannot bypass a failed CI gate.
    assert "if" not in release["jobs"]["release"]
    for i, step in enumerate(steps):
        assert step.get("continue-on-error", False) is False
        if i != attest_index:
            assert "if" not in step
    active_indices = [i for i in range(len(steps)) if not (private and i == attest_index)]
    assert gate_index in active_indices
    assert active_indices.index(gate_index) < active_indices.index(build_index)
    assert active_indices.index(build_index) < active_indices.index(checksum_index)
    assert active_indices.index(checksum_index) < active_indices.index(publish_index)
    assert (attest_index in active_indices) is (not private)
    assert "gh release create" in steps[publish_index]["run"]
    assert "--notes-file bin/NOTES.md" in steps[publish_index]["run"]
    assert steps[attest_index]["with"]["subject-path"].splitlines() == [
        "bin/upkeeper-linux-amd64", "bin/upkeeper-linux-arm64",
    ]
    assert release["permissions"]["actions"] == "read"
    assert release["permissions"]["id-token"] == "write"
    assert release["permissions"]["attestations"] == "write"


@pytest.mark.parametrize("private", [False, True], ids=["public", "private"])
def test_release_notes_disclose_skipped_private_attestation(tmp_path, private):
    steps = workflow("release.yml")["jobs"]["release"]["steps"]
    notes = next(step for step in steps if step["name"] == "Write the release notes")
    assert notes["env"]["REPOSITORY_PRIVATE"] == "${{ github.event.repository.private }}"
    (tmp_path / "bin").mkdir()
    (tmp_path / "bin/TOOLCHAIN").write_text("go version test\n")
    (tmp_path / "bin/SHA256SUMS").write_text("test-checksums\n")
    subprocess.run(
        ["bash", "-e", "-o", "pipefail", "-c", notes["run"]],
        cwd=tmp_path, check=True,
        env={**os.environ, "REPOSITORY_PRIVATE": str(private).lower(),
             "GITHUB_REF_NAME": "v0.1.31", "GITHUB_SHA": "abc"},
    )
    body = (tmp_path / "bin/NOTES.md").read_text()
    disclosure = "Provenance attestation was skipped because the repository is private."
    assert (disclosure in body) is private
    assert "test-checksums" in body


@pytest.mark.parametrize("failed", [
    None, "ci-manifest", "ci-go-checks", "GROUP=author-catalog", "ci-python",
])
def test_local_job_failures_match_dependency_and_matrix_behavior(monkeypatch, failed):
    local = load_script("ci-local")
    monkeypatch.setenv("UPKEEPER_POLICY_INPUTS_READY", "1")
    calls = []

    def fake_call(argv, env):
        calls.append((argv, env))
        if argv[1] == "ci-go-gate":
            return int(any(env[key] != "success"
                           for key in ("MANIFEST_RESULT", "CHECKS_RESULT", "RACE_RESULT")))
        return int(failed in argv)

    monkeypatch.setattr(local.subprocess, "call", fake_call)
    assert local.run() == (0 if failed is None else 1)
    names = [argv[1] for argv, _ in calls]
    assert names[-1] == "ci-python"
    gate_env = next(env for argv, env in calls if argv[1] == "ci-go-gate")
    if failed == "ci-manifest":
        assert names == ["ci-manifest", "ci-go-gate", "ci-python"]
        assert gate_env["MANIFEST_RESULT"] == "failure"
        assert gate_env["CHECKS_RESULT"] == gate_env["RACE_RESULT"] == "skipped"
    else:
        assert names.count("ci-race") == 4
        assert gate_env["CHECKS_RESULT"] == ("failure" if failed == "ci-go-checks" else "success")
        assert gate_env["RACE_RESULT"] == (
            "failure" if failed == "GROUP=author-catalog" else "success"
        )
