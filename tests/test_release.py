import io
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

# An installed agents-instructions-gen that understands being asked a question,
# and the copy that predates the check and writes instead of reporting.
CURRENT_GENERATOR = (
    "#!/usr/bin/env bash\n# Usage: agents-instructions-gen [--root DIR] [--check]\nexit 0\n"
)
LEGACY_GENERATOR = '#!/usr/bin/env bash\nout="$HOME/.config/agents"\nmkdir -p "$out"\n'


class FakeMachine:
    """Answers the git and version questions capture asks, without a real machine."""

    def __init__(self, *, dirty: set[str] | None = None, steward="0.11.0-rc.1", branches=None):
        self.dirty = dirty or set()
        self.steward = steward
        self.branches = branches or {"origin/main", "origin/feature/huyang"}
        self.calls: list[list[str]] = []
        # What the installed agents-instructions-gen --check answers. The
        # default is a converged host, because that is the only state from
        # which a release may be captured.
        self.check_code = 0
        self.check_stdout = ""

    def __call__(self, argv, cwd, timeout):
        self.calls.append(argv)
        if any(part.endswith("agents-instructions-gen") for part in argv):
            return subprocess.CompletedProcess(
                [], self.check_code, stdout=self.check_stdout, stderr=""
            )
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
    # The command that decides whether this host still holds what its source
    # generates. Its contents never matter here: every command is answered by
    # FakeMachine, so nothing is executed. What matters is that the capture gate
    # can see an installed, executable check.
    # It also has to carry the evidence the gate looks for before it runs
    # anything: the copy that predates the check ignores its arguments and
    # rewrites the host's instruction files instead of reporting on them.
    generator = home / ".local/bin/agents-instructions-gen"
    generator.parent.mkdir(parents=True, exist_ok=True)
    generator.write_text(CURRENT_GENERATOR)
    generator.chmod(0o755)
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


def test_capture_of_selected_components_leaves_the_rest_as_published(tmp_path: Path) -> None:
    """A capture asked for one component must not carry another along.

    Capture reads the whole machine, so a controller whose local checkout of an
    unrelated component is ahead of the release proposes that newer commit every
    time. Without a way to scope the capture an operator has to notice and revert
    it by hand on every release.
    """
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)
    machine = FakeMachine()

    baseline = capture_release(root, controller="normandy", home=home, runner=machine)
    # The release published a huyang the controller is no longer running, which is
    # what happens once a checkout moves ahead of its pin.
    manifest = json.loads(json.dumps(baseline.manifest))
    older = "c" * 40
    manifest["components"]["huyang"]["commit"] = older
    write_release(root, validate_release(manifest))
    published = load_release(root).components

    everything = capture_release(root, controller="normandy", home=home, runner=machine)
    assert everything.components["huyang"]["commit"] == COMMIT_A

    scoped = capture_release(
        root, controller="normandy", home=home, runner=machine, only=["agent-environment"]
    )
    assert scoped.components["huyang"] == published["huyang"]
    assert scoped.components["huyang"]["commit"] == older
    assert scoped.components["omarchy-setup"] == published["omarchy-setup"]
    assert scoped.components["t3-steward"] == published["t3-steward"]
    assert scoped.components["agent-environment"] == everything.components["agent-environment"]


def test_capture_rejects_an_unknown_component_name(tmp_path: Path) -> None:
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)
    with pytest.raises(CaptureError, match="unknown component"):
        capture_release(
            root, controller="normandy", home=home, runner=FakeMachine(), only=["huyangg"]
        )


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


# A capture photographs the pushing host's home directory, and the resident
# agent instructions it finds there are published to the whole fleet as files
# that overwrite every other host. No repository installs them, so nothing after
# the capture can notice that this host had drifted. These tests hold the one
# place that can notice to refusing, in the Python reference implementation as
# well as in the Go one.

DIVERGED_REPORT = (
    "agents-instructions-gen: checking this host against /home/igor/.local/share/omarchy-setup\n"
    "  ~/.claude/skills/t3-backlog/SKILL.md: the host copy differs from "
    "config/claude/skills/t3-backlog/SKILL.md\n"
    "  ~/.claude/omarchy-setup/CLAUDE.md: the host copy differs from config/claude/CLAUDE.md\n"
    "agents-instructions-gen: this host has diverged.\n"
)


