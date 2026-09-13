import subprocess

import pytest
from test_pull import ROOT, release_for

from dev_fleet.config import load_config
from dev_fleet.pull import check_reachable, exit_code_for, pull_fleet
from dev_fleet.scheduled import ScheduledRunner, notification, post_report


def host(result="converged", **kwargs):
    return {"host": "homelab", "reachable": True, "result": result, **kwargs}


def test_network_skip_does_not_dispatch_or_acquire_lock(tmp_path):
    config = load_config(ROOT)
    release = release_for(tmp_path)
    offline = {"host": "homelab", "reachable": False, "result": "skipped",
               "skipped_unreachable": True}
    report, code = pull_fleet(
        config, release, hosts=["homelab"], dry_run=True, stored_files={},
        skip_unreachable=True, reachability=lambda spec: offline,
        runner=lambda *a, **kw: pytest.fail("must not dispatch"),
    )
    assert code == 0 and report["hosts"] == [offline]


@pytest.mark.parametrize("error,skipped", [
    ("ssh: connect to host x port 22: Connection timed out", True),
    ("Permission denied (publickey).", False),
    ("Host key verification failed.", False),
])
def test_reachability_keeps_authentication_failures_visible(monkeypatch, error, skipped):
    monkeypatch.setattr("dev_fleet.pull.local_machine_id_sha256", lambda: "0" * 64)
    def run(command, **kwargs):
        assert command[-1] == "true" and "StrictHostKeyChecking=yes" in command
        return subprocess.CompletedProcess(command, 255, "", error)
    monkeypatch.setattr("dev_fleet.pull.subprocess.run", run)
    result = check_reachable(load_config(ROOT).hosts["homelab"])
    assert result is not None
    assert result["skipped_unreachable"] is skipped
    assert (exit_code_for([result]) == 0) is skipped


def test_timeout_after_dispatch_is_held_across_runs(tmp_path):
    calls = []
    def lost(spec, program, **kwargs):
        calls.append(spec.name)
        return {"host": spec.name, "reachable": False, "result": "failed"}
    spec = load_config(ROOT).hosts["homelab"]
    first = ScheduledRunner(tmp_path, lost)(spec, "", timeout=60)
    second = ScheduledRunner(tmp_path, lost)(spec, "", timeout=60)
    assert not first["reachable"]
    assert "operator observation" in second["error"]
    assert calls == ["homelab"]
    assert (tmp_path / "homelab.hold").stat().st_mode & 0o777 == 0o600


def test_process_exception_keeps_hold_but_complete_success_clears_it(tmp_path):
    spec = load_config(ROOT).hosts["homelab"]
    def crash(*args, **kwargs):
        raise RuntimeError("process died")
    with pytest.raises(RuntimeError):
        ScheduledRunner(tmp_path, crash)(spec, "", timeout=60)
    assert (tmp_path / "homelab.hold").exists()
    (tmp_path / "homelab.hold").unlink()  # operator recovery in this fixture
    ScheduledRunner(tmp_path, lambda *a, **kw: host("updated"))(spec, "", timeout=60)
    assert not (tmp_path / "homelab.hold").exists()


def test_feed_silent_when_converged_or_only_offline():
    for hosts in ([host()], [{"skipped_unreachable": True, "result": "skipped"}]):
        report = {"exit_code": 0, "hosts": hosts}
        assert notification(report) is None
        post_report(report, runner=lambda *a, **kw: pytest.fail("quiet run must not post"))


@pytest.mark.parametrize("report,severity", [
    ({"exit_code": 0, "hosts": [host("updated")]}, "info"),
    ({"exit_code": 0, "hosts": [host(legacy_updater={"state": "updated"})]}, "info"),
    ({"exit_code": 40, "hosts": [host("failed")]}, "fail"),
    ({"exit_code": 30, "error": "dirty controller"}, "fail"),
])
def test_changed_and_failed_feed_posts_once(report, severity):
    calls = []
    def run(command, **kwargs):
        calls.append(command)
        assert kwargs["input"] and command[command.index("--severity") + 1] == severity
        return subprocess.CompletedProcess(command, 0, "", "")
    post_report(report, runner=run)
    assert len(calls) == 1


def test_feed_does_not_copy_large_environment_observations():
    report = {"exit_code": 0, "hosts": [host("updated", components=[
        {"name": "agent-environment", "state": "updated", "before": {"drift": "x" * 500000}}
    ])]}
    def run(command, **kwargs):
        assert len(kwargs["input"]) < 1000
        assert "before" not in kwargs["input"]
        return subprocess.CompletedProcess(command, 0, "", "")
    post_report(report, runner=run)


def test_failed_feed_post_is_not_retried():
    calls = []
    def fail(command, **kwargs):
        calls.append(command)
        return subprocess.CompletedProcess(command, 1, "", "")
    with pytest.raises(RuntimeError):
        post_report({"exit_code": 40}, runner=fail)
    assert len(calls) == 1
