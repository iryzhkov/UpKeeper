import json
import subprocess
from pathlib import Path

import pytest

from dev_fleet.release import (
    DEFAULT_MANIFEST,
    CaptureError,
    capture_release,
    load_release,
    validate_release,
    write_release,
)
from dev_fleet.validation import ValidationError

COMMIT_A = "a" * 40
COMMIT_B = "b" * 40


class FakeMachine:
    """Answers the git and version questions capture asks, without a real machine."""

    def __init__(self, *, dirty: set[str] | None = None, steward="0.11.0-rc.1", branches=None):
        self.dirty = dirty or set()
        self.steward = steward
        self.branches = branches or {"origin/main", "origin/feature/huyang"}
        self.calls: list[list[str]] = []

    def __call__(self, argv, cwd, timeout):
        self.calls.append(argv)
        if argv[0] == "git":
            checkout = argv[2]
            command = argv[3:]
            if command == ["status", "--porcelain"]:
                return _done(" M file\n" if Path(checkout).name in self.dirty else "")
            if command == ["rev-parse", "HEAD"]:
                return _done((COMMIT_A if Path(checkout).name == "huyang" else COMMIT_B) + "\n")
            if command[:3] == ["fetch", "--quiet", "origin"]:
                return _done("")
            if command[:2] == ["merge-base", "--is-ancestor"]:
                ref = next(a for a in self.calls[-2] if a in ("main", "feature/huyang"))
                return subprocess.CompletedProcess(
                    [], 0 if f"origin/{ref}" in self.branches else 1, stdout="", stderr=""
                )
        if argv[0] == "npm":
            return _done('{"dependencies":{"t3":{"version":"0.0.38"}}}')
        if argv[-1] == "version":
            return _done(f"t3-steward {self.steward} (tested with T3 0.0.38..0.0.38)\n")
        raise AssertionError(f"unexpected command {argv}")


def _done(stdout: str) -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess([], 0, stdout=stdout, stderr="")


def fake_home(tmp_path: Path) -> Path:
    home = tmp_path / "home"
    for checkout in (".local/share/huyang", ".local/share/omarchy-setup"):
        (home / checkout / ".git").mkdir(parents=True)
    (home / ".codex").mkdir()
    (home / ".codex/AGENTS.md").write_text("# Global agent instructions\n", encoding="utf-8")
    from dev_fleet.release_assets import CLAUDE_BEGIN, CLAUDE_END

    files = {
        ".claude/omarchy-setup/CLAUDE.md": "# Shared rules\n",
        ".claude/CLAUDE.md": CLAUDE_BEGIN + "\n@~/.claude/omarchy-setup/CLAUDE.md\n" + CLAUDE_END,
        ".claude/settings.json": '{"model":"test","permissions":{"allow":[]}}',
        ".claude.json": (
            '{"mcpServers":{"huyang":{"command":"huyang"}},"oauthAccount":{"token":"not-captured"}}'
        ),
        ".codex/config.toml": '[mcp_servers.huyang]\ncommand = "huyang"\n',
        ".config/opencode/opencode.json": (
            '{"mcp":{"huyang":{"command":["huyang"]}},"instructions":[]}'
        ),
        ".config/agents/huyang.md": "# Huyang\n",
        ".claude/skills/sample/SKILL.md": "# Sample\n",
    }
    for path, text in files.items():
        target = home / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text)
    (tmp_path / "repo/README.md").write_text("# dev-fleet\n")
    return home


def test_capture_reads_the_controller_and_round_trips(tmp_path: Path) -> None:
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)
    machine = FakeMachine()

    release = capture_release(root, controller="normandy", home=home, runner=machine)
    write_release(root, release)

    components = release.components
    assert components["huyang"]["commit"] == COMMIT_A
    assert components["omarchy-setup"]["commit"] == COMMIT_B
    assert components["t3-steward"]["version"] == "0.11.0-rc.1"
    import base64

    assets = components["agent-environment"]["files"]
    stored = next(f for f in assets if f["path"] == ".codex/AGENTS.md")
    assert base64.b64decode(stored["content"]) == b"# Global agent instructions\n"
    assert components["agent-config"]["files"] == []
    assert components["worker-configuration"]["hosts"] == {}
    assert components["t3"]["version"] == "0.0.38"
    assert "not-captured" not in release.canonical_bytes().decode()
    assert load_release(root).manifest == release.manifest
    assert json.loads((root / "desired/release.json").read_text())["captured_from"] == "normandy"


def test_capture_refuses_dirty_checkouts_and_local_builds(tmp_path: Path) -> None:
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)

    with pytest.raises(CaptureError, match="uncommitted"):
        capture_release(
            root, controller="normandy", home=home, runner=FakeMachine(dirty={"huyang"})
        )
    with pytest.raises(CaptureError, match="not a published release"):
        capture_release(
            root,
            controller="normandy",
            home=home,
            runner=FakeMachine(steward="v0.10.1-117-gdd438c5-dirty"),
        )
    with pytest.raises(CaptureError, match="not on origin/main"):
        capture_release(
            root,
            controller="normandy",
            home=home,
            runner=FakeMachine(branches={"origin/feature/huyang"}),
        )


def test_manifest_rejects_values_that_could_reach_a_shell() -> None:
    manifest = json.loads(json.dumps(DEFAULT_MANIFEST))
    manifest.update({"captured_at": "2026-09-12T00:00:00Z", "captured_from": "normandy"})
    components = manifest["components"]
    components["huyang"]["commit"] = COMMIT_A
    components["omarchy-setup"]["commit"] = COMMIT_B
    components["t3-steward"]["version"] = "0.11.0-rc.1"
    components["agent-config"]["files"][0]["sha256"] = "0" * 64
    validate_release(manifest)

    bad_ref = json.loads(json.dumps(manifest))
    bad_ref["components"]["huyang"]["ref"] = "main; rm -rf ~"
    with pytest.raises(ValidationError):
        validate_release(bad_ref)

    bad_path = json.loads(json.dumps(manifest))
    bad_path["components"]["agent-config"]["files"][0]["path"] = "../etc/passwd"
    with pytest.raises(ValidationError):
        validate_release(bad_path)

    bad_module = json.loads(json.dumps(manifest))
    bad_module["components"]["omarchy-setup"]["modules"] = ["45-agents; true"]
    with pytest.raises(ValidationError):
        validate_release(bad_module)

    short_commit = json.loads(json.dumps(manifest))
    short_commit["components"]["huyang"]["commit"] = "abc123"
    with pytest.raises(ValidationError):
        validate_release(short_commit)
