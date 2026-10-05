"""`upkeeper pull`: converge selected hosts on the committed release manifest.

The controller never streams shell to a target. It sends one fixed standard-library
Python program on stdin, the same transport `fleet doctor` uses, with the validated
release embedded as a JSON literal. On the target that program observes each
component, changes only what differs, verifies the result, and rolls a failed
Huyang build back to the commit it found. Every host is locked on both sides for
the duration and the controller writes a redacted run record.
"""

from __future__ import annotations

import inspect
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

from . import (
    environment,
    huyang_lifecycle,
    release_assets,
    t3_lifecycle,
    worker_lifecycle,
    worker_release,
)
from .config import FleetConfig, HostSpec
from .constants import (
    EXIT_DEPLOYMENT,
    EXIT_DRIFT,
    EXIT_LOCK_CONTENTION,
    EXIT_PARTIAL,
    EXIT_ROLLBACK,
    EXIT_SUCCESS,
)
from .locks import LocalLockStore, LockContentionError, LockMetadata
from .probe import local_machine_id_sha256
from .records import RunRecord, RunRecordStore
from .redaction import redact
from .release import AGENT_CONFIG_DIR, COMPONENT_ORDER, Release
from .run_ids import new_run_id

# Result states a host or component can end in, from best to worst.
STATES = ("converged", "updated", "ahead", "skipped", "lock-contention", "rolled-back", "failed", "rollback-failed")