def test_a_converged_host_is_checked_before_it_is_captured(tmp_path: Path) -> None:
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)
    machine = FakeMachine()

    capture_release(root, controller="normandy", home=home, runner=machine)

    checks = [argv for argv in machine.calls if argv[-1] == "--check"]
    assert checks, f"the capture never asked whether this host matches its source: {machine.calls}"
    assert str(home / ".local/share/omarchy-setup") in checks[0]
    assert f"HOME={home}" in checks[0]


def test_a_diverged_host_is_refused_by_name_and_direction(tmp_path: Path) -> None:
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)
    machine = FakeMachine()
    machine.check_code = 3
    machine.check_stdout = DIVERGED_REPORT

    with pytest.raises(CaptureError) as refusal:
        capture_release(root, controller="normandy", home=home, runner=machine)

    message = str(refusal.value)
    assert "~/.claude/skills/t3-backlog/SKILL.md: the host copy differs from" in message
    assert "~/.claude/omarchy-setup/CLAUDE.md: the host copy differs from" in message
    assert "photographs this host's home directory" in message
    assert "--publish-diverged-instructions" in message
    assert not (root / "desired").exists()


def test_a_capture_that_cannot_be_checked_is_refused(tmp_path: Path) -> None:
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)
    (home / ".local/bin/agents-instructions-gen").unlink()
    machine = FakeMachine()

    with pytest.raises(CaptureError) as refusal:
        capture_release(root, controller="normandy", home=home, runner=machine)

    message = str(refusal.value)
    assert "cannot check whether this host's agent instructions match their source" in message
    assert "no executable" in message
    assert "have diverged from" not in message
    assert not (root / "desired").exists()


def test_a_generator_that_predates_the_check_is_never_run(tmp_path: Path) -> None:
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)
    (home / ".local/bin/agents-instructions-gen").write_text(LEGACY_GENERATOR)
    machine = FakeMachine()

    with pytest.raises(CaptureError) as refusal:
        capture_release(root, controller="normandy", home=home, runner=machine)

    assert "does not understand --check" in str(refusal.value)
    assert not [argv for argv in machine.calls if argv[-1] == "--check"]


def test_the_named_override_publishes_and_accounts_for_every_path(tmp_path: Path) -> None:
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)
    machine = FakeMachine()
    machine.check_code = 3
    machine.check_stdout = DIVERGED_REPORT
    notice = io.StringIO()

    capture_release(
        root,
        controller="normandy",
        home=home,
        runner=machine,
        publish_diverged_instructions=True,
        notice=notice,
    )

    printed = notice.getvalue()
    assert "--publish-diverged-instructions" in printed
    assert "~/.claude/skills/t3-backlog/SKILL.md" in printed
    assert "~/.claude/omarchy-setup/CLAUDE.md" in printed
    assert "every host in the fleet" in printed


def test_the_override_does_not_cover_a_check_that_could_not_run(tmp_path: Path) -> None:
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)
    (home / ".local/bin/agents-instructions-gen").unlink()
    machine = FakeMachine()
    notice = io.StringIO()

    with pytest.raises(CaptureError) as refusal:
        capture_release(
            root,
            controller="normandy",
            home=home,
            runner=machine,
            publish_diverged_instructions=True,
            notice=notice,
        )

    assert "no override for this case" in str(refusal.value)
    assert notice.getvalue() == ""


def test_a_selection_that_publishes_no_instructions_is_not_gated(tmp_path: Path) -> None:
    root = tmp_path / "repo"
    root.mkdir()
    home = fake_home(tmp_path)
    machine = FakeMachine()
    write_release(root, capture_release(root, controller="normandy", home=home, runner=machine))

    (home / ".local/bin/agents-instructions-gen").unlink()
    machine = FakeMachine()
    capture_release(
        root, controller="normandy", home=home, runner=machine, only=["t3-steward"]
    )

    assert not [argv for argv in machine.calls if argv[-1] == "--check"]
