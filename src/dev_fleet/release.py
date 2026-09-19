"""The release manifest: what `upkeeper push` captures and `upkeeper pull` applies.

A release is the exact state of the controller's own machine, taken from what is
running there rather than typed by hand: the Huyang checkout commit, the
omarchy-setup checkout commit, the installed t3-steward release, and the
agent instruction files that no other mechanism distributes. Every value is
validated on the way in so that nothing in the manifest can become a shell
fragment on a target.
"""

from __future__ import annotations

import copy
import hashlib
import json
import os
import re
import subprocess
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, TextIO

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


# The resident agent instructions are the shared CLAUDE.md, the AGENTS.md family
# under ~/.config/agents and the skills under ~/.claude/skills. Every agent
# session on every host loads them, and they are the one part of a release with
# no repository behind it at capture time: capture_environment reads them out of
# the pushing host's home directory and convergence writes those exact bytes over
# the same paths on every other host.
#
# Nothing downstream can notice a mistake here. A manifest captured from a host
# whose copies have drifted is well formed, its assets validate, and every
# convergence that applies it succeeds. The difference is visible only on the
# machine the capture is taken from, which makes this the only place it can be
# refused. This mirrors internal/release/resident.go in the Go implementation,
# down to the wording of the refusals, because the two must not disagree about
# what a host is allowed to publish.
RESIDENT_GENERATOR = ".local/bin/agents-instructions-gen"
RESIDENT_DIVERGED_EXIT = 3
# The two options the check is spelled with. They are looked for in the
# installed generator before it is run, and their absence is the degraded case
# rather than a reason to try anyway: the generator that predates the check
# ignores every argument it is given, rewrites ~/.config/agents and
# ~/.codex/AGENTS.md unconditionally, and exits 0. Running that copy as a check
# would modify the host it was supposed to report on and report success while
# doing it.
RESIDENT_CAPABILITY_MARKERS = ("--check", "--root")
RESIDENT_GENERATOR_SIZE_LIMIT = 1 << 20
# The check reads a few dozen small files and compares them, so a minute is
# generous; a check that hangs is a check that could not run.
RESIDENT_CHECK_TIMEOUT = 60
RESIDENT_OVERRIDE_FLAG = "--publish-diverged-instructions"
RESIDENT_WHY = (
    "A release capture photographs this host's home directory. The shared CLAUDE.md, "
    "the files under ~/.config/agents and every skill under ~/.claude/skills are copied "
    "into the manifest exactly as this machine holds them, and the next convergence "
    "writes those bytes over the same paths on every other host. No repository installs "
    "a skill onto a host, so a file edited in place here becomes the fleet's instruction "
    "to every agent on every machine, and an edit made anywhere else is destroyed "
    "without a word. A host that has drifted from its source would publish its drift."
)


def _resident_uncheckable(reason: str) -> CaptureError:
    """Refuse a capture whose resident instructions could not be checked at all.

    "I could not check" is not "there is nothing to check". Treating the two as
    the same is the silent-success shape this gate exists to remove, so the
    degraded case refuses as firmly as a detected divergence and names which
    degradation it is.
    """
    return CaptureError(
        "agent-environment: cannot check whether this host's agent instructions match "
        f"their source: {reason}.\n\n{RESIDENT_WHY}\n\n"
        'A capture taken now would publish this host unexamined, so it is refused: "could '
        'not check" is not "nothing to check". Repair the cause and capture again, or '
        "capture only the components that do not carry the resident instructions with "
        "--components. There is deliberately no override for this case: an override has "
        "to print what it promotes, and a check that could not run cannot name it.\n"
        "Nothing has been captured, written, committed or pushed."
    )


def _resident_reported_paths(stdout: str) -> list[str]:
    """Keep the generator's own per-path lines, and nothing else.

    Each already carries the direction of the difference: whether the host copy
    differs from what the checkout generates, is absent, or exists on this host
    and is generated by nothing.

    What must not be kept is the rest of the report. A divergence is printed as
    a header, then the block of path lines, then an epilogue telling the
    operator how to converge, and the third line of that epilogue is indented
    exactly like a path line::

          /home/igor/.local/share/omarchy-setup/run.sh   (or: agents-instructions-gen --root)

    Keeping it put a converge command into the override's list of the paths it
    promotes, which is the one artifact the override exists to produce. It also
    meant that a report naming no path at all still yielded one line here, so
    the "divergence without naming a path" refusal could never fire against the
    real generator.

    Two conditions separate the block from the epilogue. A path line belongs to
    the first run of indented lines, which the epilogue's unindented opening
    ends, and its indent is followed by one unbroken token and ": ", which
    "run.sh   (or:" is not. A line in a shape neither condition recognises is
    dropped rather than reported, which can only turn a divergence into the
    refusal that says no path was named; it can never turn one into a capture.
    """
    paths: list[str] = []
    block = False
    for line in stdout.splitlines():
        if not line.startswith("  ") or not line.strip():
            if block:
                break
            continue
        block = True
        entry = line.strip()
        if _resident_names_path(entry):
            paths.append("  " + entry)
    return paths


