"""The release manifest: what `upkeeper push` captures and `upkeeper pull` applies.

A release is the exact state of the controller's own machine, taken from what is
running there rather than typed by hand: the Huyang checkout commit, the
omarchy-setup checkout commit, the installed t3-steward release, and the
agent instruction files that no other mechanism distributes. Every value is
validated on the way in so that nothing in the manifest can become a shell
fragment on a target.
"""

from __future__ import annotations

import hashlib
import json
import re
import subprocess
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from .environment import ProjectInstructionRef
from .release_assets import capture_environment, validate_environment
from .validation import (
    ValidationError,
    validate_git_commit,
    validate_home_relative_path,
    validate_host_name,
    validate_source_url,
)
from .worker_release import validate_workers

RELEASE_SCHEMA_VERSION = 1
RELEASE_FILE = "desired/release.json"
AGENT_CONFIG_DIR = "desired/agent-config"

# Home directories a release may touch. Wider than the components manifest
# because the harness instruction files live in ~/.claude and ~/.codex.
RELEASE_HOME_ROOTS = frozenset({".config", ".local", ".claude", ".codex"})

REF_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$")
VERSION_RE = re.compile(r"^\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?$")
LOCAL_BUILD_RE = re.compile(r"-\d+-g[0-9a-f]{7,}|dirty")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
MODULE_RE = re.compile(r"^\d{2}-[a-z][a-z0-9-]*$")
GITHUB_REPOSITORY_RE = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")
STORE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9][A-Za-z0-9._-]*)*$")

LEGACY_COMPONENT_ORDER = ("omarchy-setup", "agent-config", "huyang", "t3-steward")
COMPONENT_ORDER = (
    "omarchy-setup",
    "agent-config",
    "agent-environment",
    "huyang",
    "t3-steward",
    "t3",
    "worker-configuration",
)

# The shape of a fresh manifest. Values that push captures are None here.
DEFAULT_MANIFEST: dict[str, Any] = {
    "schema_version": RELEASE_SCHEMA_VERSION,
    "captured_at": None,
    "captured_from": None,
    "components": {
        "omarchy-setup": {
            "kind": "git-checkout",
            "source": "https://github.com/iryzhkov/omarchy-setup.git",
            "ref": "main",
            "commit": None,
            "path": ".local/share/omarchy-setup",
            "modules": ["28-scripts", "45-agents"],
        },
        "agent-config": {
            "kind": "files",
            "files": [{"path": ".codex/AGENTS.md", "store": "codex/AGENTS.md", "sha256": None}],
        },
        "huyang": {
            "kind": "git-checkout",
            "source": "https://github.com/iryzhkov/huyang.git",
            "ref": "main",
            "commit": None,
            "path": ".local/share/huyang",
            "build": ["make", "build"],
            "service": "huyang.service",
            "probe": {"kind": "huyang-mcp", "tools": 19},
        },
        "t3-steward": {
            "kind": "github-release",
            "repository": "iryzhkov/t3-steward",
            "version": None,
            "path": ".local/bin/t3-steward",
            "service": "t3-steward.service",
            "installer": "t3-update",
        },
    },
}

Runner = Callable[[list[str], Path | None, int], subprocess.CompletedProcess[str]]


def default_runner(
    argv: list[str], cwd: Path | None, timeout: int
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        argv, cwd=cwd, capture_output=True, check=False, text=True, timeout=timeout
    )


class CaptureError(RuntimeError):
    """The controller's own state is not publishable as a release."""


@dataclass(frozen=True)
class Release:
    manifest: dict[str, Any]

    @property
    def components(self) -> dict[str, dict[str, Any]]:
        return self.manifest["components"]

    def canonical_bytes(self) -> bytes:
        return (json.dumps(self.manifest, indent=2, sort_keys=True) + "\n").encode()

    def sha256(self) -> str:
        return hashlib.sha256(self.canonical_bytes()).hexdigest()


def _string(mapping: dict[str, Any], key: str, context: str) -> str:
    value = mapping.get(key)
    if not isinstance(value, str) or not value:
        raise ValidationError(f"{context}.{key} must be a non-empty string")
    return value


def validate_ref(value: str, context: str) -> str:
    if not REF_RE.fullmatch(value) or ".." in value or value.endswith("/") or "@{" in value:
        raise ValidationError(f"{context}.ref is not a safe git ref: {value!r}")
    return value


def validate_release_version(value: str, context: str) -> str:
    if not VERSION_RE.fullmatch(value):
        raise ValidationError(f"{context}.version is not a release version: {value!r}")
    if LOCAL_BUILD_RE.search(value):
        raise ValidationError(f"{context}.version is a local build, not a release: {value!r}")
    return value