REMOTE_AGENT = r'''
import hashlib
import json
import os
import shutil
import subprocess
import sys
import threading
import time
import re
from pathlib import Path
from dev_fleet.release_assets import observe_environment, apply_environment
from dev_fleet.worker_release import worker_projection
from dev_fleet.t3_lifecycle import retire_updater, seed_t3_service
from dev_fleet.huyang_lifecycle import bootstrap_huyang
from dev_fleet.worker_lifecycle import WorkerRestartFailed, restart_persistent_worker

PAYLOAD = json.loads(__PAYLOAD__)
HOME = Path.home()
HOST = PAYLOAD["host"]
RUN_ID = PAYLOAD["run_id"]
DRY_RUN = PAYLOAD["dry_run"]
COMPONENTS = PAYLOAD["manifest"]["components"]
if HOST.get("setup_checkout") and "omarchy-setup" in COMPONENTS:
    COMPONENTS["omarchy-setup"] = dict(COMPONENTS["omarchy-setup"], path=HOST["setup_checkout"])
SELECTED = PAYLOAD["components"]
STORED = PAYLOAD["stored_files"]
ENV = dict(os.environ)
ENV["PATH"] = ":".join(
    [
        str(HOME / ".local/share/mise/shims"),
        str(HOME / ".local/bin"),
        str(HOME / "go/bin"),
        "/usr/local/go/bin",
        ENV.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
    ]
)
ENV["GIT_TERMINAL_PROMPT"] = "0"
ENV["GIT_SSH_COMMAND"] = "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=10"
RUNTIME = Path(os.environ.get("XDG_RUNTIME_DIR") or f"/run/user/{os.getuid()}")


class StepFailed(Exception):
    pass


def run(argv, timeout=60, cwd=None):
    try:
        completed = subprocess.run(
            argv, capture_output=True, check=False, cwd=cwd, env=ENV, text=True, timeout=timeout
        )
    except OSError as exc:
        return {"ok": False, "rc": None, "output": str(exc)}
    except subprocess.TimeoutExpired:
        return {"ok": False, "rc": None, "output": "timeout", "timeout": True}
    output = (completed.stdout + completed.stderr).strip()
    return {"ok": completed.returncode == 0, "rc": completed.returncode, "output": output[-2000:]}


def must(argv, timeout=60, cwd=None, what=None):
    result = run(argv, timeout=timeout, cwd=cwd)
    if not result["ok"]:
        raise StepFailed(f"{what or argv[0]} failed: {result['output'][-400:]}")
    return result["output"]


def sha256_file(path):
    try:
        return hashlib.sha256(Path(path).read_bytes()).hexdigest()
    except OSError:
        return None


def unit_active(unit):
    return run(["systemctl", "--user", "is-active", unit], timeout=10)["output"] == "active"


def unit_loaded(unit):
    result = run(["systemctl", "--user", "show", unit, "-p", "LoadState", "--value"], timeout=10)
    return result["ok"] and result["output"] == "loaded"


def git_head(checkout):
    result = run(["git", "-C", str(checkout), "rev-parse", "HEAD"], timeout=15)
    return result["output"] if result["ok"] else None


def git_dirty(checkout):
    result = run(["git", "-C", str(checkout), "status", "--porcelain"], timeout=15)
    return bool(result["output"]) if result["ok"] else None


def git_contains(checkout, ancestor, descendant):
    return run(
        ["git", "-C", str(checkout), "merge-base", "--is-ancestor", ancestor, descendant], timeout=15
    )["ok"]


# ---- target-side lock ---------------------------------------------------------

LOCK_DIR = HOME / ".local/state/dev-fleet/locks" / f"{HOST['name']}.lock"
LOCK_STATE = {"owned": False}


def acquire_lock():
    if DRY_RUN and not LOCK_DIR.exists():
        return None
    LOCK_DIR.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    try:
        LOCK_DIR.mkdir(mode=0o700)
    except FileExistsError:
        owner = None
        try:
            owner = json.loads((LOCK_DIR / "owner.json").read_text())
        except (OSError, ValueError):
            pass
        # When the controller is this machine it already holds the lock for this
        # run in the same directory; re-entering our own run is not contention,
        # and the controller, not this program, releases it.
        if isinstance(owner, dict) and owner.get("run_id") == RUN_ID:
            return None
        return owner or {"run_id": "unknown"}
    (LOCK_DIR / "owner.json").write_text(
        json.dumps(PAYLOAD["lock"], sort_keys=True) + "\n", encoding="utf-8"
    )
    LOCK_STATE["owned"] = True
    return None


def release_lock():
    if not LOCK_STATE["owned"]:
        return
    try:
        (LOCK_DIR / "owner.json").unlink()
        LOCK_DIR.rmdir()
    except OSError:
        pass


# ---- component: omarchy-setup ---------------------------------------------------


def managed_file_pairs(checkout):
    pairs = []
    if "agent-environment" not in COMPONENTS:
        pairs.append((checkout / "config/claude/CLAUDE.md", HOME / ".claude/omarchy-setup/CLAUDE.md"))
    skills = checkout / "config/claude/skills"
    if "agent-environment" not in COMPONENTS and skills.is_dir():
        for skill in sorted(skills.iterdir()):
            for source in sorted(skill.iterdir()):
                if source.is_file():
                    pairs.append((source, HOME / ".claude/skills" / skill.name / source.name))
    scripts = checkout / "config/bin"
    if scripts.is_dir():
        for source in sorted(scripts.iterdir()):
            if source.is_file():
                pairs.append((source, HOME / ".local/bin" / source.name))
    return pairs


def stale_managed_files(checkout):
    return [
        str(target.relative_to(HOME))
        for source, target in managed_file_pairs(checkout)
        if sha256_file(source) != sha256_file(target)
    ]


def observe_omarchy_setup(spec):
    checkout = HOME / spec["path"]
    head = git_head(checkout)
    return {
        "commit": head,
        "dirty": git_dirty(checkout) if head else None,
        "stale_files": stale_managed_files(checkout) if head else None,
    }


def apply_omarchy_setup(spec, before, actions):
    checkout = HOME / spec["path"]
    if before["commit"] is None:
        raise StepFailed(f"{checkout} is not a git checkout; bootstrap omarchy-setup by hand first")
    if before["dirty"]:
        raise StepFailed(f"{checkout} has local changes; refusing to pull over them")
    state = "converged"
    if before["commit"] != spec["commit"]:
        if DRY_RUN:
            actions.append(f"would git pull --ff-only to {spec['commit'][:12]}")
        else:
            must(["git", "-C", str(checkout), "pull", "--ff-only", "--quiet"], timeout=120, what="git pull")
            actions.append("git pull --ff-only")
        head = git_head(checkout)
        if not DRY_RUN and head != spec["commit"]:
            if head and git_contains(checkout, spec["commit"], head):
                state = "ahead"
                actions.append(f"checkout is ahead of the release at {head[:12]}")
            else:
                raise StepFailed(f"after pull HEAD is {head}, wanted {spec['commit']}")
    stale = stale_managed_files(checkout)
    if stale or before["commit"] != spec["commit"]:
        modules = spec.get("modules", [])
        if DRY_RUN:
            actions.append(f"would run modules {' '.join(modules)} (stale: {', '.join(stale) or 'none'})")
        else:
            run_modules(checkout, modules)
            actions.append(f"ran modules {' '.join(modules)}")
            remaining = stale_managed_files(checkout)
            if remaining:
                raise StepFailed(f"managed files still differ after modules: {', '.join(remaining)}")
        if state == "converged":
            state = "updated"
    return state


def run_modules(checkout, modules):
    if set(modules) & {"26-t3", "45-agents"}:
        raise StepFailed("historical setup release includes retired fleet modules; select other components")
    if shutil.which("omarchy") and (checkout / "run.sh").exists():
        argv = ["bash", str(checkout / "run.sh"), "--yes", "--skip-secrets"]
        for module in modules:
            argv += ["--only", module]
        must(argv, timeout=900, what="omarchy-setup run.sh")
        return
    # A host without Omarchy (homelab) cannot use run.sh; drive the modules the
    # bootstrap environment without delegating fleet installation.
    env_prefix = {
        "OMARCHY_SETUP_ROOT": str(checkout),
        "OMARCHY_SETUP_LIB": str(checkout / "lib"),
        "ASSUME_YES": "1",
        "SETUP_PROFILE": "remote",
    }
    saved = dict(ENV)
    ENV.update(env_prefix)
    try:
        for module in modules:
            must(["bash", str(checkout / "modules/common" / f"{module}.sh")], timeout=900, what=module)
    finally:
        ENV.clear()
        ENV.update(saved)


# ---- component: agent-config ----------------------------------------------------


def observe_agent_config(spec):
    return {entry["path"]: sha256_file(HOME / entry["path"]) for entry in spec["files"]}


def apply_agent_config(spec, before, actions):
    changed = False
    for entry in spec["files"]:
        target = HOME / entry["path"]
        if before.get(entry["path"]) == entry["sha256"]:
            continue
        payload = STORED[entry["store"]].encode("utf-8")
        if hashlib.sha256(payload).hexdigest() != entry["sha256"]:
            raise StepFailed(f"stored content for {entry['path']} does not match its digest")
        if DRY_RUN:
            actions.append(f"would write {entry['path']}")
            changed = True
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        if target.exists():
            backup = target.with_name(target.name + f".upkeeper-{RUN_ID}.bak")
            shutil.copy2(target, backup)
            actions.append(f"backed up {entry['path']} to {backup.name}")
        temporary = target.with_name(f".{target.name}.upkeeper-tmp")
        temporary.write_bytes(payload)
        os.chmod(temporary, 0o644)
        os.replace(temporary, target)
        if sha256_file(target) != entry["sha256"]:
            raise StepFailed(f"{entry['path']} did not verify after write")
        actions.append(f"wrote {entry['path']}")
        changed = True
    return "updated" if changed else "converged"


# ---- component: huyang ------------------------------------------------------------


def huyang_socket():
    return RUNTIME / "huyang/control.sock"


def huyang_probe(checkout, expected_tools):
    binary = checkout / "bin/huyang"
    argv = [str(binary), "mcp", "-socket", str(huyang_socket()), "-profile", "full"]
    try:
        proc = subprocess.Popen(
            argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            env=ENV, text=True,
        )
    except OSError as exc:
        return {"ok": False, "error": str(exc)}
    found = {}

    def reader():
        for line in proc.stdout:
            line = line.strip()
            if not line:
                continue
            try:
                message = json.loads(line)
            except ValueError:
                continue
            if message.get("id") in (1, 2):
                found[message["id"]] = message
            if 2 in found:
                break

    thread = threading.Thread(target=reader, daemon=True)
    thread.start()
    try:
        proc.stdin.write(json.dumps({
            "jsonrpc": "2.0", "id": 1, "method": "initialize",
            "params": {"protocolVersion": "2025-06-18", "capabilities": {},
                       "clientInfo": {"name": "upkeeper", "version": "1"}},
        }) + "\n")
        proc.stdin.flush()
        time.sleep(1)
        proc.stdin.write(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n")
        proc.stdin.write(json.dumps({"jsonrpc": "2.0", "id": 2, "method": "tools/list"}) + "\n")
        proc.stdin.flush()
    except (OSError, ValueError) as exc:
        proc.kill()
        return {"ok": False, "error": f"mcp write failed: {exc}"}
    thread.join(timeout=30)
    proc.kill()
    tools = found.get(2, {}).get("result", {}).get("tools")
    server = found.get(1, {}).get("result", {}).get("serverInfo", {})
    count = len(tools) if isinstance(tools, list) else None
    return {
        "ok": count == expected_tools,
        "tools": count,
        "server_version": server.get("version"),
    }


def huyang_wait_probe(checkout, expected_tools):
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline and not huyang_socket().exists():
        time.sleep(1)
    if not huyang_socket().exists():
        raise StepFailed("control socket did not appear after restart")
    return huyang_probe(checkout, expected_tools)


def observe_huyang(spec):
    checkout = HOME / spec["path"]
    head = git_head(checkout)
    active = unit_active(spec["service"])
    return {
        "commit": head,
        "dirty": git_dirty(checkout) if head else None,
        "service_active": active,
        "socket": huyang_socket().exists(),
        "binary_sha256": sha256_file(checkout / "bin/huyang"),
    }


def huyang_build_and_restart(spec, checkout, commit, actions):
    must(["git", "-C", str(checkout), "checkout", "--quiet", "--detach", commit], timeout=60, what="git checkout")
    if git_head(checkout) != commit:
        raise StepFailed("checkout did not land on the requested commit")
    actions.append(f"checked out {commit[:12]}")
    must(list(spec.get("build", ["make", "build"])), timeout=600, cwd=checkout, what="build")
    actions.append("built")
    must(["systemctl", "--user", "restart", spec["service"]], timeout=60, what="restart")
    actions.append(f"restarted {spec['service']}")
    deadline = time.time() + 45
    while time.time() < deadline and not huyang_socket().exists():
        time.sleep(1)
    if not huyang_socket().exists():
        raise StepFailed("control socket did not appear after restart")
    time.sleep(1)
    probe = huyang_probe(checkout, spec["probe"]["tools"])
    actions.append(f"mcp probe tools={probe.get('tools')} server={probe.get('server_version')}")
    if not probe["ok"]:
        raise StepFailed(f"mcp probe failed: {probe}")


def apply_huyang(spec, before, actions):
    checkout = HOME / spec["path"]
    if before["commit"] is None:
        return bootstrap_huyang(HOME, spec, RUN_ID, DRY_RUN, ENV, must, huyang_wait_probe, actions)
    if before["dirty"]:
        raise StepFailed(f"{checkout} has local changes; refusing to move it")
    if (before["commit"] != spec["commit"] and not PAYLOAD.get("allow_downgrade", False)
            and git_contains(checkout, spec["commit"], before["commit"])):
        actions.append("checkout is ahead of the pin; normal pull preserves newer Huyang state")
        return "ahead"
    converged = before["commit"] == spec["commit"] and before["service_active"] and before["socket"]
    if converged:
        probe = huyang_probe(checkout, spec["probe"]["tools"])
        actions.append(f"mcp probe tools={probe.get('tools')}")
        if probe["ok"]:
            return "converged"
        actions.append("running service does not answer the probe; rebuilding")
    if DRY_RUN:
        actions.append(f"would fetch origin {spec['ref']}, check out {spec['commit'][:12]}, build, restart")
        return "updated"
    must(["git", "-C", str(checkout), "fetch", "--quiet", "origin", spec["ref"]], timeout=180, what="git fetch")
    actions.append(f"fetched origin {spec['ref']}")
    if not run(["git", "-C", str(checkout), "cat-file", "-e", spec["commit"] + "^{commit}"], timeout=15)["ok"]:
        raise StepFailed(f"origin/{spec['ref']} does not contain {spec['commit'][:12]}")
    if git_dirty(checkout) or git_head(checkout) != before["commit"]:
        raise StepFailed("Huyang checkout changed during preparation; preserving local work")
    if (not PAYLOAD.get("allow_downgrade", False)
            and not git_contains(checkout, before["commit"], spec["commit"])):
        actions.append("checkout diverges from the pin; normal pull preserves local Huyang work")
        return "ahead"
    try:
        huyang_build_and_restart(spec, checkout, spec["commit"], actions)
    except StepFailed as failure:
        actions.append(f"failure: {failure}")
        actions.append(f"rolling back to {before['commit'][:12]}")
        try:
            huyang_build_and_restart(spec, checkout, before["commit"], actions)
        except StepFailed as rollback_failure:
            raise StepFailed(f"rollback failed: {rollback_failure}") from rollback_failure
        raise RolledBack(str(failure))
    return "updated"


class RolledBack(Exception):
    pass


# ---- component: t3-steward ------------------------------------------------------------


def version_tuple(value):
    core, _, pre = value.partition("-")
    numbers = tuple(int(part) for part in core.split("."))
    # A prerelease sorts below its final version, like semver.
    return (numbers, 0 if pre else 1, pre)


def observe_steward(spec):
    binary = HOME / spec["path"]
    result = run([str(binary), "version"], timeout=15)
    words = result["output"].split() if result["ok"] else []
    return {
        "version": words[1] if len(words) > 1 else None,
        "service_active": unit_active(spec["service"]),
        "binary_sha256": sha256_file(binary),
    }


def apply_steward(spec, before, actions):
    return apply_pinned_steward(spec, before, actions)


# ---- U0 pair, environment and worker-bootstrap components ---------------------------


def observe_release_environment(spec):
    _, _, _, drift = observe_environment(spec, HOME)
    return {"drift": drift}


def apply_release_environment(spec, before, actions):
    result = apply_environment(spec, HOME, RUN_ID, DRY_RUN)
    actions.extend(("would project " if DRY_RUN else "projected ") + p for p in result["changed"])
    actions.extend(("would remove " if DRY_RUN else "removed ") + p for p in result["removed"])
    if result["state"] == "rolled-back":
        actions.append("restored and verified the complete previous environment")
    return result["state"]


def observe_worker_configuration(spec):
    return worker_projection(spec, HOME, HOST["name"], RUN_ID, dry_run=True)


def apply_worker_configuration(spec, before, actions):
    result = worker_projection(spec, HOME, HOST["name"], RUN_ID, dry_run=DRY_RUN)
    if result["configured"]:
        actions.append("worker bootstrap projected; enrollment belongs to S4")
    return result["state"]


def pair_range(output):
    match = re.search(r"tested with T3 (\d+\.\d+\.\d+)\.\.(\d+\.\d+\.\d+)", output)
    return (match[1], match[2]) if match else None


def pair_version(output):
    match = re.match(r"t3-steward v?(\S+)", output)
    return match[1] if match else None


def numeric_version(value):
    if not isinstance(value, str) or not re.fullmatch(r"\d+\.\d+\.\d+", value):
        raise StepFailed("T3 version is not a stable numeric version")
    return tuple(int(n) for n in value.split("."))


def verify_pair_binary(binary):
    observed = run([str(binary), "version"], timeout=15)
    bounds = pair_range(observed["output"])
    if not observed["ok"] or not bounds or pair_version(observed["output"]) != COMPONENTS["t3-steward"]["version"]:
        raise StepFailed("steward binary does not prove the pinned version and T3 compatibility")
    if "t3" in COMPONENTS:
        spec = COMPONENTS["t3"]
        wanted = spec["compatibility"]
        if (bounds != (wanted["min"], wanted["max"])
                or not numeric_version(bounds[0]) <= numeric_version(spec["version"]) <= numeric_version(bounds[1])):
            raise StepFailed("steward binary does not prove the frozen T3 compatibility range")
    return bounds


def steward_session_hook(spec):
    """Only the explicitly pinned hook is managed; legacy releases are unchanged."""
    return HOME / ".config/opencode/plugins/steward-session.js"


def session_hook_converged(spec):
    expected = spec.get("session_hook_sha256")
    if not expected:
        return True
    path = steward_session_hook(spec)
    return not path.is_symlink() and path.is_file() and sha256_file(path) == expected


def stage_session_hook(tar, stage, spec):
    expected = spec.get("session_hook_sha256")
    if not expected:
        return None
    member = tar.getmember("packaging/opencode/steward-session.js")
    if not member.isfile() or member.size > 64 * 1024:
        raise StepFailed("invalid steward session hook member")
    body = tar.extractfile(member).read()
    if hashlib.sha256(body).hexdigest() != expected:
        raise StepFailed("steward session hook checksum mismatch")
    candidate = stage / "steward-session.js"
    candidate.write_bytes(body)
    candidate.chmod(0o644)
    return candidate


def apply_pinned_steward(spec, before, actions):
    binary = HOME / spec["path"]
    if before["version"] and before["version"].removeprefix("v") == spec["version"]:
        verify_pair_binary(binary)
        if before["service_active"] and session_hook_converged(spec):
            return "converged"
    if DRY_RUN:
        actions.append("would install and verify pinned steward " + spec["version"])
        return "updated"
    # This is a pinned artifact install, not the self-update command or timer.
    import platform
    import tarfile
    import tempfile
    import urllib.request
    arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    system = platform.system().lower()
    if not arch or system not in {"linux", "darwin"}:
        raise StepFailed("no steward release artifact for this platform")
    archive = f"t3-steward_{spec['version']}_{system}_{arch}.tar.gz"
    base = f"https://github.com/{spec['repository']}/releases/download/v{spec['version']}"
    with tempfile.TemporaryDirectory(prefix="upkeeper-steward-") as directory:
        stage = Path(directory)
        try:
            with urllib.request.urlopen(base + "/checksums.txt", timeout=60) as response:
                checksums = response.read(1024 * 1024).decode()
            expected = [line.split()[0] for line in checksums.splitlines()
                        if len(line.split()) == 2 and line.split()[1].lstrip("*") == archive]
            if len(expected) != 1:
                raise StepFailed("release checksum entry missing or ambiguous")
            with urllib.request.urlopen(base + "/" + archive, timeout=120) as response:
                payload = response.read(128 * 1024 * 1024 + 1)
            if len(payload) > 128 * 1024 * 1024 or hashlib.sha256(payload).hexdigest() != expected[0]:
                raise StepFailed("steward release archive checksum mismatch")
            (stage / archive).write_bytes(payload)
            with tarfile.open(stage / archive) as tar:
                member = tar.getmember("t3-steward")
                if not member.isfile() or member.size > 128 * 1024 * 1024:
                    raise StepFailed("invalid steward archive member")
                candidate = stage / "t3-steward"
                candidate.write_bytes(tar.extractfile(member).read())
                candidate.chmod(0o755)
                hook_candidate = stage_session_hook(tar, stage, spec)
            bounds = verify_pair_binary(candidate)
            current_t3 = observe_t3(COMPONENTS.get("t3", {"service": "t3code.service"}))
            # The compiled binary also reports a T3 program deleted from under
            # its unit; the reference path names what it observed and how.
            if not current_t3["check_ok"] or not current_t3["served"]:
                cause = ("t3-steward check failed" if not current_t3["check_ok"]
                         else "t3-steward check reported no T3 server version")
                raise StepFailed(
                    f"running T3 version could not be observed ({cause}); no safe pair "
                    "transition. Check that T3 is serving and that the steward reaches it, "
                    "then retry")
            if not numeric_version(bounds[0]) <= numeric_version(current_t3["served"]) <= numeric_version(bounds[1]):
                raise StepFailed(
                    f"running T3 {current_t3['served']} is outside the candidate steward's "
                    f"T3 range {bounds[0]}..{bounds[1]}; no safe pair transition. Install a T3 "
                    f"inside that range or publish a steward tested with T3 "
                    f"{current_t3['served']}, then retry")
        except StepFailed:
            raise
        except Exception as exc:
            raise StepFailed("steward artifact fetch failed: " + type(exc).__name__) from None
        backup = HOME / ".local/state/dev-fleet/binary-rollbacks" / RUN_ID
        backup.parent.mkdir(parents=True, exist_ok=True)
        hook_path = steward_session_hook(spec)
        hook_before = None
        hook_mode = None
        hook_installed = False
        if hook_candidate is not None:
            if hook_path.is_symlink() or (hook_path.exists() and not hook_path.is_file()):
                raise StepFailed("session hook destination is not a regular file")
            if hook_path.exists():
                hook_before = hook_path.read_bytes()
                hook_mode = hook_path.stat().st_mode & 0o777
        existed = binary.exists()
        if existed:
            shutil.copy2(binary, backup)
        try:
            binary.parent.mkdir(parents=True, exist_ok=True)
            temporary = binary.with_name(binary.name + ".upkeeper-tmp")
            if temporary.exists() or temporary.is_symlink():
                raise StepFailed("steward staging path already exists")
            shutil.copy2(candidate, temporary)
            os.replace(temporary, binary)
            if hook_candidate is not None:
                hook_path.parent.mkdir(parents=True, exist_ok=True)
                hook_tmp = hook_path.with_name(hook_path.name + ".upkeeper-tmp")
                with hook_tmp.open("xb") as stream:
                    stream.write(hook_candidate.read_bytes())
                hook_tmp.chmod(0o644)
                os.replace(hook_tmp, hook_path)
                hook_installed = True
            must(["systemctl", "--user", "restart", spec["service"]], what="restart pinned steward")
            verify_pair_binary(binary)
            if not unit_active(spec["service"]):
                raise StepFailed("pinned steward service is inactive")
        except Exception:
            hook_restore_failed = False
            if hook_installed:
                try:
                    if hook_before is None:
                        hook_path.unlink(missing_ok=True)
                    else:
                        hook_path.write_bytes(hook_before)
                        hook_path.chmod(hook_mode)
                except OSError:
                    hook_restore_failed = True
            if not existed:
                binary.unlink(missing_ok=True)
                raise StepFailed("new steward failed; removed unverified binary") from None
            try:
                shutil.copy2(backup, binary.with_name(binary.name + ".upkeeper-restore"))
                os.replace(binary.with_name(binary.name + ".upkeeper-restore"), binary)
                must(["systemctl", "--user", "restart", spec["service"]], what="restore steward")
                if sha256_file(binary) != before["binary_sha256"] or not unit_active(spec["service"]):
                    raise StepFailed("previous steward did not verify")
            except Exception:
                raise StepFailed("rollback failed: steward restoration failed") from None
            if hook_restore_failed:
                raise StepFailed("steward restored but session hook restoration failed") from None
            raise RolledBack("restored previous steward binary, service and session hook") from None
    actions.append("installed verified pinned steward " + spec["version"])
    if spec.get("session_hook_sha256"):
        actions.append("installed verified OpenCode caller hook; loaded by new OpenCode servers")
    try:
        restart_persistent_worker(binary, actions, run=run, must=must, unit_loaded=unit_loaded,
                                  unit_active=unit_active, dry_run=DRY_RUN)
    except WorkerRestartFailed as exc:
        raise StepFailed(str(exc)) from None
    return "updated"


def refuse_controlled_t3(spec):
    if "artifact" in spec or os.path.lexists(HOME / ".local/state/dev-fleet/t3-controlled"):
        raise StepFailed("controlled T3 operations require the Go agent")
    prefix = run(["npm", "prefix", "-g"], timeout=30)
    path = Path(prefix["output"].strip())
    if path.is_absolute():
        package = path / "lib/node_modules/t3/package.json"
        try:
            metadata = json.loads(package.read_text())
            if "upkeeperControlledBuild" in metadata:
                raise StepFailed("controlled T3 operations require the Go agent")
        except (ValueError, OSError):
            if package.exists():
                raise StepFailed("ambiguous installed T3 provenance") from None


def observe_t3(spec):
    refuse_controlled_t3(spec)
    package = run(["npm", "ls", "-g", "--depth=0", "--json", "t3"], timeout=30)
    try:
        installed = json.loads(package["output"])["dependencies"]["t3"]["version"] if package["ok"] else None
    except (ValueError, KeyError, TypeError):
        installed = None
    check = run([str(HOME / COMPONENTS["t3-steward"]["path"]), "check"], timeout=30)
    served = re.search(r"T3 server (\d+\.\d+\.\d+) ", check["output"])
    threads = re.search(r"shell snapshot: \d+ threads, (\d+) running", check["output"])
    return {"installed": installed, "served": served[1] if served else None,
            "running_threads": int(threads[1]) if threads else None,
            "service_active": unit_active(spec["service"]), "check_ok": check["ok"]}


def apply_t3(spec, before, actions):
    refuse_controlled_t3(spec)
    # Service seeding never overwrites an existing unit or its credentials/bind address.
    # Only start a seed after the installed package and steward have been verified.
    seeded = False
    if before["installed"] == spec["version"] and not before["service_active"]:
        if not DRY_RUN:
            verify_pair_binary(HOME / COMPONENTS["t3-steward"]["path"])
        seeded = seed_t3_service(HOME, run, must, DRY_RUN, actions)
        if seeded and not DRY_RUN:
            before = observe_t3(spec)
    if before["installed"] == spec["version"] and before["served"] == spec["version"] and before["service_active"]:
        if not DRY_RUN:
            verify_pair_binary(HOME / COMPONENTS["t3-steward"]["path"])
        return "updated" if seeded else "converged"
    if DRY_RUN:
        actions.append("would install t3@" + spec["version"] + "; restart requires a fresh zero-running-thread observation")
        return "updated"
    verify_pair_binary(HOME / COMPONENTS["t3-steward"]["path"])
    # Guard the installation too: npm replaces files a running T3 process may need.
    fresh = observe_t3(spec)
    if not fresh["check_ok"] or fresh["running_threads"] != 0:
        actions.append("T3 change deferred: steward cannot prove zero running threads")
        return "deferred"
    if fresh["installed"] != spec["version"]:
        must(["npm", "install", "-g", "t3@" + spec["version"]], timeout=400, what="install pinned T3")
        if shutil.which("mise", path=ENV["PATH"]):
            must(["mise", "reshim"], what="refresh T3 shim")
    fresh = observe_t3(spec)
    if not fresh["check_ok"] or fresh["running_threads"] != 0:
        actions.append("T3 installed; restart deferred because idle evidence changed")
        return "deferred"
    must(["systemctl", "--user", "restart", spec["service"]], what="restart idle T3")
    after = observe_t3(spec)
    if after["installed"] != spec["version"] or after["served"] != spec["version"] or not after["service_active"]:
        raise StepFailed("T3 did not verify after restart; observe before retry")
    actions.append("installed and serving t3@" + spec["version"])
    return "updated"


# ---- harness registration (read-only) ----------------------------------------------


def harness_report():
    report = {}
    try:
        claude = json.loads((HOME / ".claude.json").read_text(encoding="utf-8"))
        report["claude"] = "huyang" in claude.get("mcpServers", {})
    except (OSError, ValueError):
        report["claude"] = None
    for name, path, needle in (
        ("codex", HOME / ".codex/config.toml", "[mcp_servers.huyang]"),
        ("opencode", HOME / ".config/opencode/opencode.json", '"huyang"'),
    ):
        try:
            report[name] = needle in path.read_text(encoding="utf-8")
        except OSError:
            report[name] = None
    return report


HANDLERS = {
    "omarchy-setup": (observe_omarchy_setup, apply_omarchy_setup),
    "agent-config": (observe_agent_config, apply_agent_config),
    "agent-environment": (observe_release_environment, apply_release_environment),
    "worker-configuration": (observe_worker_configuration, apply_worker_configuration),
    "t3": (observe_t3, apply_t3),
    "huyang": (observe_huyang, apply_huyang),
    "t3-steward": (observe_steward, apply_steward),
}


def main():
    result = {"host": HOST["name"], "run_id": RUN_ID, "dry_run": DRY_RUN, "components": []}
    owner = acquire_lock()
    if owner is not None:
        result["result"] = "lock-contention"
        result["lock_owner"] = owner
        print(json.dumps(result, sort_keys=True))
        return
    worst = "converged"
    rank = ["converged", "updated", "ahead", "skipped", "deferred", "rolled-back", "failed", "rollback-failed"]
    stop = False
    try:
        if PAYLOAD.get("retire_updater"):
            try:
                retirement = retire_updater(HOME, run, must, DRY_RUN)
            except Exception as exc:
                retirement = {"state": "failed", "actions": [], "error": str(exc)}
            result["legacy_updater"] = retirement
            worst = retirement["state"]
            stop = worst in {"failed", "deferred"}
        for name in SELECTED:
            observe, apply = HANDLERS[name]
            entry = {"name": name, "actions": []}
            if stop:
                entry["state"] = "skipped"
                result["components"].append(entry)
                continue
            try:
                entry["before"] = observe(COMPONENTS[name])
                entry["state"] = apply(COMPONENTS[name], entry["before"], entry["actions"])
                if entry["state"] in {"deferred", "rolled-back", "rollback-failed"}:
                    stop = True
            except RolledBack as exc:
                entry["state"] = "rolled-back"
                entry["error"] = str(exc)
                stop = True
            except StepFailed as exc:
                entry["state"] = "rollback-failed" if str(exc).startswith("rollback failed") else "failed"
                entry["error"] = str(exc)
                stop = True
            except Exception as exc:  # keep the report well-formed no matter what
                entry["state"] = "failed"
                entry["error"] = f"{type(exc).__name__}: {exc}"
                stop = True
            try:
                entry["after"] = observe(COMPONENTS[name])
            except Exception as exc:
                entry["after"] = {"error": f"{type(exc).__name__}: {exc}"}
            result["components"].append(entry)
            if rank.index(entry["state"]) > rank.index(worst):
                worst = entry["state"]
        result["harness"] = harness_report()
    finally:
        release_lock()
    result["result"] = worst
    print(json.dumps(result, sort_keys=True))


main()
'''