def _resident_names_path(entry: str) -> bool:
    """Report whether one line of the generator's path block opens with a path.

    A report line is a single unbroken token, then ": ", then the direction of
    the difference. That is the shape the generator prints every comparison in,
    and the shape its converge epilogue does not have.
    """
    label, separator, direction = entry.partition(": ")
    if not separator or not label or not direction:
        return False
    return " " not in label and "\t" not in label


def _resident_generator_answers_checks(generator: Path) -> None:
    """Refuse to run an installed generator that cannot be asked a question.

    The copy that cannot be asked answers by rewriting the files.

    Be exact about how much this establishes. It is a substring test over the
    installed file's bytes, so it proves only that both option strings appear
    somewhere in it, in a usage line, in a comment or in a here-document. It
    does not prove the command implements them. What it reliably recognises is
    the one copy it was written for: the generator that predates the check
    parses no arguments at all and mentions neither string, so it is refused
    before it can rewrite the host it was asked to report on. A generator that
    merely mentions both flags, ignores them and exits 0 rather than dying on an
    unknown option would pass here and be believed, which is a residual risk
    this inspection does not close.

    It deliberately does not compare the installed generator against the copy in
    the checkout, for the reasons the Go implementation records at length in
    internal/release/resident.go.
    """
    size = generator.stat().st_size
    if size > RESIDENT_GENERATOR_SIZE_LIMIT:
        raise _resident_uncheckable(
            f"{generator} is {size} bytes, which is not the generator script this expects"
        )
    try:
        payload = generator.read_text(encoding="utf-8", errors="replace")
    except OSError as exc:
        raise _resident_uncheckable(f"{generator} cannot be read: {exc}") from exc
    for marker in RESIDENT_CAPABILITY_MARKERS:
        if marker in payload:
            continue
        raise _resident_uncheckable(
            f"the generator installed at {generator} does not understand {marker}, so it "
            "predates the check; that copy ignores its arguments and rewrites this host's "
            "instruction files instead of reporting on them, so it is not run. Converge "
            "omarchy-setup to install the copy that can answer"
        )


