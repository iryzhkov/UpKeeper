import ast
import os
import subprocess

import pytest

from dev_fleet.huyang_lifecycle import bootstrap_huyang
from dev_fleet.pull import REMOTE_AGENT


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], text=True).strip()


@pytest.fixture
def source(tmp_path, monkeypatch):
    root = tmp_path / "source"
    root.mkdir()
    git(root, "init", "-b", "main")
    (root / "Makefile").write_text("build:\n\tmkdir -p bin\n\tprintf pinned > bin/huyang\n")
    git(root, "add", ".")
    git(root, "-c", "user.name=Test", "-c", "user.email=test@example.com",
        "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
    spec = {"source": str(root), "ref": "main", "commit": git(root, "rev-parse", "HEAD"),
            "path": ".local/share/huyang", "service": "huyang.service",
            "build": ["make", "build"], "probe": {"tools": 19}}
    monkeypatch.setattr("dev_fleet.huyang_lifecycle.shutil.which", lambda *a, **k: "/bin/true")
    return spec


def installer(home, spec, dry_run=False, fail_build=False, probe_ok=True, commands=None):
    commands = [] if commands is None else commands
    def must(argv, **kwargs):
        commands.append(argv)
        if argv[0] == "systemctl":
            return ""
        if fail_build and argv[0] == "make":
            raise RuntimeError("injected build failure")
        return subprocess.check_output(argv, cwd=kwargs.get("cwd"), text=True)
    result = bootstrap_huyang(
        home, spec, "fixture", dry_run, os.environ, must,
        lambda checkout, count: {"ok": probe_ok, "tools": count}, [],
    )
    return result, commands


def test_absent_checkout_is_cloned_pinned_built_and_service_seeded(tmp_path, source):
    home = tmp_path / "home"
    home.mkdir()
    result, commands = installer(home, source)
    checkout = home / source["path"]
    assert result == "updated"
    assert git(checkout, "rev-parse", "HEAD") == source["commit"]
    assert (checkout / "bin/huyang").read_text() == "pinned"
    unit = home / ".config/systemd/user/huyang.service"
    assert "ExecStart=%h/.local/share/huyang/bin/huyang serve" in unit.read_text()
    assert ["systemctl", "--user", "enable", "--now", "huyang.service"] in commands


def test_absent_checkout_preview_writes_nothing(tmp_path, source):
    home = tmp_path / "home"
    home.mkdir()
    assert installer(home, source, dry_run=True)[0] == "updated"
    assert list(home.iterdir()) == []


@pytest.mark.parametrize("kind", ["directory", "symlink", "unit"])
def test_preexisting_paths_are_preserved(tmp_path, source, kind):
    home = tmp_path / "home"
    checkout = home / source["path"]
    checkout.parent.mkdir(parents=True)
    unit = home / ".config/systemd/user/huyang.service"
    if kind == "directory":
        checkout.mkdir()
    elif kind == "symlink":
        checkout.symlink_to(tmp_path / "absent")
    else:
        unit = home / ".config/systemd/user/huyang.service"
        unit.parent.mkdir(parents=True)
        unit.write_text("custom")
    with pytest.raises(ValueError):
        installer(home, source)
    if kind == "unit":
        assert unit.read_text() == "custom"
    else:
        assert checkout.exists() or checkout.is_symlink()


def test_failed_build_never_installs_checkout_or_unit(tmp_path, source):
    home = tmp_path / "home"
    home.mkdir()
    with pytest.raises(RuntimeError, match="build failure"):
        installer(home, source, fail_build=True)
    assert not (home / source["path"]).exists()
    assert not (home / ".config/systemd/user/huyang.service").exists()


def test_failed_first_start_disables_unit_and_retains_diagnostic_checkout(tmp_path, source):
    home = tmp_path / "home"
    home.mkdir()
    commands = []
    with pytest.raises(ValueError, match="bootstrap probe failed"):
        installer(home, source, probe_ok=False, commands=commands)
    assert ["systemctl", "--user", "disable", "--now", "huyang.service"] in commands
    assert (home / source["path"] / "bin/huyang").exists()


def test_unreachable_pin_fails_before_activation(tmp_path, source):
    home = tmp_path / "home"
    home.mkdir()
    source["commit"] = "a" * 40
    with pytest.raises(subprocess.CalledProcessError):
        installer(home, source)
    assert not (home / source["path"]).exists()


@pytest.mark.parametrize("rollback", [False, True])
def test_normal_pull_preserves_newer_checkout_but_explicit_rollback_can_move(tmp_path, rollback):
    node = next(n for n in ast.parse(REMOTE_AGENT).body
                if isinstance(n, ast.FunctionDef) and n.name == "apply_huyang")
    calls = []
    env = {
        "HOME": tmp_path, "DRY_RUN": True, "PAYLOAD": {"allow_downgrade": rollback},
        "git_contains": lambda *args: True,
    }
    exec(compile(ast.Module(body=[node], type_ignores=[]), "<agent-test>", "exec"), env)  # noqa: S102
    spec = {"path": ".local/share/huyang", "commit": "a" * 40, "ref": "main"}
    before = {"commit": "b" * 40, "dirty": False, "service_active": True, "socket": True}
    state = env["apply_huyang"](spec, before, calls)
    assert state == ("updated" if rollback else "ahead")
    assert ("would fetch" in calls[0]) is rollback


@pytest.mark.parametrize("dirty,changed_head,diverged", [
    (True, False, False), (False, True, False), (False, False, True),
])
def test_huyang_preserves_work_that_changes_or_diverges_before_activation(
    tmp_path, dirty, changed_head, diverged,
):
    node = next(n for n in ast.parse(REMOTE_AGENT).body
                if isinstance(n, ast.FunctionDef) and n.name == "apply_huyang")
    old, pin = "b" * 40, "a" * 40
    env = {
        "HOME": tmp_path, "DRY_RUN": False, "PAYLOAD": {},
        "StepFailed": ValueError,
        "git_contains": lambda checkout, ancestor, descendant:
            ancestor == old and not diverged,
        "must": lambda *a, **kw: None,
        "run": lambda *a, **kw: {"ok": True},
        "git_dirty": lambda checkout: dirty,
        "git_head": lambda checkout: "c" * 40 if changed_head else old,
        "huyang_build_and_restart": lambda *a: pytest.fail("must preserve development"),
    }
    exec(compile(ast.Module(body=[node], type_ignores=[]), "<agent-test>", "exec"), env)  # noqa: S102
    spec = {"path": ".local/share/huyang", "commit": pin, "ref": "main"}
    before = {"commit": old, "dirty": False, "service_active": True, "socket": True}
    if dirty or changed_head:
        with pytest.raises(ValueError, match="preserving local work"):
            env["apply_huyang"](spec, before, [])
    else:
        assert env["apply_huyang"](spec, before, []) == "ahead"


def test_dirty_huyang_never_fetches_or_activates(tmp_path):
    node = next(n for n in ast.parse(REMOTE_AGENT).body
                if isinstance(n, ast.FunctionDef) and n.name == "apply_huyang")
    env = {"HOME": tmp_path, "StepFailed": ValueError}
    exec(compile(ast.Module(body=[node], type_ignores=[]), "<agent-test>", "exec"), env)  # noqa: S102
    with pytest.raises(ValueError, match="local changes"):
        env["apply_huyang"]({"path": ".local/share/huyang"},
                            {"commit": "a" * 40, "dirty": True}, [])
