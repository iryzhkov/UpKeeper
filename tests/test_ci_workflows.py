"""Local/hosted gate parity and exact-commit release authorization."""

import importlib.util
import json
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


def run(run_id, conclusion, ref="main", status="completed", sha="abc", event="push"):
    return {"databaseId": run_id, "headSha": sha, "status": status, "conclusion": conclusion,
            "event": event, "headBranch": ref,
            "createdAt": f"2026-10-06T15:{run_id:02d}:00Z"}


def test_release_gate_does_not_use_an_older_success():
    gate = load_script("check-release-ci")
    # gh lists the newest run first.
    assert not gate.passed([run(2, "failure"), run(1, "success")], "abc")


@pytest.mark.parametrize("runs,expected", [
    # A failed run superseded by a later success on the same ref.
    ([run(3, "success"), run(2, "failure")], "pass"),
    ([run(5, "success"), run(4, "cancelled"), run(3, "success", ref="v0.1.33"),
      run(2, "failure")], "pass"),
    # Every completed run succeeded, on every ref.
    ([run(3, "success", ref="v0.1.33"), run(2, "success")], "pass"),
    # A later success on another ref does not supersede a failure.
    ([run(3, "success", ref="v0.1.33"), run(2, "failure")], "fail"),
    # A failure after the last success on its ref is not superseded.
    ([run(4, "failure", ref="v0.1.33"), run(3, "success"), run(2, "success", ref="v0.1.33")],
     "fail"),
    ([run(2, "failure")], "fail"),
    # A run still in progress could still fail, so the gate waits for it.
    ([run(3, None, status="in_progress"), run(2, "success")], "wait"),
    ([run(3, None, status="queued", ref="v0.1.33"), run(2, "failure")], "wait"),
    ([], "wait"),
    # Runs for another commit or another event are not the tagged commit's gate.
    ([run(3, "success", sha="other"), run(2, "success", event="pull_request")], "wait"),
    ([run(3, "failure", sha="other"), run(2, "success")], "pass"),
])
def test_release_gate_requires_every_failed_run_to_be_superseded(runs, expected):
    gate = load_script("check-release-ci")
    decision, reasons = gate.decide(runs, "abc")
    assert decision == expected, reasons
    assert reasons


@pytest.mark.parametrize("runs,code,decision", [
    ([run(3, "success"), run(2, "failure")], 0, "pass"),
    ([run(3, "success", ref="v0.1.33"), run(2, "failure")], 1, "fail"),
])
def test_release_gate_prints_its_decision(monkeypatch, capsys, runs, code, decision):
    gate = load_script("check-release-ci")
    monkeypatch.setenv("GITHUB_REPOSITORY", "iryzhkov/UpKeeper")
    calls = []

    def fake_check_output(argv, **_):
        calls.append(argv)
        if argv[0] == "git":
            return "abc\n"
        return json.dumps(runs)

    monkeypatch.setattr(gate.subprocess, "check_output", fake_check_output)
    assert gate.main() == code
    output = capsys.readouterr().out
    assert f"ci.yml release gate decision for abc: {decision}" in output
    for listed in runs:
        assert f"run {listed['databaseId']}" in output
    listing = next(argv for argv in calls if argv[0] == "gh")
    assert listing[listing.index("--limit") + 1] != "1"
    assert "headBranch" in listing[listing.index("--json") + 1]


def fake_listing(monkeypatch, gate, respond):
    monkeypatch.setenv("GITHUB_REPOSITORY", "iryzhkov/UpKeeper")
    monkeypatch.setattr(gate.time, "sleep", lambda _: pytest.fail("the gate waited"))

    def fake_check_output(argv, **_):
        if argv[0] == "git":
            return "abc\n"
        return respond(int(argv[argv.index("--limit") + 1]))

    monkeypatch.setattr(gate.subprocess, "check_output", fake_check_output)


def test_release_gate_refuses_a_history_cut_at_the_retrieval_cap(monkeypatch, capsys):
    # gh returns only the newest runs up to --limit. A failure older than the
    # cap is invisible, so a full page proves nothing about the history.
    gate = load_script("check-release-ci")
    history = [run(index, "success") for index in range(5000, 0, -1)]
    history.append(run(0, "failure", ref="v0.1.33"))
    fake_listing(monkeypatch, gate, lambda limit: json.dumps(history[:limit]))
    assert gate.main() == 1
    captured = capsys.readouterr()
    assert "ci.yml release gate decision for abc: fail" in captured.out
    assert "incomplete" in captured.out + captured.err