class PullError(RuntimeError):
    """The pull could not start."""


def _controller(config: FleetConfig) -> str:
    current = local_machine_id_sha256()
    for name, spec in config.hosts.items():
        if spec.machine_id_sha256 == current:
            return name
    return "unknown"


def _stored_files(root: Path, release: Release) -> dict[str, str]:
    return {
        entry["store"]: (root / AGENT_CONFIG_DIR / entry["store"]).read_text(encoding="utf-8")
        for entry in release.components["agent-config"]["files"]
    }


def _agent_program(payload: dict[str, Any]) -> str:
    literal = json.dumps(json.dumps(payload, sort_keys=True))
    # Supply audited stdlib modules in memory; targets never read this repository,
    # and a dry run does not create an import cache or a temporary checkout.
    sources = {m.__name__: inspect.getsource(m) for m in (environment, release_assets, worker_release, t3_lifecycle, huyang_lifecycle, worker_lifecycle)}
    bootstrap = (
        "import sys, types\n"
        "package = types.ModuleType('dev_fleet'); package.__path__ = []\n"
        "sys.modules['dev_fleet'] = package\n"
        f"for name, source in {sources!r}.items():\n"
        "    module = types.ModuleType(name); module.__package__ = 'dev_fleet'\n"
        "    sys.modules[name] = module\n"
        "    exec(compile(source, '<upkeeper-module>', 'exec'), module.__dict__)\n"
    )
    return bootstrap + REMOTE_AGENT.replace("__PAYLOAD__", literal, 1)


