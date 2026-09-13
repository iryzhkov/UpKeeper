import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest
from test_pull import release_for

from dev_fleet.config import load_config
from dev_fleet.probe import local_machine_id_sha256
from dev_fleet.pull import run_host
from dev_fleet.self_pull import checkout_revision, local_host
from dev_fleet.validation import ValidationError

ROOT = Path(__file__).resolve().parents[1]


def git(root, *args):
    return subprocess.run(["git", "-C", str(root), *args], check=True,
                          text=True, capture_output=True).stdout.strip()


def self_checkout(tmp_path):
    root = tmp_path / "repo"
    root.mkdir()
    release = release_for(root)
    (root / "desired/release.json").write_bytes(release.canonical_bytes())
    shutil.copytree(ROOT / "src", root / "src", ignore=shutil.ignore_patterns("__pycache__"))
    (root / "scripts").mkdir()
    shutil.copy2(ROOT / "scripts/upkeeper", root / "scripts/upkeeper")
    shutil.copy2(ROOT / "desired/components.yml", root / "desired/components.yml")
    (root / "inventory").mkdir()
    inventory = json.loads((ROOT / "inventory/hosts.yml").read_text())
    inventory["all"]["hosts"]["normandy"]["machine_id_sha256"] = local_machine_id_sha256()
    (root / "inventory/hosts.yml").write_text(json.dumps(inventory))
    git(root, "init", "-b", "main")
    git(root, "config", "user.name", "Test")
    git(root, "config", "user.email", "test@example.com")
    git(root, "add", ".")
    git(root, "commit", "-m", "Record fixture release")
    home = tmp_path / "home"
    home.mkdir()
    stub = tmp_path / "bin"
    stub.mkdir()
    systemctl = stub / "systemctl"
    systemctl.write_text("#!/bin/sh\nprintf 'LoadState=not-found\\nActiveState=inactive\\n'\n")
    systemctl.chmod(0o755)
    ssh = stub / "ssh"
    ssh.write_text("#!/bin/sh\nexit 99\n")
    ssh.chmod(0o755)
    environment = {**os.environ, "HOME": str(home), "PYTHONDONTWRITEBYTECODE": "1",
                   "PATH": str(stub) + ":" + os.environ["PATH"],
                   "XDG_RUNTIME_DIR": str(tmp_path / "runtime")}
    return root, home, environment


def invoke(root, environment, *args):
    return subprocess.run([sys.executable, str(root / "scripts/upkeeper"),
                           "pull", "--self", *args], capture_output=True,
                          text=True, env=environment, check=False)


def test_self_uses_own_committed_checkout_and_only_local_transport(tmp_path):
    root, home, env = self_checkout(tmp_path)
    # An external config-root override must never turn --self into a controller pull.
    env["DEV_FLEET_CONFIG_ROOT"] = "/does/not/exist"
    result = invoke(root, env, "--components", "agent-config", "--dry-run", "--json")
    assert result.returncode == 0, result.stdout + result.stderr
    report = json.loads(result.stdout)
    assert report["self"] is True
    assert report["desired_manifest_commit"] == git(root, "rev-parse", "HEAD")
    assert [(h["host"], h["transport"]) for h in report["hosts"]] == [("normandy", "local")]
    assert report["hosts"][0]["components"][0]["state"] == "updated"
    assert list(home.iterdir()) == []
    assert git(root, "status", "--porcelain") == ""


def test_self_applies_once_and_preserves_normal_lock_and_report_semantics(tmp_path):
    root, home, env = self_checkout(tmp_path)
    result = invoke(root, env, "--components", "agent-config", "--json")
    assert result.returncode == 0, result.stdout + result.stderr
    report = json.loads(result.stdout)
    assert (home / ".codex/AGENTS.md").read_text() == "# Global agent instructions\n"
    assert list((home / ".local/state/dev-fleet/runs").rglob("*.json"))
    assert not list((home / ".local/state/dev-fleet/locks").glob("*.lock"))
    result = invoke(root, env, "--components", "agent-config", "--json")
    assert json.loads(result.stdout)["hosts"][0]["result"] == "converged"
    assert report["self"]


@pytest.mark.parametrize("mutation", ["tracked", "untracked"])
def test_self_refuses_dirty_checkout_without_writing_host(tmp_path, mutation):
    root, home, env = self_checkout(tmp_path)
    target = root / ("desired/release.json" if mutation == "tracked" else "untracked.txt")
    target.write_text("local work")
    result = invoke(root, env, "--components", "agent-config", "--json")
    assert result.returncode == 30
    assert "dirty checkout" in result.stdout
    assert list(home.iterdir()) == []


def test_self_rejects_unknown_and_ambiguous_inventory_identity(tmp_path, monkeypatch):
    root, _, _ = self_checkout(tmp_path)
    config = load_config(root)
    monkeypatch.setattr("dev_fleet.self_pull.local_machine_id_sha256", lambda: "f" * 64)
    with pytest.raises(ValidationError, match="exactly one"):
        local_host(config)
    actual = config.hosts["normandy"].machine_id_sha256
    config.hosts["alias"] = config.hosts["normandy"]
    monkeypatch.setattr("dev_fleet.self_pull.local_machine_id_sha256", lambda: actual)
    with pytest.raises(ValidationError, match="exactly one"):
        local_host(config)


def test_self_has_no_remote_fallback_if_identity_changes(monkeypatch):
    host = load_config(ROOT).hosts["omarchy-pc"]
    monkeypatch.setattr("dev_fleet.pull.local_machine_id_sha256", lambda: "f" * 64)
    monkeypatch.setattr("dev_fleet.pull.subprocess.run",
                        lambda *a, **kw: pytest.fail("must not launch SSH"))
    result = run_host(host, "unused", timeout=60, local_only=True)
    assert result["result"] == "failed"
    assert "remote fallback refused" in result["error"]


def test_self_selector_is_mutually_exclusive_and_not_rollback(tmp_path):
    root, _, env = self_checkout(tmp_path)
    assert invoke(root, env, "--all").returncode == 2
    assert invoke(root, env, "--hosts", "homelab").returncode == 2
    with pytest.raises(ValidationError):
        checkout_revision(tmp_path)