def test_release_gate_accepts_a_history_below_the_retrieval_cap(monkeypatch):
    gate = load_script("check-release-ci")
    fake_listing(monkeypatch, gate, lambda limit: json.dumps(
        [run(index, "success") for index in range(limit - 1, 0, -1)]))
    assert gate.main() == 0


@pytest.mark.parametrize("response", [
    '{"runs": []}', '["run"]', "[null]", '[{"headSha": "abc", "event": "push", "status": 1}]',
])
def test_release_gate_refuses_a_malformed_listing(monkeypatch, response):
    gate = load_script("check-release-ci")
    fake_listing(monkeypatch, gate, lambda _: response)
    assert gate.main() == 1


def without(entry, *fields, **values):
    entry = {key: value for key, value in entry.items() if key not in fields}
    entry.update(values)
    return entry


@pytest.mark.parametrize("failed,success", [
    # Absent ref and time on both runs: None == None is not the same ref.
    (without(run(2, "failure"), "headBranch", "createdAt"),
     without(run(3, "success"), "headBranch")),
    (without(run(2, "failure"), "headBranch"), without(run(3, "success"), "headBranch")),
    (without(run(2, "failure"), headBranch=None), without(run(3, "success"), headBranch=None)),
    (without(run(2, "failure"), headBranch=""), without(run(3, "success"), headBranch="")),
    # A failure without a usable time cannot be shown to be older.
    (without(run(2, "failure"), "createdAt"), run(3, "success")),
    (without(run(2, "failure"), createdAt=None), run(3, "success")),
    (without(run(2, "failure"), createdAt=""), run(3, "success")),
    (without(run(2, "failure"), createdAt="yesterday"), run(3, "success")),
    (without(run(2, "failure"), createdAt="2026-10-06T15:02:00"), run(3, "success")),
    # Nor can a success without a usable time be shown to be later.
    (run(2, "failure"), without(run(3, "success"), "createdAt")),
    (run(2, "failure"), without(run(3, "success"), createdAt="9999")),
    (run(2, "failure"), without(run(3, "success"), createdAt=20261006)),
])
def test_release_gate_never_supersedes_a_failure_on_incomplete_evidence(failed, success):
    gate = load_script("check-release-ci")
    decision, reasons = gate.decide([success, failed], "abc")
    assert decision == "fail", reasons
    assert not gate.passed([success, failed], "abc")


@pytest.mark.parametrize("incomplete", [
    without(run(2, "failure"), "headSha"), without(run(2, "failure"), headSha=None),
    without(run(2, "failure"), "event"), without(run(2, "failure"), event=None),
    without(run(2, "failure"), headSha=1),
    # An empty, blank or padded value names no commit or event either, so it
    # cannot show that the failure belongs to another one.
    run(2, "failure", ref="release", sha=""), run(2, "failure", ref="release", event=""),
    run(2, "failure", sha="  "), run(2, "failure", event=" "),
    run(2, "failure", sha="abc "), run(2, "failure", event=" push"),
])
def test_release_gate_refuses_an_entry_it_cannot_attribute(incomplete):
    # gh filters on the commit and the event, so every entry carries both. An
    # entry without them is malformed, not a run of another commit.
    gate = load_script("check-release-ci")
    decision, reasons = gate.decide([run(3, "success"), incomplete], "abc")
    assert decision == "fail", reasons


def test_release_gate_still_ignores_runs_it_can_attribute_elsewhere():
    gate = load_script("check-release-ci")
    unrelated = [run(2, "failure", ref="release", sha="def"),
                 run(4, "failure", event="pull_request")]
    decision, reasons = gate.decide([run(3, "success"), *unrelated], "abc")
    assert decision == "pass", reasons


def test_release_gate_orders_runs_by_time_not_by_text():
    gate = load_script("check-release-ci")
    failed = without(run(2, "failure"), createdAt="2026-10-06T16:00:00Z")
    success = without(run(3, "success"), createdAt="2026-10-06T17:30:00+02:00")
    # 15:30Z is earlier than 16:00Z although the text sorts after it.
    assert gate.decide([success, failed], "abc")[0] == "fail"
    success["createdAt"] = "2026-10-06T18:30:00+02:00"
    assert gate.decide([success, failed], "abc")[0] == "pass"


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
