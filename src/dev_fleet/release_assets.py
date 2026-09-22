"""Owned file projections and F05 environment transactions for release pull.

Only this module knows how shared harness files are projected. Secret values are
never captured; worker credentials remain target-local opaque references.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import re
import stat
import tomllib
from dataclasses import asdict
from pathlib import Path
from typing import Any

from .environment import (
    AgentEnvironmentSpec,
    BehavioralProbe,
    ManagedAsset,
    PriorAsset,
    ProjectInstructionRef,
    RecoverySource,
    classify_asset_drift,
    environment_from_dict,
    environment_to_dict,
)

CLAUDE_BEGIN = "<!-- >>> omarchy-setup:claude >>> -->"
CLAUDE_END = "<!-- <<< omarchy-setup:claude <<< -->"
SECRET_RE = re.compile(r"(password|token|credential|api.?key|authorization|secret)", re.I)
SAFE_ENV = {"OV_AGENT", "LOCAL_LLM_MODEL", "LOCAL_LLM_NUM_CTX", "LOCAL_LLM_URL"}
ROOTS = {".config", ".local", ".claude", ".codex", ".agents"}
SETTINGS_KEYS = ("model", "theme", "tui", "syntaxHighlightingDisabled", "permissions")
REF_RE = re.compile(r"secretref:[A-Za-z0-9][A-Za-z0-9._/-]*")
MODES = {"file", "claude-block", "claude-settings", "claude-mcp", "codex-mcp", "opencode"}

# The directory whose instruction files the release owns. It is matched as a path
# fragment rather than against one home directory, because the targets do not
# share a user name.
MANAGED_INSTRUCTION_DIRECTORY = "/.config/agents/"


def digest(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def safe_path(value: str) -> str:
    p = Path(value)
    if value == ".claude.json":
        return value
    if (
        p.is_absolute()
        or not p.parts
        or p.parts[0] not in ROOTS
        or ".." in p.parts
        or any(ord(c) < 32 for c in value)
    ):
        raise ValueError("unsafe managed home path")
    return value


def check_public(value, key=""):
    """Reject credential-bearing registration fields instead of redacting a release."""
    if SECRET_RE.search(key):
        raise ValueError("credential-bearing configuration must use a secret reference")
    if isinstance(value, dict):
        for k, v in value.items():
            if k in {"env", "environment"} and (not isinstance(v, dict) or set(v) - SAFE_ENV):
                raise ValueError(
                    "unapproved MCP environment key; use a target-local secret reference"
                )
            check_public(v, k)
    elif isinstance(value, list):
        for item in value:
            if isinstance(item, str) and item.startswith("-") and SECRET_RE.search(item):
                raise ValueError("credential-bearing command arguments are forbidden")
            check_public(item)
    elif isinstance(value, str):
        if re.search(r"(?:https?://[^/\s]+@|[?&](?:key|token)=|(?:Bearer|sk-)\S+)", value):
            raise ValueError("credential-bearing registration value")


def canonical(value) -> bytes:
    return (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()


def capture_environment(
    home: Path,
    provenance: ProjectInstructionRef,
    previous: dict | None = None,
    skills_root: Path | None = None,
) -> dict:
    files = []

    def add(path, mode="file", value=None, kind="global-instruction", permissions=None):
        path = safe_path(path)
        source = home / path
        data = source.read_bytes() if value is None else value
        files.append(
            {
                "path": path,
                "mode": mode,
                "kind": kind,
                "content": base64.b64encode(data).decode(),
                "sha256": digest(data),
                "size_bytes": len(data),
                "permissions": (
                    permissions
                    if permissions is not None
                    else stat.S_IMODE(source.stat().st_mode)
                    if mode == "file"
                    else 0o600
                ),
            }
        )

    add(".codex/AGENTS.md")
    add(".claude/omarchy-setup/CLAUDE.md")
    text = (home / ".claude/CLAUDE.md").read_text()
    # Capture the managed import, never the user's surrounding instructions.
    if text.count(CLAUDE_BEGIN) != 1 or text.count(CLAUDE_END) != 1:
        raise ValueError("Claude managed block is missing or ambiguous")
    block = text.split(CLAUDE_BEGIN, 1)[1].split(CLAUDE_END, 1)[0]
    add(".claude/CLAUDE.md", "claude-block", block.encode())
    for p in sorted((home / ".config/agents").glob("*.md")):
        add(str(p.relative_to(home)))
    # Resolve published skill directory symlinks, but freeze bytes, never links.
    for relative_root in (".claude/skills", ".codex/skills", ".agents/skills"):
        root = home / relative_root
        if not root.exists():
            continue
        for directory in sorted(root.iterdir()):
            if (
                directory.name == ".system"
                or not directory.is_dir()
                or not directory.resolve().is_relative_to(home.resolve())
            ):
                # OS-supplied skill links remain package-owned. Never turn
                # their installed bytes into a writable target.
                continue
            for p in sorted(directory.rglob("*")):
                if p.is_file() and ".git" not in p.parts and "__pycache__" not in p.parts:
                    add(str(p.relative_to(home)), kind="skill")

    if previous is not None:
        validate_environment(previous)
    source_names = set()
    if skills_root is not None:
        for directory in sorted(skills_root.iterdir()):
            if directory.is_symlink() or not directory.is_dir() or directory.name == ".system":
                continue
            entrypoint = directory / "SKILL.md"
            if not entrypoint.is_file() or entrypoint.is_symlink():
                raise ValueError(f"repository skill {directory.name} has no regular SKILL.md")
            source_names.add(directory.name)

    def replaced_source_skill(f):
        parts = Path(f["path"]).parts
        return (
            len(parts) >= 3
            and parts[0] in {".claude", ".agents", ".codex"}
            and parts[1] == "skills"
            and parts[2] in source_names
        )

    files[:] = [f for f in files if not replaced_source_skill(f)]
    if skills_root is not None:
        for name in sorted(source_names):
            directory = skills_root / name
            for p in sorted(directory.rglob("*")):
                if p.is_symlink():
                    raise ValueError(f"repository skill {name} contains a symlink")
                if not p.is_file():
                    continue
                relative = p.relative_to(directory)
                data = p.read_bytes()
                permissions = stat.S_IMODE(p.stat().st_mode)
                if permissions not in {0o600, 0o644, 0o700, 0o755}:
                    raise ValueError(f"repository skill {name} contains an unsupported file")
                for root in (".claude/skills", ".agents/skills"):
                    add(
                        str(Path(root) / name / relative),
                        value=data,
                        kind="skill",
                        permissions=permissions,
                    )
    settings = json.loads((home / ".claude/settings.json").read_text())
    owned = {k: settings[k] for k in SETTINGS_KEYS if k in settings and k != "permissions"}
    owned["permissions"] = {"allow": settings.get("permissions", {}).get("allow", [])}
    check_public(owned)
    add(".claude/settings.json", "claude-settings", canonical(owned), "harness-setting")
    claude = json.loads((home / ".claude.json").read_text())["mcpServers"]
    codex = tomllib.loads((home / ".codex/config.toml").read_text())["mcp_servers"]
    opencode = json.loads((home / ".config/opencode/opencode.json").read_text())
    for path, mode, data in (
        (".claude.json", "claude-mcp", claude),
        (".codex/config.toml", "codex-mcp", codex),
        (
            ".config/opencode/opencode.json",
            "opencode",
            {"mcp": opencode["mcp"], "instructions": opencode["instructions"]},
        ),
    ):
        check_public(data)
        add(path, mode, canonical(data), "mcp-client")
    files.sort(key=lambda f: f["path"])
    active_paths = {f["path"] for f in files}
    retired_by_path = {}
    if previous is not None:
        for f in previous.get("retired_files", []):
            retired_by_path[f["path"]] = dict(f)
        for f in previous["files"]:
            if f["mode"] == "file" and f["path"] not in active_paths:
                retired_by_path[f["path"]] = dict(f)
    for path in active_paths:
        retired_by_path.pop(path, None)
    retired_files = [retired_by_path[path] for path in sorted(retired_by_path)]
    desired = {f["path"]: base64.b64decode(f["content"]) for f in files}
    spec = make_spec(home, desired, desired, files, provenance, "capture")
    result = {
        "kind": "agent-environment",
        "files": files,
        "retired_files": retired_files,
        "spec": environment_to_dict(spec),
        "source_home": str(home),
    }
    validate_environment(result)
    return result


def validate_environment(component):
    fields = set(component)
    if fields not in (
        {"kind", "files", "spec", "source_home"},
        {"kind", "files", "retired_files", "spec", "source_home"},
    ):
        raise ValueError("unknown agent-environment fields")
    spec = environment_from_dict(component["spec"])
    if component["kind"] != "agent-environment" or not Path(component["source_home"]).is_absolute():
        raise ValueError("invalid agent-environment identity")
    files = component["files"]
    paths = [safe_path(f["path"]) for f in files]
    if not paths or paths != sorted(set(paths)) or len(files) != len(spec.assets):
        raise ValueError("environment files must be unique, sorted and cover the F05 spec")
    for f, asset in zip(files, spec.assets, strict=True):
        if (
            set(f) != {"path", "mode", "kind", "content", "sha256", "size_bytes", "permissions"}
            or f["mode"] not in MODES
        ):
            raise ValueError("invalid owned file descriptor")
        if f["permissions"] not in {0o600, 0o644, 0o700, 0o755}:
            raise ValueError("unsupported managed file permissions")
        data = base64.b64decode(f["content"], validate=True)
        if digest(data) != f["sha256"] or len(data) != f["size_bytes"]:
            raise ValueError("environment payload digest mismatch")
        if (
            asset.sha256 != f["sha256"]
            or asset.size_bytes != f["size_bytes"]
            or asset.kind != f["kind"]
            or asset.active_path != str(Path(component["source_home"]) / f["path"])
        ):
            raise ValueError("environment file does not match F05 asset")
        if f["mode"] in {"claude-settings", "claude-mcp", "codex-mcp", "opencode"}:
            check_public(json.loads(data))
        expected = {
            ".claude.json": "claude-mcp",
            ".codex/config.toml": "codex-mcp",
            ".claude/settings.json": "claude-settings",
            ".config/opencode/opencode.json": "opencode",
            ".claude/CLAUDE.md": "claude-block",
        }
        if f["mode"] != expected.get(f["path"], "file"):
            raise ValueError("owned projection does not match its allowed destination")
    retired = component.get("retired_files", [])
    retired_paths = [safe_path(f["path"]) for f in retired]
    if retired_paths != sorted(set(retired_paths)) or set(paths) & set(retired_paths):
        raise ValueError("retired environment files must be unique, sorted and inactive")
    for f in retired:
        if (
            set(f) != {"path", "mode", "kind", "content", "sha256", "size_bytes", "permissions"}
            or f["mode"] != "file"
            or f["permissions"] not in {0o600, 0o644, 0o700, 0o755}
        ):
            raise ValueError("invalid retired file descriptor")
        data = base64.b64decode(f["content"], validate=True)
        if digest(data) != f["sha256"] or len(data) != f["size_bytes"]:
            raise ValueError("retired environment payload digest mismatch")


def make_spec(home, desired, prior, files, provenance, identity):
    assets, previous = [], []
    for i, f in enumerate(files):
        p = f["path"]
        active = str(home / p)
        data, old = desired[p], prior.get(p) or b""
        asset_id = f"asset-{i:06d}"
        assets.append(
            ManagedAsset(
                asset_id,
                f["kind"],
                provenance.repository,
                provenance.repository_revision,
                digest(data),
                len(data),
                str(home / ".local/state/dev-fleet/environments" / identity / asset_id),
                active,
                i,
            )
        )
        previous.append(PriorAsset(asset_id, digest(old), len(old), active))
    baseline = digest(canonical([asdict(p) for p in previous]))
    return AgentEnvironmentSpec(
        1,
        "local-single-worker-v1",
        "release-host",
        ("release-host",),
        ("install", "activate", "probe", "rollback"),
        tuple(assets),
        (provenance,),
        (
            BehavioralProbe(
                "owned-projection",
                ("/usr/bin/python3", "-"),
                30,
                (
                    "explicit-effect-authority",
                    "instruction-safety",
                    "preserve-unexplained-changes",
                    "skill-safety",
                ),
            ),
        ),
        tuple(
            RecoverySource(k, owner, str(home / p))
            for k, owner, p in (
                ("huyang-journal", "Huyang", ".local/state/huyang"),
                ("lease", "t3-steward", ".local/state/t3-steward"),
                ("t3-thread", "T3", ".local/share/t3"),
                ("worktree", "Git", "Work"),
            )
        ),
        (),
        baseline,
        tuple(previous),
    )


def _toml_value(value):
    if isinstance(value, str):
        return json.dumps(value)
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (int, float)):
        return str(value)
    if isinstance(value, list):
        return "[" + ", ".join(_toml_value(v) for v in value) + "]"
    if isinstance(value, dict):
        return (
            "{ "
            + ", ".join(json.dumps(k) + " = " + _toml_value(v) for k, v in value.items())
            + " }"
        )
    raise ValueError("unsupported MCP TOML value")


def merge_registrations(current, owned):
    result = dict(current)
    for name, values in owned.items():
        previous = current.get(name, {})
        merged = {**previous, **values}
        for key in ("env", "environment"):
            if key in previous or key in values:
                merged[key] = {**previous.get(key, {}), **values.get(key, {})}
        result[name] = merged
    return result


def render(f, old: bytes | None, source_home: str, home: Path) -> bytes:
    data = base64.b64decode(f["content"], validate=True)
    if f["mode"] == "file":
        return data
    text = (old or b"").decode()
    if f["mode"] == "claude-block":
        if text.count(CLAUDE_BEGIN) != text.count(CLAUDE_END) or text.count(CLAUDE_BEGIN) > 1:
            raise ValueError("ambiguous Claude managed block")
        block = CLAUDE_BEGIN + data.decode() + CLAUDE_END
        if CLAUDE_BEGIN in text:
            start, end = text.index(CLAUDE_BEGIN), text.index(CLAUDE_END)
            if end < start:
                raise ValueError("reversed Claude managed block")
            return (text[:start] + block + text[end + len(CLAUDE_END) :]).encode()
        return (text.rstrip() + "\n\n" + block + "\n").encode()

    # Rebase only the controller home prefix in structured registration values.
    def rebase(v) -> Any:
        if isinstance(v, str):
            return v.replace(source_home + "/", str(home) + "/")
        if isinstance(v, list):
            return [rebase(x) for x in v]
        if isinstance(v, dict):
            return {k: rebase(x) for k, x in v.items()}
        return v

    owned = rebase(json.loads(data))
    if f["mode"] == "codex-mcp":
        current = tomllib.loads(text)
        current_mcp = current.get("mcp_servers", {})
        merged_mcp = merge_registrations(current_mcp, owned)
        owned = {k: merged_mcp[k] for k in owned}
        if current_mcp == merged_mcp:
            return old or b""
        # Preserve every unrelated table byte-for-byte, including projects and providers.
        lines, kept, skipping = text.splitlines(keepends=True), [], False
        for line in lines:
            if re.match(r"^\s*\[", line):
                header = re.match(
                    r"^\s*\[\s*mcp_servers\.(?:\"([^\"]+)\"|([A-Za-z0-9_-]+))(?:\.|\s*\])", line
                )
                skipping = bool(header and (header[1] or header[2]) in owned)
            if not skipping:
                kept.append(line)
        rendered = "".join(kept).rstrip() + "\n"
        for name, entry in owned.items():
            rendered += "\n[mcp_servers." + json.dumps(name) + "]\n"
            rendered += "".join(
                json.dumps(k) + " = " + _toml_value(v) + "\n" for k, v in entry.items()
            )
        parsed = tomllib.loads(rendered)
        if parsed.get("mcp_servers") != {**current_mcp, **owned}:
            raise ValueError("Codex TOML layout cannot be safely projected")
        return rendered.encode()
    current = json.loads(text or "{}")
    if not isinstance(current, dict):
        raise ValueError("harness settings must be an object")
    if f["mode"] == "claude-settings":
        allow = sorted(
            set(
                current.get("permissions", {}).get("allow", [])
                + owned.get("permissions", {}).get("allow", [])
            )
        )
        for k, v in owned.items():
            if k != "permissions":
                current[k] = v
        current.setdefault("permissions", {})["allow"] = allow
    elif f["mode"] == "claude-mcp":
        current["mcpServers"] = merge_registrations(current.get("mcpServers", {}), owned)
    elif f["mode"] == "opencode":
        current["mcp"] = merge_registrations(current.get("mcp", {}), owned["mcp"])
        # The shared instruction directory is fleet-owned, so the release decides
        # which of its files OpenCode loads. An entry of the target's own is kept:
        # only a generated one the release no longer names is dropped, which is how
        # a file that left the loaded set stops being loaded everywhere.
        kept = [
            entry
            for entry in current.get("instructions", [])
            if not (isinstance(entry, str) and MANAGED_INSTRUCTION_DIRECTORY in entry)
        ]
        current["instructions"] = list(dict.fromkeys(owned["instructions"] + kept))
    # Harnesses rewrite their own account/cache state and formatting. Those
    # bytes are not owned drift when all projected keys already agree.
    if old is not None and json.loads(text) == current:
        return old
    return canonical(current)


def target_path(home: Path, relative: str) -> Path:
    target = home / safe_path(relative)
    resolved = target.resolve()
    if not resolved.is_relative_to(home.resolve()):
        raise ValueError("managed target symlink escapes home")
    return resolved


def retirement_path(home: Path, relative: str) -> Path:
    target = home / safe_path(relative)
    current = home
    for part in Path(relative).parts:
        current /= part
        if current.is_symlink():
            raise ValueError("retired managed path traverses a symlink")
        if not current.exists():
            break
    return target


def observe_environment(component, home, identity="pending"):
    validate_environment(component)
    desired, prior = {}, {}
    for f in component["files"]:
        path = target_path(home, f["path"])
        prior[f["path"]] = path.read_bytes() if path.exists() else None
        desired[f["path"]] = render(f, prior[f["path"]], component["source_home"], home)
    for f in component.get("retired_files", []):
        path = retirement_path(home, f["path"])
        old = None
        if path.exists():
            if not path.is_file() or stat.S_IMODE(path.stat().st_mode) != f["permissions"]:
                raise ValueError("retired managed file has unexplained changes: " + f["path"])
            old = path.read_bytes()
            if digest(old) != f["sha256"]:
                raise ValueError("retired managed file has unexplained changes: " + f["path"])
        prior[f["path"]] = old
        desired[f["path"]] = None
    provenance = environment_from_dict(component["spec"]).project_instructions[0]
    spec = make_spec(home, desired, prior, component["files"], provenance, identity)
    observed = {
        a.asset_id: (
            digest(prior[f["path"]]) if prior[f["path"]] is not None else None,
            "owned projection",
        )
        for a, f in zip(spec.assets, component["files"], strict=True)
    }
    drift = [asdict(x) for x in classify_asset_drift(spec, observed)]
    for f in component.get("retired_files", []):
        if prior[f["path"]] is None:
            continue
        drift.append(
            {
                "subject": "retired:" + f["path"],
                "state": "MISMATCH",
                "expected_sha256": None,
                "observed_sha256": digest(prior[f["path"]]),
                "evidence": "retired owned projection",
            }
        )
    return spec, desired, prior, drift


def atomic_write(path, data, mode):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".upkeeper-tmp")
    # No following an attacker-created temporary symlink.
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    try:
        with os.fdopen(fd, "wb") as out:
            os.fchmod(out.fileno(), mode)
            out.write(data)
            out.flush()
            os.fsync(out.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def apply_environment(component, home, run_id, dry_run=False, fault=None):
    spec, desired, prior, drift = observe_environment(component, home, run_id)
    desired_modes = {f["path"]: f["permissions"] for f in component["files"]}
    changed = [
        f["path"]
        for f in component["files"]
        if desired[f["path"]] != prior[f["path"]]
        or stat.S_IMODE(target_path(home, f["path"]).stat().st_mode) != f["permissions"]
    ]
    removed = [
        f["path"]
        for f in component.get("retired_files", [])
        if prior[f["path"]] is not None
    ]
    mutations = changed + removed
    if not mutations or dry_run:
        return {
            "state": "updated" if mutations else "converged",
            "changed": changed,
            "removed": removed,
            "drift": drift,
        }
    root = home / ".local/state/dev-fleet/environments" / run_id
    root.mkdir(parents=True, exist_ok=False, mode=0o700)
    descriptors = component["files"] + component.get("retired_files", [])
    retired_modes = {
        f["path"]: f["permissions"] for f in component.get("retired_files", [])
    }
    retired_paths = set(retired_modes)
    paths = {
        f["path"]: (
            retirement_path(home, f["path"])
            if f["path"] in retired_paths
            else target_path(home, f["path"])
        )
        for f in descriptors
    }
    if len(set(paths.values())) != len(paths):
        raise ValueError("managed paths alias one another")
    modes = {
        p: stat.S_IMODE(path.stat().st_mode) if path.exists() else 0o600
        for p, path in paths.items()
    }
    # Freeze the complete F05 prior identity and bytes before the first activation.
    atomic_write(root / "spec.json", canonical(environment_to_dict(spec)), 0o600)
    atomic_write(
        root / "prior.json",
        canonical(
            {
                p: {
                    "content": base64.b64encode(v).decode() if v is not None else None,
                    "mode": modes[p],
                }
                for p, v in prior.items()
            }
        ),
        0o600,
    )
    for i, f in enumerate(component["files"]):
        atomic_write(root / f"asset-{i:06d}", desired[f["path"]], 0o600)
    activated = []
    try:
        for p in mutations:
            path = paths[p]
            if desired[p] is None:
                fresh = retirement_path(home, p)
                if fresh != path:
                    raise ValueError("retired managed path changed since observation")
                if (
                    not fresh.exists()
                    or not fresh.is_file()
                    or stat.S_IMODE(fresh.stat().st_mode) != retired_modes[p]
                    or fresh.read_bytes() != prior[p]
                ):
                    raise ValueError("retired managed file changed since observation")
                fresh.unlink()
            else:
                actual = path.read_bytes() if path.exists() else None
                if actual != prior[p]:
                    raise ValueError("managed file changed since observation")
                atomic_write(path, desired[p], desired_modes[p])
            activated.append(p)
            if fault:
                fault(p)
        for a, f in zip(spec.assets, component["files"], strict=True):
            if (
                digest(paths[f["path"]].read_bytes()) != a.sha256
                or stat.S_IMODE(paths[f["path"]].stat().st_mode) != desired_modes[f["path"]]
            ):
                raise ValueError("F05 active asset failed verification")
        if any(paths[p].exists() for p in removed):
            raise ValueError("retired managed file still exists")
    except Exception:
        for p in reversed(activated):
            # Preserve concurrent/unexplained changes; do not claim a clean rollback.
            current = paths[p].read_bytes() if paths[p].exists() else None
            if current != desired[p]:
                raise RuntimeError("rollback failed: unexplained managed-path change") from None
            if prior[p] is None:
                paths[p].unlink(missing_ok=True)
            else:
                atomic_write(paths[p], prior[p], modes[p])
        for p in paths:
            restored = paths[p].read_bytes() if paths[p].exists() else None
            if restored != prior[p]:
                raise RuntimeError("rollback failed: prior environment did not verify") from None
        return {
            "state": "rolled-back",
            "changed": changed,
            "removed": removed,
            "drift": drift,
        }
    return {"state": "updated", "changed": changed, "removed": removed, "drift": drift}