def validate_release(manifest: dict[str, Any]) -> Release:
    if manifest.get("schema_version") != RELEASE_SCHEMA_VERSION:
        raise ValidationError("unsupported release schema_version")
    for key in ("captured_at", "captured_from"):
        _string(manifest, key, "release")
    validate_host_name(manifest["captured_from"])
    components = manifest.get("components")
    if not isinstance(components, dict) or set(components) not in (
        set(LEGACY_COMPONENT_ORDER),
        set(COMPONENT_ORDER),
    ):
        raise ValidationError(f"release components must be exactly {', '.join(COMPONENT_ORDER)}")
    for name, component in components.items():
        if not isinstance(component, dict):
            raise ValidationError(f"component {name} must be an object")
        kind = _string(component, "kind", name)
        if name == "agent-environment":
            try:
                validate_environment(component)
            except (ValueError, KeyError, TypeError) as exc:
                raise ValidationError("invalid agent-environment: " + str(exc)) from exc
            continue
        if name == "worker-configuration":
            try:
                validate_workers(component)
            except (ValueError, KeyError, TypeError) as exc:
                raise ValidationError("invalid worker configuration: " + str(exc)) from exc
            continue
        if name == "t3":
            if set(component) != {"kind", "package", "version", "compatibility", "service"}:
                raise ValidationError("invalid T3 component fields")
            if (
                kind != "npm-package"
                or component["package"] != "t3"
                or component["service"] != "t3code.service"
            ):
                raise ValidationError("T3 must be the pinned t3 npm package and service")
            validate_release_version(_string(component, "version", name), name)
            bounds = component["compatibility"]
            if not isinstance(bounds, dict) or set(bounds) != {"min", "max", "steward_version"}:
                raise ValidationError("T3 compatibility must name min, max and steward_version")
            for bound in ("min", "max"):
                if not re.fullmatch(r"\d+\.\d+\.\d+", str(bounds[bound])):
                    raise ValidationError("T3 compatibility requires numeric release bounds")
            if bounds["steward_version"] != components["t3-steward"][
                "version"
            ] or not _version_tuple(bounds["min"]) <= _version_tuple(
                component["version"]
            ) <= _version_tuple(bounds["max"]):
                raise ValidationError("T3 is outside the captured steward compatibility range")
            continue
        if kind == "git-checkout":
            validate_source_url(_string(component, "source", name))
            validate_ref(_string(component, "ref", name), name)
            validate_git_commit(_string(component, "commit", name))
            validate_home_relative_path(_string(component, "path", name), RELEASE_HOME_ROOTS)
            for key in ("build", "modules"):
                items = component.get(key)
                if items is None:
                    continue
                if not isinstance(items, list) or not all(isinstance(x, str) for x in items):
                    raise ValidationError(f"{name}.{key} must be a string list")
            for module in component.get("modules", []):
                if not MODULE_RE.fullmatch(module):
                    raise ValidationError(f"{name}.modules contains an unsafe name: {module!r}")
            for word in component.get("build", []):
                if not re.fullmatch(r"[A-Za-z0-9._-]+", word):
                    raise ValidationError(f"{name}.build contains an unsafe word: {word!r}")
            if "service" in component and not re.fullmatch(
                r"[a-z][a-z0-9-]*\.service", str(component["service"])
            ):
                raise ValidationError(f"{name}.service is not a user unit name")
            probe = component.get("probe")
            if probe is not None and (
                not isinstance(probe, dict)
                or probe.get("kind") != "huyang-mcp"
                or not isinstance(probe.get("tools"), int)
                or probe["tools"] <= 0
            ):
                raise ValidationError(f"{name}.probe must be a huyang-mcp probe with a tool count")
        elif kind == "github-release":
            if not GITHUB_REPOSITORY_RE.fullmatch(_string(component, "repository", name)):
                raise ValidationError(f"{name}.repository must be owner/name")
            validate_release_version(_string(component, "version", name), name)
            if "session_hook_sha256" in component and not SHA256_RE.fullmatch(
                _string(component, "session_hook_sha256", name)
            ):
                raise ValidationError("invalid steward session hook checksum")
            validate_home_relative_path(_string(component, "path", name), RELEASE_HOME_ROOTS)
            installer = "upkeeper" if "t3" in components else "t3-update"
            if component.get("installer") != installer:
                raise ValidationError(f"{name}.installer must be {installer}")
            if not re.fullmatch(r"[a-z][a-z0-9-]*\.service", _string(component, "service", name)):
                raise ValidationError(f"{name}.service is not a user unit name")
        elif kind == "files":
            files = component.get("files")
            if not isinstance(files, list) or (not files and "agent-environment" not in components):
                raise ValidationError(f"{name}.files must be a non-empty list")
            for entry in files:
                if not isinstance(entry, dict):
                    raise ValidationError(f"{name}.files entries must be objects")
                validate_home_relative_path(_string(entry, "path", name), RELEASE_HOME_ROOTS)
                if not STORE_RE.fullmatch(_string(entry, "store", name)):
                    raise ValidationError(f"{name}.files store path is unsafe")
                if not SHA256_RE.fullmatch(_string(entry, "sha256", name)):
                    raise ValidationError(f"{name}.files sha256 is invalid")
        else:
            raise ValidationError(f"{name}.kind {kind!r} is not supported")
    if "agent-environment" in components:
        if components["agent-config"]["files"]:
            raise ValidationError(
                "agent-config must be empty when agent-environment owns its files"
            )
        if components["omarchy-setup"]["modules"] != ["28-scripts"]:
            raise ValidationError("release environment replaces the 45-agents module")
    return Release(manifest)


