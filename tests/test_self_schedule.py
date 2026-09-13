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

@pytest.mark.parametrize("host", ["laptop", "normandy", "homelab", "omarchy-pc"])
def test_each_host_schedules_only_its_own_local_pull(tmp_path, monkeypatch, host):
    from dev_fleet import self_schedule
    config = load_config(ROOT)
    monkeypatch.setattr("dev_fleet.self_pull.local_machine_id_sha256",
                        lambda: config.hosts[host].machine_id_sha256)
    monkeypatch.setattr(self_schedule, "checkout_revision", lambda root: "f" * 40)
    monkeypatch.setattr(self_schedule, "load_config", lambda root: config)
    monkeypatch.setattr(self_schedule, "load_historical_release",
                        lambda root, revision: (release_for(tmp_path), revision, {}))
    def pull(config, release, **kwargs):
        assert kwargs["hosts"] == [host]
        assert kwargs["self_only"] is True
        assert kwargs["dry_run"] is True
        return {"controller": host}, 0
    monkeypatch.setattr(self_schedule, "pull_fleet", pull)
    assert self_schedule.converge(tmp_path, dry_run=True) == ({"controller": host}, 0)


def test_unknown_host_refused_before_refresh(monkeypatch, capsys):
    from dev_fleet import self_schedule
    monkeypatch.setattr("dev_fleet.self_pull.local_machine_id_sha256", lambda: "unknown")
    monkeypatch.setattr(self_schedule, "refresh",
                        lambda root: pytest.fail("unknown host must not fetch"))
    assert self_schedule.main([]) == 30
    assert "exactly one inventory match" in capsys.readouterr().out


def test_divergent_checkout_preserved(pair):
    clone, _, candidate = pair
    git(clone, "config", "user.name", "Test")
    git(clone, "config", "user.email", "test@example.com")
    git(clone, "config", "commit.gpgsign", "false")
    (clone / "local").write_text("preserve")
    git(clone, "add", ".")
    git(clone, "commit", "-m", "local change")
    before = git(clone, "rev-parse", "HEAD")
    with pytest.raises(ValueError, match="refresh failed"):
        refresh(clone, runner_for(candidate))
    assert git(clone, "rev-parse", "HEAD") == before
