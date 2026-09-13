import base64
import json
import os
import stat
import tomllib

import pytest
from test_release import FakeMachine, fake_home

from dev_fleet.environment import ProjectInstructionRef
from dev_fleet.pull import _agent_program, exit_code_for
from dev_fleet.release import CaptureError, _capture_t3, validate_release
from dev_fleet.release_assets import (
    CLAUDE_BEGIN,
    CLAUDE_END,
    apply_environment,
    capture_environment,
    observe_environment,
    validate_environment,
)
from dev_fleet.worker_release import (
    DESTINATION,
    resolve_secret_reference,
    validate_workers,
    worker_projection,
)

PROVENANCE = ProjectInstructionRef("github.com/iryzhkov/dev-fleet", "a" * 40, "README.md", "b" * 64)


def environment(tmp_path):
    (tmp_path / "repo").mkdir()
    home = fake_home(tmp_path)
    component = capture_environment(home, PROVENANCE)
    return home, component


def test_environment_preserves_host_settings_and_rolls_back_the_whole_file_set(tmp_path):
    home, component = environment(tmp_path)
    claude = home / ".claude.json"
    claude.write_text(
        '{"mcpServers":{"local":{"command":"local"}},"oauthAccount":{"token":"private"}}'
    )
    settings = home / ".claude/settings.json"
    settings.write_text(
        '{"hooks":{"local":"keep"},"permissions":{"allow":["local"],"deny":["danger"]}}'
    )
    codex = home / ".codex/config.toml"
    codex.write_text(
        '[projects."/private"]\ntrust_level="trusted"\n[mcp_servers.local]\ncommand="local"\n'
    )
    instructions = home / ".claude/CLAUDE.md"
    instructions.write_text(
        "Local rules\n\n" + CLAUDE_BEGIN + "\nstale\n" + CLAUDE_END + "\nTail\n"
    )
    before = {f["path"]: (home / f["path"]).read_bytes() for f in component["files"]}
    calls = []

    def fail_second(path):
        calls.append(path)
        if len(calls) == 2:
            raise OSError("injected activation failure")

    result = apply_environment(component, home, "rollback-test", fault=fail_second)
    assert result["state"] == "rolled-back"
    assert all((home / p).read_bytes() == data for p, data in before.items())
    result = apply_environment(component, home, "success-test")
    assert result["state"] == "updated"
    assert json.loads(claude.read_text())["oauthAccount"]["token"] == "private"  # noqa: S105
    assert "local" in json.loads(claude.read_text())["mcpServers"]
    assert json.loads(settings.read_text())["hooks"] == {"local": "keep"}
    assert json.loads(settings.read_text())["permissions"]["deny"] == ["danger"]
    assert json.loads(settings.read_text())["permissions"]["allow"] == ["local"]
    assert tomllib.loads(codex.read_text())["projects"]["/private"]["trust_level"] == "trusted"
    assert "Local rules" in instructions.read_text() and "Tail" in instructions.read_text()
    assert apply_environment(component, home, "noop")["state"] == "converged"
    assert not (home / ".local/state/dev-fleet/environments/noop").exists()


def test_target_local_mcp_credentials_survive_owned_registration_updates(tmp_path):
    home, component = environment(tmp_path)
    claude = home / ".claude.json"
    claude.write_text('{"mcpServers":{"huyang":{"command":"old","env":{"API_TOKEN":"private"}}}}')
    codex = home / ".codex/config.toml"
    codex.write_text(
        '[mcp_servers.huyang]\ncommand="old"\n[mcp_servers.huyang.env]\nAPI_TOKEN="private"\n'
    )
    opencode = home / ".config/opencode/opencode.json"
    opencode.write_text(
        '{"mcp":{"huyang":{"command":["old"],"environment":{"API_TOKEN":"private"}}}}'
    )
    apply_environment(component, home, "preserve-mcp")
    assert json.loads(claude.read_text())["mcpServers"]["huyang"]["env"]["API_TOKEN"] == "private"  # noqa: S105
    assert (
        tomllib.loads(codex.read_text())["mcp_servers"]["huyang"]["env"]["API_TOKEN"] == "private"  # noqa: S105
    )
    assert (
        json.loads(opencode.read_text())["mcp"]["huyang"]["environment"]["API_TOKEN"] == "private"  # noqa: S105
    )