def load_release(root: Path) -> Release:
    path = root / RELEASE_FILE
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValidationError(f"cannot load {RELEASE_FILE}: {exc}") from exc
    if not isinstance(raw, dict):
        raise ValidationError(f"{RELEASE_FILE} must contain an object")
    release = validate_release(raw)
    for entry in release.components["agent-config"]["files"]:
        stored = root / AGENT_CONFIG_DIR / entry["store"]
        try:
            digest = hashlib.sha256(stored.read_bytes()).hexdigest()
        except OSError as exc:
            raise ValidationError(f"stored agent file missing: {entry['store']}") from exc
        if digest != entry["sha256"]:
            raise ValidationError(
                f"stored agent file does not match its manifest digest: {entry['store']}"
            )
    return release


def write_release(root: Path, release: Release) -> Path:
    path = root / RELEASE_FILE
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(release.canonical_bytes())
    return path


def _existing_or_default(root: Path) -> dict[str, Any]:
    path = root / RELEASE_FILE
    if path.exists():
        return load_release(root).manifest
    return json.loads(json.dumps(DEFAULT_MANIFEST))


def _git(runner: Runner, checkout: Path, *arguments: str) -> str:
    completed = runner(["git", "-C", str(checkout), *arguments], None, 30)
    if completed.returncode != 0:
        raise CaptureError(
            f"git {' '.join(arguments)} failed in {checkout}: {completed.stderr.strip()}"
        )
    return completed.stdout.strip()


def _capture_checkout(runner: Runner, home: Path, name: str, component: dict[str, Any]) -> None:
    checkout = home / component["path"]
    if not (checkout / ".git").exists():
        raise CaptureError(f"{name}: {checkout} is not a git checkout")
    if _git(runner, checkout, "status", "--porcelain"):
        raise CaptureError(f"{name}: {checkout} has uncommitted changes; a release must be clean")
    commit = validate_git_commit(_git(runner, checkout, "rev-parse", "HEAD"))
    ref = validate_ref(component["ref"], name)
    # Ask the remote itself rather than trusting stale remote-tracking refs: a
    # target can only fetch what origin/<ref> actually reaches today.
    fetched = runner(["git", "-C", str(checkout), "fetch", "--quiet", "origin", ref], None, 90)
    if fetched.returncode != 0:
        raise CaptureError(f"{name}: cannot fetch origin/{ref}: {fetched.stderr.strip()[-300:]}")
    reachable = runner(
        ["git", "-C", str(checkout), "merge-base", "--is-ancestor", commit, "FETCH_HEAD"],
        None,
        30,
    )
    if reachable.returncode != 0:
        raise CaptureError(
            f"{name}: commit {commit[:12]} is not on origin/{ref}; "
            "push it there before publishing a release"
        )
    component["commit"] = commit


def _capture_steward(runner: Runner, home: Path, name: str, component: dict[str, Any]) -> None:
    binary = home / component["path"]
    completed = runner([str(binary), "version"], None, 15)
    if completed.returncode != 0:
        raise CaptureError(f"{name}: {binary} version failed")
    words = completed.stdout.split()
    if len(words) < 2:
        raise CaptureError(f"{name}: cannot parse version output {completed.stdout.strip()!r}")
    try:
        component["version"] = validate_release_version(words[1].removeprefix("v"), name)
    except ValidationError as exc:
        raise CaptureError(
            f"{name}: installed build {words[1]!r} is not a published release; "
            "tag and release it first"
        ) from exc


def _version_tuple(value: str) -> tuple[int, ...]:
    if not re.fullmatch(r"\d+\.\d+\.\d+", value):
        raise ValidationError("T3 pair requires a stable numeric npm version")
    return tuple(int(n) for n in value.split("."))


