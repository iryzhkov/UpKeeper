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
    check_public,
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


def test_local_harness_reformatting_and_account_updates_are_not_owned_drift(tmp_path):
    home, component = environment(tmp_path)
    apply_environment(component, home, "initial")
    path = home / ".claude.json"
    data = json.loads(path.read_text())
    data["localCache"] = {"new": "value"}
    text = json.dumps(data, separators=(",", ":"))
    path.write_text(text)
    result = apply_environment(component, home, "no-owned-drift")
    assert result["state"] == "converged"
    assert path.read_text() == text
    assert not (home / ".local/state/dev-fleet/environments/no-owned-drift").exists()


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


def test_owned_instructions_replace_the_generated_ones_and_keep_the_targets_own(tmp_path):
    """A generated instruction file the release stops naming stops being loaded.

    The list used to be additive, so a file removed from the loaded set stayed in
    every target's list for ever and kept costing that session its tokens.
    """
    (tmp_path / "repo").mkdir()
    home = fake_home(tmp_path)
    shared = str(home / ".config/agents/AGENTS.md")
    superseded = str(home / ".config/agents/t3-steward.md")
    targets_own = str(home / "notes/mine.md")
    opencode = home / ".config/opencode/opencode.json"
    opencode.write_text(
        json.dumps({"mcp": {"huyang": {"command": ["huyang"]}}, "instructions": [shared]})
    )
    component = capture_environment(home, PROVENANCE)
    # The target still loads the reference file the release has dropped, and one
    # file of its own from outside the fleet-owned directory.
    opencode.write_text(
        json.dumps(
            {
                "mcp": {"huyang": {"command": ["huyang"]}},
                "instructions": [superseded, targets_own],
            }
        )
    )
    apply_environment(component, home, "instructions")
    assert json.loads(opencode.read_text())["instructions"] == [shared, targets_own]


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


def test_capture_preserves_os_owned_skill_symlinks(tmp_path):
    home, _ = environment(tmp_path)
    system_skill = tmp_path / "system-skill"
    system_skill.mkdir()
    (system_skill / "SKILL.md").write_text("# OS-owned skill")
    (home / ".claude/skills/os-owned").symlink_to(system_skill, target_is_directory=True)
    component = capture_environment(home, PROVENANCE)
    assert not any("os-owned" in f["path"] for f in component["files"])
    apply_environment(component, home, "preserve-os")
    assert (home / ".claude/skills/os-owned").is_symlink()
    assert (system_skill / "SKILL.md").read_text() == "# OS-owned skill"




def test_capture_skips_symlinked_repository_skill_directories(tmp_path):
    home, _ = environment(tmp_path)
    source = tmp_path / "source-skills"
    source.mkdir()
    external = tmp_path / "external-skill"
    external.mkdir()
    (external / "SKILL.md").write_text("# External skill\n")
    (source / "linked").symlink_to(external, target_is_directory=True)

    component = capture_environment(home, PROVENANCE, skills_root=source)
    assert not any("/linked/" in f["path"] for f in component["files"])


def test_retired_owned_file_is_removed_transactionally_and_tombstone_persists(tmp_path):
    home, _ = environment(tmp_path)
    skill = home / ".claude/skills/retired/SKILL.md"
    skill.parent.mkdir()
    skill.write_text("# Retired skill\n")
    previous = capture_environment(home, PROVENANCE)
    skill.unlink()
    skill.parent.rmdir()

    component = capture_environment(home, PROVENANCE, previous=previous)
    assert [f["path"] for f in component["retired_files"]] == [
        ".claude/skills/retired/SKILL.md"
    ]
    later = capture_environment(home, PROVENANCE, previous=component)
    assert later["retired_files"] == component["retired_files"]

    skill.parent.mkdir()
    skill.write_text("# Retired skill\n")
    dry = apply_environment(component, home, "retire-dry", dry_run=True)
    assert dry["drift"][-1]["subject"] == "retired:.claude/skills/retired/SKILL.md"
    assert dry["removed"] == [".claude/skills/retired/SKILL.md"]
    assert skill.exists()

    def fail_after_removal(path):
        if path == ".claude/skills/retired/SKILL.md":
            raise OSError("injected retirement failure")

    rolled_back = apply_environment(component, home, "retire-rollback", fault=fail_after_removal)
    assert rolled_back["state"] == "rolled-back"
    assert skill.read_text() == "# Retired skill\n"
    applied = apply_environment(component, home, "retire-success")
    assert applied["removed"] == [".claude/skills/retired/SKILL.md"]
    assert not skill.exists()

    personal = home / ".config/personal-retirement-target"
    personal.write_text("# Retired skill\n")
    skill.symlink_to(personal)
    with pytest.raises(ValueError, match="symlink"):
        apply_environment(component, home, "retire-symlink")
    assert personal.read_text() == "# Retired skill\n"
    skill.unlink()

    os.mkfifo(skill, 0o644)
    with pytest.raises(ValueError, match="unexplained changes"):
        apply_environment(component, home, "retire-fifo")
    skill.unlink()

    skill.write_text("# Retired skill\n")
    skill.chmod(0o600)
    with pytest.raises(ValueError, match="unexplained changes"):
        apply_environment(component, home, "retire-mode")
    skill.write_text("locally modified\n")
    skill.chmod(0o644)
    with pytest.raises(ValueError, match="unexplained changes"):
        apply_environment(component, home, "retire-refuse")


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


def test_setup_drift_does_not_reclaim_environment_owned_files(tmp_path):
    ns = remote_namespace(tmp_path)
    ns["COMPONENTS"] = {"agent-environment": {}}
    checkout = tmp_path / "setup"
    for path in ("config/claude/CLAUDE.md", "config/claude/skills/example/SKILL.md",
                 "config/bin/helper"):
        file = checkout / path
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text("source")
    pairs = ns["managed_file_pairs"](checkout)
    assert [str(source.relative_to(checkout)) for source, _ in pairs] == ["config/bin/helper"]
    ns["COMPONENTS"] = {}
    assert len(ns["managed_file_pairs"](checkout)) == 3


def test_pair_dry_run_does_not_require_future_steward_already_installed(tmp_path):
    ns = remote_namespace(tmp_path)
    ns["DRY_RUN"] = True
    ns["verify_pair_binary"] = lambda _: pytest.fail("future binary is not installed in dry run")
    ns["seed_t3_service"] = lambda *args: pytest.fail("an active T3 service is never seeded")
    assert ns["apply_t3"](
        {"version": "0.0.38"},
        {"installed": "0.0.38", "served": "0.0.38", "service_active": True},
        [],
    ) == "converged"


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


def test_openviking_base_url_is_public_but_not_its_credentials():
    def registration(url):
        return {
            "ov-memory": {
                "command": "/home/user/.local/bin/ov-memory-mcp",
                "env": {"OV_AGENT": "claude-code", "OV_BASE_URL": url},
            }
        }

    check_public(registration("https://ov.example.test"))
    with pytest.raises(ValueError, match="credential-bearing registration value"):
        check_public(registration("https://user:secret@ov.example.test"))