def test_missing_files_dry_run_and_cross_host_rebasing(tmp_path):
    _, component = environment(tmp_path)
    home = tmp_path / "other"
    home.mkdir()
    result = apply_environment(component, home, "dry", dry_run=True)
    assert result["state"] == "updated"
    assert list(home.iterdir()) == []
    apply_environment(component, home, "install")
    spec, _, _, drift = observe_environment(component, home)
    assert all(str(home) in asset.active_path for asset in spec.assets)
    assert all(d["state"] == "MATCH" for d in drift)


def test_rejects_corrupt_payload_and_escaping_symlink(tmp_path):
    home, component = environment(tmp_path)
    corrupt = json.loads(json.dumps(component))
    corrupt["files"][0]["content"] = base64.b64encode(b"corrupt").decode()
    with pytest.raises(ValueError, match="digest"):
        validate_environment(corrupt)
    target = home / ".codex/AGENTS.md"
    target.unlink()
    target.symlink_to(tmp_path / "outside")
    with pytest.raises(ValueError, match="escapes"):
        observe_environment(component, home)


def test_capture_rejects_credentials_in_owned_registrations(tmp_path):
    home, _ = environment(tmp_path)
    (home / ".claude.json").write_text(
        '{"mcpServers":{"bad":{"command":"client","env":{"API_TOKEN":"private"}}}}'
    )
    with pytest.raises(ValueError, match="environment key"):
        capture_environment(home, PROVENANCE)


def test_no_blind_overwrite_when_file_changes_during_activation(tmp_path):
    home, component = environment(tmp_path)
    path = home / ".claude/CLAUDE.md"
    path.write_text("old")
    changed_paths = []

    def concurrent_edit(changed):
        changed_paths.append(changed)
        (home / changed).write_text("concurrent")
        raise OSError("failure")

    with pytest.raises(RuntimeError, match="unexplained"):
        apply_environment(component, home, "conflict", fault=concurrent_edit)
    assert (home / changed_paths[0]).read_text() == "concurrent"


def workers():
    return {
        "kind": "worker-configuration",
        "hosts": {
            "homelab": {
                "schema_version": 1,
                "worker_id": "homelab",
                "coordinator_id": "normandy",
                "transport": "ssh",
                "capabilities": ["git", "huyang"],
                "provider_routes": [],
                "credential_ref": "secretref:f02-protocol/homelab",
            }
        },
    }


def test_worker_contract_is_host_scoped_ref_only_and_never_enrolls(tmp_path):
    component = workers()
    validate_workers(component)
    secret = tmp_path / ".config/upkeeper/secrets/f02-protocol/homelab"
    secret.parent.mkdir(parents=True)
    secret.write_text("credential-not-for-records")
    secret.chmod(0o600)
    assert (
        resolve_secret_reference(tmp_path, component["hosts"]["homelab"]["credential_ref"])
        == secret
    )
    assert not worker_projection(component, tmp_path, "omarchy-pc", "x")["configured"]
    result = worker_projection(component, tmp_path, "homelab", "dry", dry_run=True)
    assert result["state"] == "updated" and not (tmp_path / DESTINATION).exists()
    result = worker_projection(component, tmp_path, "homelab", "apply")
    assert result["enrolled"] is False
    assert "credential-not-for-records" not in json.dumps(result)
    assert "credential-not-for-records" not in (tmp_path / DESTINATION).read_text()
    assert stat.S_IMODE((tmp_path / DESTINATION).stat().st_mode) == 0o600
    assert worker_projection(component, tmp_path, "homelab", "again")["state"] == "converged"
    secret.chmod(0o644)
    with pytest.raises(ValueError, match="0600"):
        worker_projection(component, tmp_path, "homelab", "refuse")
    component["hosts"]["homelab"]["credential"] = "never allowed"
    with pytest.raises(ValueError, match="unknown"):
        validate_workers(component)


def test_worker_requires_reference_availability_without_creating_target(tmp_path):
    with pytest.raises(FileNotFoundError):
        worker_projection(workers(), tmp_path, "homelab", "missing")
    assert not (tmp_path / DESTINATION).exists()


