import subprocess

import pytest
from test_pull import release_for
from test_release_u0 import remote_namespace

from dev_fleet.rollback import load_historical_release
from dev_fleet.t3_lifecycle import retire_updater, seed_t3_service
from dev_fleet.validation import ValidationError


class Units:
    def __init__(self, home, active=False):
        self.home = home
        self.active = active
        self.commands = []
        self.enabled = True
        directory = home / ".config/systemd/user"
        directory.mkdir(parents=True)
        for name in ("t3-update.timer", "t3-update.service"):
            (directory / name).write_text("legacy unit")

    def run(self, argv, **kwargs):
        name = argv[3]
        path = self.home / ".config/systemd/user" / name
        if not path.exists():
            return {"ok": False, "output": "LoadState=not-found\nActiveState=inactive\n"}
        active = self.active if name.endswith(".service") else self.enabled
        return {"ok": True, "output": (
            "LoadState=loaded\nActiveState=" + ("active" if active else "inactive")
            + "\nFragmentPath=" + str(path) + "\n"
        )}

    def must(self, argv, **kwargs):
        self.commands.append(argv)
        if "t3-update.timer" in argv:
            self.enabled = False


def test_retirement_is_no_write_in_dry_run_and_idempotent(tmp_path):
    units = Units(tmp_path)
    assert retire_updater(tmp_path, units.run, units.must, True)["state"] == "updated"
    assert not units.commands
    assert units.enabled
    assert retire_updater(tmp_path, units.run, units.must, False)["state"] == "updated"
    assert not units.enabled
    assert not list((tmp_path / ".config/systemd/user").iterdir())
    commands = list(units.commands)
    assert retire_updater(tmp_path, units.run, units.must, False)["state"] == "converged"
    assert commands == units.commands


def test_running_updater_is_not_killed_or_deleted(tmp_path):
    units = Units(tmp_path, active=True)
    assert retire_updater(tmp_path, units.run, units.must, False)["state"] == "deferred"
    assert not units.enabled
    assert len(list((tmp_path / ".config/systemd/user").iterdir())) == 2
    assert all("t3-update.service" not in argv for argv in units.commands)


def test_unavailable_unit_observation_stops_retirement(tmp_path):
    with pytest.raises(RuntimeError, match="cannot observe"):
        retire_updater(tmp_path, lambda *a, **kw: {"ok": False, "output": ""},
                       lambda *a, **kw: pytest.fail("must not mutate"), False)


def test_failed_disable_keeps_unit_files(tmp_path):
    units = Units(tmp_path)
    def fail(*a, **kw):
        raise RuntimeError("injected disable failure")
    with pytest.raises(RuntimeError, match="injected"):
        retire_updater(tmp_path, units.run, fail, False)
    assert len(list((tmp_path / ".config/systemd/user").iterdir())) == 2


def test_seed_preserves_existing_service_and_warns_without_reading_oauth(tmp_path):
    units = Units(tmp_path)
    actions = []
    assert seed_t3_service(tmp_path, units.run, units.must, True, actions)
    assert not (tmp_path / ".config/systemd/user/t3code.service").exists()
    assert "oauth.env" in actions[0]
    assert seed_t3_service(tmp_path, units.run, units.must, False, actions)
    path = tmp_path / ".config/systemd/user/t3code.service"
    assert "--host 127.0.0.1" in path.read_text()
    assert "EnvironmentFile=-%h/.config/claude/oauth.env" in path.read_text()
    path.write_text("host-local bind address and settings")
    units.commands.clear()
    assert not seed_t3_service(tmp_path, units.run, units.must, False, [])
    assert path.read_text() == "host-local bind address and settings"
    assert not units.commands


def git(root, *args):
    return subprocess.run(["git", "-C", str(root), *args], check=True,
                          text=True, capture_output=True).stdout.strip()


def historical_repo(tmp_path):
    release = release_for(tmp_path)
    path = tmp_path / "desired/release.json"
    path.write_bytes(release.canonical_bytes())
    git(tmp_path, "init", "-b", "main")
    git(tmp_path, "config", "user.name", "Test")
    git(tmp_path, "config", "user.email", "test@example.com")
    git(tmp_path, "add", ".")
    git(tmp_path, "commit", "-m", "Record previous release")
    old = git(tmp_path, "rev-parse", "HEAD")
    companion = tmp_path / "desired/agent-config/codex/AGENTS.md"
    companion.write_text("newer or dirty bytes")
    path.write_text("current manifest deliberately malformed")
    git(tmp_path, "add", ".")
    git(tmp_path, "commit", "-m", "Change current files")
    return old, path, companion


def test_rollback_reads_manifest_and_companions_from_same_commit(tmp_path):
    old, path, companion = historical_repo(tmp_path)
    head = git(tmp_path, "rev-parse", "HEAD")
    before = (path.read_bytes(), companion.read_bytes())
    release, resolved, stored = load_historical_release(tmp_path, old[:9])
    assert resolved == old
    assert release.components["t3-steward"]["version"] == "0.11.0-rc.1"
    assert stored == {"codex/AGENTS.md": "# Global agent instructions\n"}
    assert git(tmp_path, "rev-parse", "HEAD") == head
    assert before == (path.read_bytes(), companion.read_bytes())


@pytest.mark.parametrize(
    "revision", ["--help", "HEAD", "main:desired/release.json", "../outside", "a" * 40]
)
def test_rollback_rejects_bad_or_missing_commit(tmp_path, revision):
    historical_repo(tmp_path)
    with pytest.raises(ValidationError):
        load_historical_release(tmp_path, revision)


def test_rollback_rejects_unrelated_history_and_missing_companion(tmp_path):
    old, _, companion = historical_repo(tmp_path)
    git(tmp_path, "checkout", "--orphan", "unrelated")
    git(tmp_path, "commit", "-m", "Unrelated root")
    with pytest.raises(ValidationError):
        load_historical_release(tmp_path, old)
    git(tmp_path, "checkout", "main")
    git(tmp_path, "checkout", old, "--", "desired/release.json")
    companion.unlink()
    git(tmp_path, "add", ".")
    git(tmp_path, "commit", "-m", "Break historical companion")
    with pytest.raises(ValidationError):
        load_historical_release(tmp_path, git(tmp_path, "rev-parse", "HEAD"))


def test_historical_steward_uses_pinned_installer_and_candidate_range(tmp_path):
    ns = remote_namespace(tmp_path)
    ns["COMPONENTS"] = {"t3-steward": {"version": "0.11.0-rc.1"}}
    ns["run"] = lambda *a, **kw: {
        "ok": True, "output": "t3-steward 0.11.0-rc.1 (tested with T3 0.0.38..0.0.38)"}
    assert ns["verify_pair_binary"]("candidate") == ("0.0.38", "0.0.38")
    ns["COMPONENTS"]["t3-steward"]["version"] = "0.11.0-rc.2"
    with pytest.raises(Exception, match="pinned version"):
        ns["verify_pair_binary"]("candidate")
    calls = []
    ns["apply_pinned_steward"] = lambda *a: calls.append(a) or "updated"
    assert ns["apply_steward"]({}, {}, []) == "updated"
    assert len(calls) == 1


def test_retired_modules_are_refused_without_running_setup(tmp_path):
    ns = remote_namespace(tmp_path)
    ns["must"] = lambda *a, **kw: pytest.fail("retired installer must not execute")
    with pytest.raises(Exception, match="retired fleet modules"):
        ns["run_modules"](tmp_path, ["45-agents"])
