import json
import subprocess

import pytest
from test_pull import ROOT, release_for

from dev_fleet.config import load_config
from dev_fleet.pull import pull_fleet
from dev_fleet.scheduled import ScheduledRunner
from dev_fleet.self_schedule import refresh


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], text=True).strip()


@pytest.fixture
def pair(tmp_path, monkeypatch):
    source = tmp_path / "source"
    source.mkdir()
    git(source, "init", "-b", "main")
    git(source, "config", "user.name", "Test")
    git(source, "config", "user.email", "test@example.com")
    git(source, "config", "commit.gpgsign", "false")
    (source / "release").write_text("old")
    git(source, "add", ".")
    git(source, "commit", "-m", "initial")
    clone = tmp_path / "clone"
    subprocess.run(["git", "clone", str(source), str(clone)], check=True, capture_output=True)
    before = git(clone, "rev-parse", "HEAD")
    (source / "release").write_text("new")
    git(source, "commit", "-am", "promote")
    candidate = git(source, "rev-parse", "HEAD")
    monkeypatch.setattr("dev_fleet.self_schedule.ORIGINS", {str(source)})
    return clone, before, candidate


def runner_for(candidate, conclusion="success", status="completed"):
    def run(command, **kwargs):
        if command[0] == "gh":
            assert command[command.index("--commit") + 1] == candidate
            assert command[command.index("--event") + 1] == "push"
            return subprocess.CompletedProcess(command, 0, json.dumps([
                {"headSha": candidate, "status": status, "conclusion": conclusion}
            ]), "")
        return subprocess.run(command, **kwargs)
    return run


def test_refresh_fast_forwards_only_after_exact_push_ci(pair):
    clone, before, candidate = pair
    assert refresh(clone, runner_for(candidate)) == candidate
    assert git(clone, "rev-parse", "HEAD") == candidate
    assert before != candidate


@pytest.mark.parametrize("status,conclusion", [
    ("in_progress", ""), ("completed", "failure"), ("completed", "cancelled"),
])
def test_unpassed_ci_preserves_checked_out_release(pair, status, conclusion):
    clone, before, candidate = pair
    with pytest.raises(ValueError, match="successful GitHub"):
        refresh(clone, runner_for(candidate, conclusion, status))
    assert git(clone, "rev-parse", "HEAD") == before
    assert (clone / "release").read_text() == "old"


def test_dirty_checkout_refused_before_any_network(pair):
    clone, before, _ = pair
    (clone / "uncommitted").write_text("preserve")
    with pytest.raises(ValueError, match="dirty"):
        refresh(clone, lambda *a, **kw: pytest.fail("network must not run"))
    assert git(clone, "rev-parse", "HEAD") == before


def test_wrong_origin_refused_before_fetch(pair):
    clone, _, candidate = pair
    git(clone, "remote", "set-url", "origin", "https://example.invalid/untrusted")
    with pytest.raises(ValueError, match="canonical"):
        refresh(clone, runner_for(candidate))


def test_controller_never_probes_or_dispatches_self_managed_laptop(tmp_path):
    report, code = pull_fleet(
        load_config(ROOT), release_for(tmp_path), hosts=["laptop"], stored_files={},
        dry_run=True, skip_unreachable=True,
        runner=lambda *a, **kw: pytest.fail("controller dispatch"),
        reachability=lambda *a: pytest.fail("controller SSH probe"),
    )
    assert code == 0
    assert report["hosts"][0]["self_managed"]


def test_scheduled_runner_carries_local_only_fence(tmp_path):
    def run(spec, program, *, timeout, local_only):
        assert local_only
        return {"host": spec.name, "reachable": True, "result": "converged"}
    runner = ScheduledRunner(tmp_path, run)
    runner(load_config(ROOT).hosts["laptop"], "", timeout=60, local_only=True)
    assert not list(tmp_path.glob("*.hold"))
