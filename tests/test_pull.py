import json
import os
import subprocess
import sys
from pathlib import Path

from dev_fleet.config import load_config
from dev_fleet.locks import LocalLockStore
from dev_fleet.pull import _agent_program, exit_code_for, pull_fleet, run_host
from dev_fleet.records import RunRecordStore
from dev_fleet.release import DEFAULT_MANIFEST, validate_release

ROOT = Path(__file__).resolve().parents[1]


def release_for(tmp_path: Path):
    manifest = json.loads(json.dumps(DEFAULT_MANIFEST))
    manifest.update({"captured_at": "2026-09-12T00:00:00Z", "captured_from": "normandy"})
    components = manifest["components"]
    components["huyang"]["commit"] = "a" * 40
    components["omarchy-setup"]["commit"] = "b" * 40
    components["t3-steward"]["version"] = "0.11.0-rc.1"
    content = "# Global agent instructions\n"
    import hashlib

    components["agent-config"]["files"][0]["sha256"] = hashlib.sha256(content.encode()).hexdigest()
    stored = tmp_path / "desired/agent-config/codex/AGENTS.md"
    stored.parent.mkdir(parents=True)
    stored.write_text(content, encoding="utf-8")
    return validate_release(manifest)


def test_remote_pull_uses_the_same_strict_transport_as_probe(monkeypatch) -> None:
    config = load_config(ROOT)
    spec = config.hosts["homelab"]
    monkeypatch.setattr("dev_fleet.pull.local_machine_id_sha256", lambda: "0" * 64)
    captured = {}

    def fake_run(command, **kwargs):
        captured["command"] = command
        captured["input"] = kwargs["input"]
        stdout = '{"host":"homelab","result":"converged"}'
        return subprocess.CompletedProcess(command, 0, stdout=stdout, stderr="")

    monkeypatch.setattr("dev_fleet.pull.subprocess.run", fake_run)
    program = _agent_program(
        {
            "run_id": "x",
            "dry_run": True,
            "host": {"name": "homelab"},
            "components": [],
            "manifest": {"components": {}},
            "stored_files": {},
            "lock": {},
        }
    )
    result = run_host(spec, program, timeout=60)

    assert result["reachable"] is True and result["result"] == "converged"
    assert "StrictHostKeyChecking=yes" in captured["command"]
    assert captured["command"][-2:] == ["python3", "-"]
    assert "__PAYLOAD__" not in captured["input"]


def test_dry_run_agent_observes_a_bare_home_without_changing_it(tmp_path: Path) -> None:
    """The remote program runs locally against an empty HOME and must only report."""
    home = tmp_path / "home"
    (home / ".codex").mkdir(parents=True)
    (home / ".codex/AGENTS.md").write_text("old\n", encoding="utf-8")
    release = release_for(tmp_path)
    payload = {
        "run_id": "20260912T000000Z-deadbeef",
        "dry_run": True,
        "host": {"name": "normandy", "os_id": "omarchy"},
        "components": ["omarchy-setup", "agent-config", "huyang", "t3-steward"],
        "manifest": release.manifest,
        "stored_files": {"codex/AGENTS.md": "# Global agent instructions\n"},
        "lock": {"run_id": "20260912T000000Z-deadbeef"},
    }
    environment = {**os.environ, "HOME": str(home), "XDG_RUNTIME_DIR": str(tmp_path / "run")}
    completed = subprocess.run(
        [sys.executable, "-"],
        input=_agent_program(payload),
        capture_output=True,
        check=False,
        text=True,
        env=environment,
        timeout=120,
    )
    assert completed.returncode == 0, completed.stderr
    report = json.loads(completed.stdout.splitlines()[-1])
    states = {entry["name"]: entry["state"] for entry in report["components"]}
    # A missing checkout is a failure to bootstrap by hand, never something pull invents.
    assert states["omarchy-setup"] == "failed"
    assert all(state == "skipped" for name, state in states.items() if name != "omarchy-setup")
    assert (home / ".codex/AGENTS.md").read_text(encoding="utf-8") == "old\n"
    assert not (home / ".local/state/dev-fleet/locks" / "normandy.lock").exists()


def test_pull_locks_hosts_and_writes_a_run_record(tmp_path: Path) -> None:
    config = load_config(ROOT)
    release = release_for(tmp_path)
    (tmp_path / "inventory").mkdir()
    locks = LocalLockStore(tmp_path / "locks")
    records = RunRecordStore(tmp_path / "runs")
    seen = []

    def fake_runner(spec, program, *, timeout):
        seen.append(spec.name)
        assert locks.inspect(spec.name).state == "held"
        return {"host": spec.name, "reachable": True, "result": "updated", "components": []}

    config = type(config)(root=tmp_path, hosts=config.hosts, manifest=config.manifest)
    report, exit_code = pull_fleet(
        config,
        release,
        hosts=["homelab", "omarchy-pc"],
        lock_store=locks,
        record_store=records,
        runner=fake_runner,
        manifest_commit="c" * 40,
    )
    assert exit_code == 0 and seen == ["homelab", "omarchy-pc"]
    assert locks.inspect("homelab").state == "absent"
    stored = records.read(report["run_id"])
    assert stored["result"] == "success" and stored["details"]["hosts"][0]["host"] == "homelab"


def test_exit_codes_rank_worst_outcome_first() -> None:
    assert exit_code_for([{"reachable": True, "result": "converged"}]) == 0
    assert exit_code_for([{"reachable": True, "result": "ahead"}]) == 10
    assert exit_code_for([{"reachable": False, "result": "failed"}]) == 40
    assert exit_code_for([{"reachable": True, "result": "lock-contention"}]) == 60
    assert exit_code_for([{"reachable": True, "result": "rolled-back"}]) == 40
    worst = [{"reachable": True, "result": "rollback-failed"}, {"reachable": False}]
    assert exit_code_for(worst) == 50