def _guard_resident_instructions(
    runner: Runner,
    home: Path,
    setup: dict[str, Any],
    *,
    publish_diverged: bool,
    notice: TextIO | None,
) -> None:
    """Refuse a capture that would publish unverified resident instructions.

    The comparison is delegated to the generator's own --check mode rather than
    reimplemented here, so that "diverged" keeps one definition, in the
    repository that owns the files being compared.
    """
    path = setup.get("path")
    if not isinstance(path, str) or not path:
        raise _resident_uncheckable(
            "the release names no omarchy-setup checkout, so there is no source to "
            "compare this host against"
        )
    checkout = home / path
    if not (checkout / ".git").exists():
        raise _resident_uncheckable(
            f"no omarchy-setup checkout at {checkout}, which is where the instructions "
            "this host installs are generated from"
        )
    generator = home / RESIDENT_GENERATOR
    if not generator.is_file() or not os.access(generator, os.X_OK):
        raise _resident_uncheckable(
            f"no executable {generator}, which is the command that decides whether this "
            f"host matches {checkout}; the omarchy-setup 28-scripts module installs it"
        )
    _resident_generator_answers_checks(generator)
    # HOME is set for the check rather than inherited, because the capture may be
    # reading a home directory that is not this process's own. OMARCHY_SETUP_ROOT
    # is dropped because the checkout is passed explicitly and an inherited value
    # naming a different tree would make the answer depend on the caller's shell.
    #
    # Every way that invocation can fail to produce an answer is turned into the
    # same named refusal the Go implementation gives, rather than left to
    # propagate. An escaping TimeoutExpired or OSError would leave the CLI
    # exiting 1 with a traceback, and a traceback is not a refusal an operator
    # can act on: this gate's whole contract is that a check which could not run
    # refuses at EXIT_PREFLIGHT and says which degradation it was.
    try:
        completed = runner(
            [
                "env",
                "-u",
                "OMARCHY_SETUP_ROOT",
                f"HOME={home}",
                str(generator),
                "--root",
                str(checkout),
                "--check",
            ],
            None,
            RESIDENT_CHECK_TIMEOUT,
        )
    except subprocess.TimeoutExpired as exc:
        raise _resident_uncheckable(
            f"{generator} --root {checkout} --check could not be run: it did not finish "
            f"within {RESIDENT_CHECK_TIMEOUT} seconds"
        ) from exc
    except OSError as exc:
        raise _resident_uncheckable(
            f"{generator} --root {checkout} --check could not be run: {exc}"
        ) from exc
    if completed.returncode == 0:
        return
    detail = (completed.stderr.strip() or completed.stdout.strip() or "it printed nothing")[-400:]
    if completed.returncode != RESIDENT_DIVERGED_EXIT:
        raise _resident_uncheckable(
            f"{generator} --root {checkout} --check exited {completed.returncode} rather "
            f"than 0 (converged) or {RESIDENT_DIVERGED_EXIT} (diverged): {detail}"
        )
    diverged = _resident_reported_paths(completed.stdout)
    if not diverged:
        raise _resident_uncheckable(
            f"{generator} --root {checkout} --check reported a divergence without naming "
            f"a path: {detail}"
        )
    if publish_diverged:
        if notice is None:
            raise CaptureError(
                f"agent-environment: {RESIDENT_OVERRIDE_FLAG} was given but this capture has "
                "nowhere to print what it would promote, and an override nobody can read is "
                "not an override."
            )
        listed = "\n".join(diverged)
        notice.write(
            f"{RESIDENT_OVERRIDE_FLAG}: publishing this host's agent instructions although "
            f"they differ from {checkout}.\nEach path below is promoted to every host in the "
            f"fleet on its next convergence, overwriting whatever that host holds:\n{listed}\n"
        )
        return
    listed = "\n".join(diverged)
    raise CaptureError(
        f"agent-environment: this host's agent instructions have diverged from {checkout}, "
        f"and capturing it would publish that divergence to the whole fleet.\n{listed}\n\n"
        f"{RESIDENT_WHY}\n\nConverge this host and capture again:\n"
        f"  {checkout}/run.sh\n  {generator} --root {checkout} --check\n"
        "If these bytes are the ones that should win, commit them to the omarchy-setup "
        "checkout and converge from there; a capture is not a place to author "
        "instructions. To publish them from this host anyway, ask for it by name with "
        f"{RESIDENT_OVERRIDE_FLAG}, which prints every path it promotes.\n"
        "Nothing has been captured, written, committed or pushed."
    )


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
    only: Sequence[str] | None = None,
    publish_diverged_instructions: bool = False,
    notice: TextIO | None = None,
) -> Release:
    """Read the controller's own machine into a validated release manifest.

    Capture reads the whole machine, because the components are not independent:
    the T3 pin is derived from the installed steward, and the agent environment
    is a set of files rather than a version. When ``only`` is given, every component
    outside it is restored to exactly what the existing manifest published, so a
    capture asked for one component cannot carry another along.

    Without that, a controller whose local checkout of some unrelated component
    is ahead of the release proposes that newer commit on every capture, and the
    operator has to notice and revert it by hand each time.

    Before anything is observed, a capture that would publish the resident agent
    instructions proves that this host still holds what their source generates.
    See _guard_resident_instructions for why that refusal is the single
    enforcement point in the arrangement rather than one check among several.
    """
    home = (home or Path.home()).resolve()
    selected = None if only is None else set(only)
    published: dict[str, Any] = {}
    if selected is not None:
        unknown = sorted(selected - set(COMPONENT_ORDER))
        if unknown:
            raise CaptureError("unknown component(s): " + ", ".join(unknown))
        published = copy.deepcopy(_existing_or_default(root)["components"])
    if _git(runner, root, "status", "--porcelain"):
        raise CaptureError("release repository has uncommitted changes; commit before capture")
    manifest = _existing_or_default(root)
    components = manifest["components"]
    # The gate sits here, inside the capture, rather than in the one command
    # that happens to call it: every caller publishes the same photograph of the
    # same home directory. A selection that does not name agent-environment is
    # not gated, because the environment captured below is discarded again at
    # the end and the release keeps what it already published.
    if selected is None or "agent-environment" in selected:
        # A manifest with no omarchy-setup component is read tolerantly, as the
        # Go implementation reads it, so that the gate answers with its "the
        # release names no omarchy-setup checkout" refusal instead of a KeyError
        # that would exit 1 with a traceback.
        setup = components.get("omarchy-setup")
        _guard_resident_instructions(
            runner,
            home,
            setup if isinstance(setup, dict) else {},
            publish_diverged=publish_diverged_instructions,
            notice=notice,
        )
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
    if selected is not None:
        for name in list(manifest["components"]):
            if name in selected:
                continue
            if name in published:
                manifest["components"][name] = published[name]
            else:
                del manifest["components"][name]
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