def run_host(
    spec: HostSpec,
    program: str,
    *,
    timeout: int,
    local_only: bool = False,
) -> dict[str, Any]:
    is_local = local_machine_id_sha256() == spec.machine_id_sha256
    if local_only and not is_local:
        return {"host": spec.name, "reachable": False, "result": "failed",
                "error": "self identity changed; remote fallback refused"}
    if is_local:
        command = [sys.executable, "-"]
    else:
        command = [
            "ssh",
            "-o",
            "BatchMode=yes",
            "-o",
            "StrictHostKeyChecking=yes",
            "-o",
            "ConnectTimeout=10",
            f"{spec.user}@{spec.address}",
            "--",
            "python3",
            "-",
        ]
    try:
        completed = subprocess.run(
            command, input=program, capture_output=True, check=False, text=True, timeout=timeout
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        return {"host": spec.name, "reachable": False, "result": "failed", "error": type(exc).__name__}
    transport = "local" if is_local else "ssh"
    lines = [line for line in completed.stdout.splitlines() if line.startswith("{")]
    if completed.returncode != 0 or not lines:
        return {
            "host": spec.name,
            "reachable": False,
            "transport": transport,
            "result": "failed",
            "error": "agent-failed",
            "returncode": completed.returncode,
            "stderr": completed.stderr[-2000:],
        }
    try:
        payload = json.loads(lines[-1])
    except json.JSONDecodeError:
        return {"host": spec.name, "reachable": False, "transport": transport, "result": "failed", "error": "invalid-agent-output"}
    payload.update({"reachable": True, "transport": transport})
    return redact(payload)


def check_reachable(spec: HostSpec) -> dict[str, Any] | None:
    """Probe before dispatch; only network unavailability is a quiet skip."""
    if local_machine_id_sha256() == spec.machine_id_sha256:
        return None
    command = ["ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
               "-o", "ConnectTimeout=10", f"{spec.user}@{spec.address}", "--", "true"]
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=15, check=False)
    except subprocess.TimeoutExpired:
        return {"host": spec.name, "reachable": False, "result": "skipped",
                "skipped_unreachable": True, "error": "pre-dispatch connection timeout"}
    except OSError:
        return {"host": spec.name, "reachable": False, "result": "failed",
                "error": "reachability probe could not run"}
    if result.returncode == 0:
        return None
    offline = result.returncode == 255 and any(message in result.stderr for message in (
        "Connection timed out", "No route to host", "Network is unreachable",
        "Connection refused", "Could not resolve hostname",
    ))
    return {"host": spec.name, "reachable": False,
            "result": "skipped" if offline else "failed",
            "skipped_unreachable": offline,
            "error": "unreachable before dispatch" if offline else "SSH preflight failed",
            "stderr": result.stderr[-2000:]}