def remote_namespace(tmp_path):
    payload = {
        "host": {"name": "normandy"},
        "run_id": "test",
        "dry_run": False,
        "components": [],
        "manifest": {"components": {}},
        "stored_files": {},
    }
    program = _agent_program(payload)
    # Execute the actual transmitted implementation without its CLI main.
    namespace = {"__name__": "pull_test"}
    import sys

    previous = {
        k: v for k, v in sys.modules.items() if k == "dev_fleet" or k.startswith("dev_fleet.")
    }
    try:
        exec(program.rsplit("\nmain()", 1)[0], namespace)  # noqa: S102
    finally:
        sys.modules.update(previous)
    namespace["HOME"] = tmp_path
    return namespace


@pytest.mark.parametrize("running,ok", [(1, True), (None, True), (0, False)])
def test_t3_never_installs_or_restarts_without_proven_idle(tmp_path, running, ok):
    ns = remote_namespace(tmp_path)
    ns["COMPONENTS"] = {"t3-steward": {"path": ".local/bin/t3-steward"}}
    ns["verify_pair_binary"] = lambda _: None
    ns["observe_t3"] = lambda _: {"check_ok": ok, "running_threads": running, "installed": "0.0.37"}
    ns["must"] = lambda *a, **k: pytest.fail("must not mutate")
    assert (
        ns["apply_t3"](
            {"version": "0.0.38"},
            {"installed": "0.0.37", "served": "0.0.37", "service_active": True},
            [],
        )
        == "deferred"
    )
    assert exit_code_for([{"reachable": True, "result": "deferred"}]) == 10


def test_t3_rechecks_idle_after_install_and_verifies_served_version(tmp_path):
    ns = remote_namespace(tmp_path)
    ns["COMPONENTS"] = {"t3-steward": {"path": ".local/bin/t3-steward"}}
    ns["verify_pair_binary"] = lambda _: None
    observations = iter(
        [
            {"check_ok": True, "running_threads": 0, "installed": "0.0.37"},
            {"check_ok": True, "running_threads": 2, "installed": "0.0.38"},
        ]
    )
    ns["observe_t3"] = lambda _: next(observations)
    commands = []
    ns["must"] = lambda argv, **kwargs: commands.append(argv)
    assert (
        ns["apply_t3"](
            {"version": "0.0.38"},
            {"installed": "0.0.37", "served": "0.0.37", "service_active": True},
            [],
        )
        == "deferred"
    )
    assert ["npm", "install", "-g", "t3@0.0.38"] in commands
    assert not any("restart" in c for c in commands)


def test_capture_compatibility_and_manifest_cannot_lie_about_pair(tmp_path):
    machine = FakeMachine()
    assert (
        _capture_t3(machine, tmp_path, {"path": ".local/bin/t3-steward", "version": "0.11.0-rc.1"})[
            "version"
        ]
        == "0.0.38"
    )
    from dev_fleet.release import capture_release

    (tmp_path / "repo").mkdir()
    home = fake_home(tmp_path)
    release = capture_release(tmp_path / "repo", controller="normandy", home=home, runner=machine)
    release.components["t3"]["version"] = "0.0.39"
    with pytest.raises(ValueError, match="outside"):
        validate_release(release.manifest)

    class NoRange(FakeMachine):
        def __call__(self, argv, cwd, timeout):
            result = super().__call__(argv, cwd, timeout)
            if argv[-1] == "version":
                result.stdout = "t3-steward 0.11.0"
            return result

    with pytest.raises(CaptureError, match="range"):
        _capture_t3(NoRange(), tmp_path, {"path": ".local/bin/t3-steward", "version": "0.11.0"})


def test_remote_agent_has_no_repository_import_dependency(tmp_path):
    import subprocess
    import sys

    home, component = environment(tmp_path)
    payload = {
        "host": {"name": "normandy"},
        "run_id": "embedded",
        "dry_run": True,
        "components": ["agent-environment"],
        "manifest": {"components": {"agent-environment": component}},
        "stored_files": {},
        "lock": {"run_id": "embedded"},
    }
    target = tmp_path / "empty"
    target.mkdir()
    result = subprocess.run(
        [sys.executable, "-I", "-"],
        input=_agent_program(payload),
        text=True,
        capture_output=True,
        env={**os.environ, "HOME": str(target)},
        cwd=target,
        check=False,
    )
    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout)["result"] == "updated"
    assert list(target.iterdir()) == []