def _capture_t3(runner: Runner, home: Path, steward: dict[str, Any]) -> dict[str, Any]:
    version = runner([str(home / steward["path"]), "version"], None, 15)
    match = re.search(r"tested with T3 (\d+\.\d+\.\d+)\.\.(\d+\.\d+\.\d+)", version.stdout)
    if version.returncode or not match:
        raise CaptureError("steward does not report its tested T3 range")
    installed = runner(["npm", "ls", "-g", "--depth=0", "--json", "t3"], None, 30)
    try:
        package = json.loads(installed.stdout)["dependencies"]["t3"]["version"]
    except (ValueError, KeyError, TypeError) as exc:
        raise CaptureError("cannot observe installed npm t3 package") from exc
    if installed.returncode or not _version_tuple(match[1]) <= _version_tuple(
        package
    ) <= _version_tuple(match[2]):
        raise CaptureError("installed T3 is outside the steward tested range")
    return {
        "kind": "npm-package",
        "package": "t3",
        "version": package,
        "compatibility": {"min": match[1], "max": match[2], "steward_version": steward["version"]},
        "service": "t3code.service",
    }


def _capture_files(root: Path, home: Path, name: str, component: dict[str, Any]) -> None:
    for entry in component["files"]:
        source = home / validate_home_relative_path(entry["path"], RELEASE_HOME_ROOTS)
        try:
            payload = source.read_bytes()
        except OSError as exc:
            raise CaptureError(f"{name}: cannot read {source}") from exc
        if not STORE_RE.fullmatch(entry["store"]):
            raise ValidationError(f"{name}.files store path is unsafe")
        target = root / AGENT_CONFIG_DIR / entry["store"]
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(payload)
        entry["sha256"] = hashlib.sha256(payload).hexdigest()


def capture_release(
    root: Path,
    *,
    controller: str,
    home: Path | None = None,
    runner: Runner = default_runner,
    now: datetime | None = None,
) -> Release:
    """Read the controller's own machine into a validated release manifest."""
    home = (home or Path.home()).resolve()
    if _git(runner, root, "status", "--porcelain"):
        raise CaptureError("release repository has uncommitted changes; commit before capture")
    manifest = _existing_or_default(root)
    components = manifest["components"]
    components["huyang"]["ref"] = "main"
    for name in ("omarchy-setup", "huyang"):
        _capture_checkout(runner, home, name, components[name])
    _capture_steward(runner, home, "t3-steward", components["t3-steward"])
    hook_hash = components["t3-steward"].get("session_hook_sha256")
    if hook_hash:
        hook_path = home / ".config/opencode/plugins/steward-session.js"
        if (
            hook_path.is_symlink()
            or not hook_path.is_file()
            or hashlib.sha256(hook_path.read_bytes()).hexdigest() != hook_hash
        ):
            raise CaptureError("installed OpenCode session hook does not match the reviewed pin")
    components["t3"] = _capture_t3(runner, home, components["t3-steward"])
    components["t3-steward"]["installer"] = "upkeeper"
    provenance = ProjectInstructionRef(
        "github.com/iryzhkov/UpKeeper",
        _git(runner, root, "rev-parse", "HEAD"),
        "README.md",
        hashlib.sha256((root / "README.md").read_bytes()).hexdigest(),
    )
    try:
        components["agent-environment"] = capture_environment(home, provenance)
    except (ValueError, OSError) as exc:
        raise CaptureError("agent-environment: " + str(exc)) from exc
    components["agent-config"]["files"] = []
    components["omarchy-setup"]["modules"] = ["28-scripts"]
    components.setdefault("worker-configuration", {"kind": "worker-configuration", "hosts": {}})
    manifest["captured_at"] = (
        (now or datetime.now(UTC)).astimezone(UTC).isoformat().replace("+00:00", "Z")
    )
    manifest["captured_from"] = validate_host_name(controller)
    return validate_release(manifest)


def describe(release: Release) -> list[str]:
    components = release.components
    setup = components["omarchy-setup"]
    huyang = components["huyang"]
    files = ", ".join(
        f"{entry['path']}@{entry['sha256'][:12]}" for entry in components["agent-config"]["files"]
    )
    return [
        f"omarchy-setup {setup['commit'][:12]} ({setup['ref']})",
        f"agent-config {files}",
        f"huyang {huyang['commit'][:12]} ({huyang['ref']})",
        f"t3-steward {components['t3-steward']['version']}",
    ] + (
        [
            f"t3 {components['t3']['version']}",
            f"agent-environment {len(components['agent-environment']['files'])} owned files",
            f"worker-configuration {len(components['worker-configuration']['hosts'])} hosts",
        ]
        if "t3" in components
        else []
    )