def exit_code_for(hosts: list[dict[str, Any]]) -> int:
    states = {host.get("result") for host in hosts}
    offline = any(not host.get("reachable") and not host.get("skipped_unreachable")
                  and not host.get("self_managed") for host in hosts)
    if "rollback-failed" in states:
        return EXIT_ROLLBACK
    if "failed" in states or "rolled-back" in states:
        return EXIT_DEPLOYMENT
    if "lock-contention" in states:
        return EXIT_LOCK_CONTENTION
    if offline:
        return EXIT_PARTIAL
    if states & {"ahead", "deferred"}:
        return EXIT_DRIFT
    return EXIT_SUCCESS


def pull_fleet(
    config: FleetConfig,
    release: Release,
    *,
    hosts: list[str],
    components: list[str] | None = None,
    dry_run: bool = False,
    timeout: int = 1800,
    manifest_commit: str | None = None,
    stored_files: dict[str, str] | None = None,
    command: str = "pull",
    self_only: bool = False,
    lock_store: LocalLockStore | None = None,
    record_store: RunRecordStore | None = None,
    runner=run_host,
    skip_unreachable: bool = False,
    reachability=check_reachable,
) -> tuple[dict[str, Any], int]:
    if self_only:
        identity = local_machine_id_sha256()
        if len(hosts) != 1 or config.hosts[hosts[0]].machine_id_sha256 != identity:
            raise PullError("--self may select only this inventory machine")
    selected_components = [name for name in COMPONENT_ORDER if name in release.components and (components is None or name in components)]
    if "t3" in release.components and set(selected_components) & {"t3", "t3-steward"}:
        selected_components = [n for n in COMPONENT_ORDER if n in set(selected_components) | {"t3", "t3-steward"}]
    unknown = set(components or []) - set(COMPONENT_ORDER)
    if unknown:
        raise PullError(f"unknown components: {', '.join(sorted(unknown))}")
    run_id = new_run_id()
    controller = _controller(config)
    locks = lock_store or LocalLockStore(Path.home() / ".local/state/dev-fleet/locks")
    records = record_store or RunRecordStore.default()
    metadata = LockMetadata.new(
        run_id=run_id,
        controller=controller if controller != "unknown" else "controller",
        controller_pid=os.getpid(),
        desired_manifest_commit=manifest_commit or release.sha256(),
        components=selected_components,
    )
    record = RunRecord.new(
        run_id=run_id,
        command=command,
        controller=controller,
        desired_manifest_commit=manifest_commit,
        components=selected_components,
        hosts=hosts,
    )
    if not dry_run:
        records.write(record)
    stored = _stored_files(config.root, release) if stored_files is None else stored_files
    results: list[dict[str, Any]] = []
    runner_options = {"timeout": timeout}
    if self_only:
        runner_options["local_only"] = True
    for name in hosts:
        spec = config.hosts[name]
        if spec.self_managed and command == "scheduled-pull" and not self_only:
            results.append({"host": name, "reachable": False, "result": "skipped",
                            "self_managed": True, "error": "converges through its local timer"})
            continue
        if skip_unreachable:
            preflight = reachability(spec)
            if preflight is not None:
                results.append(preflight)
                continue
        payload = {
            "run_id": run_id,
            "retire_updater": True,
            "allow_downgrade": command == "rollback",
            "dry_run": dry_run,
            "host": {"name": name, "os_id": spec.os_id, "setup_checkout": spec.setup_checkout},
            "components": selected_components,
            "manifest": release.manifest,
            "stored_files": stored,
            "lock": {
                "schema_version": 1,
                "run_id": run_id,
                "controller": metadata.controller,
                "controller_pid": metadata.controller_pid,
                "desired_manifest_commit": metadata.desired_manifest_commit,
                "components": metadata.components,
                "started_at": metadata.started_at,
            },
        }
        if dry_run:
            results.append(runner(spec, _agent_program(payload), **runner_options))
            continue
        try:
            locks.acquire(name, metadata)
        except LockContentionError as exc:
            results.append({"host": name, "reachable": True, "result": "lock-contention", "error": str(exc)})
            continue
        try:
            results.append(runner(spec, _agent_program(payload), **runner_options))
        finally:
            locks.release(name, run_id)
    exit_code = exit_code_for(results)
    report = {
        "schema_version": 1,
        "command": command,
        "run_id": run_id,
        "controller": controller,
        "dry_run": dry_run,
        "self": self_only,
        "release_sha256": release.sha256(),
        "desired_manifest_commit": manifest_commit,
        "exit_code": exit_code,
        "hosts": results,
    }
    if not dry_run:
        final = RunRecord(
            **{
                **record.__dict__,
                "phase": "finished",
                "result": "success" if exit_code == EXIT_SUCCESS else f"exit-{exit_code}",
                "details": {"hosts": results},
            }
        )
        records.write(final)
    return report, exit_code
