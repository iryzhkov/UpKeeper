"""`upkeeper pull`: converge selected hosts on the committed release manifest.

The controller never streams shell to a target. It sends one fixed standard-library
Python program on stdin, the same transport `fleet doctor` uses, with the validated
release embedded as a JSON literal. On the target that program observes each
component, changes only what differs, verifies the result, and rolls a failed
Huyang build back to the commit it found. Every host is locked on both sides for
the duration and the controller writes a redacted run record.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

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
from pathlib import Path

PAYLOAD = json.loads(__PAYLOAD__)
HOME = Path.home()
HOST = PAYLOAD["host"]
RUN_ID = PAYLOAD["run_id"]
DRY_RUN = PAYLOAD["dry_run"]
COMPONENTS = PAYLOAD["manifest"]["components"]
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


def acquire_lock():
    LOCK_DIR.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    try:
        LOCK_DIR.mkdir(mode=0o700)
    except FileExistsError:
        owner = None
        try:
            owner = json.loads((LOCK_DIR / "owner.json").read_text())
        except (OSError, ValueError):
            pass
        return owner or {"run_id": "unknown"}
    (LOCK_DIR / "owner.json").write_text(
        json.dumps(PAYLOAD["lock"], sort_keys=True) + "\n", encoding="utf-8"
    )
    return None


def release_lock():
    try:
        (LOCK_DIR / "owner.json").unlink()
        LOCK_DIR.rmdir()
    except OSError:
        pass


# ---- component: omarchy-setup ---------------------------------------------------


def managed_file_pairs(checkout):
    pairs = [(checkout / "config/claude/CLAUDE.md", HOME / ".claude/omarchy-setup/CLAUDE.md")]
    skills = checkout / "config/claude/skills"
    if skills.is_dir():
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
    if shutil.which("omarchy") and (checkout / "run.sh").exists():
        argv = ["bash", str(checkout / "run.sh"), "--yes", "--skip-secrets"]
        for module in modules:
            argv += ["--only", module]
        must(argv, timeout=900, what="omarchy-setup run.sh")
        return
    # A host without Omarchy (homelab) cannot use run.sh; drive the modules the
    # way bin/t3-update does, with the same environment.
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
        raise StepFailed(f"{checkout} is not a git checkout; bootstrap Huyang by hand first")
    if before["dirty"]:
        raise StepFailed(f"{checkout} has local changes; refusing to move it")
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
    if before["version"] == spec["version"] and before["service_active"]:
        return "converged"
    if DRY_RUN:
        actions.append(f"would run t3-update to move {before['version']} to {spec['version']}")
        return "updated"
    must(["systemctl", "--user", "start", "t3-update.service"], timeout=400, what="t3-update")
    actions.append("ran t3-update.service")
    after = observe_steward(spec)
    actions.append(f"installed {after['version']}")
    if not after["service_active"]:
        raise StepFailed(f"{spec['service']} is not active after t3-update")
    if after["version"] == spec["version"]:
        return "updated"
    try:
        if after["version"] and version_tuple(after["version"]) > version_tuple(spec["version"]):
            actions.append("t3-update chose a newer release than the manifest")
            return "ahead"
    except ValueError:
        pass
    raise StepFailed(f"t3-update left {after['version']}, wanted {spec['version']}")


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
    rank = ["converged", "updated", "ahead", "skipped", "rolled-back", "failed", "rollback-failed"]
    stop = False
    try:
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
    return REMOTE_AGENT.replace("__PAYLOAD__", literal, 1)


def run_host(
    spec: HostSpec,
    program: str,
    *,
    timeout: int,
) -> dict[str, Any]:
    is_local = local_machine_id_sha256() == spec.machine_id_sha256
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


def exit_code_for(hosts: list[dict[str, Any]]) -> int:
    states = {host.get("result") for host in hosts}
    offline = any(not host.get("reachable") for host in hosts)
    if "rollback-failed" in states:
        return EXIT_ROLLBACK
    if "failed" in states or "rolled-back" in states:
        return EXIT_DEPLOYMENT
    if "lock-contention" in states:
        return EXIT_LOCK_CONTENTION
    if offline:
        return EXIT_PARTIAL
    if "ahead" in states:
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
    lock_store: LocalLockStore | None = None,
    record_store: RunRecordStore | None = None,
    runner=run_host,
) -> tuple[dict[str, Any], int]:
    selected_components = [name for name in COMPONENT_ORDER if components is None or name in components]
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
        command="pull",
        controller=controller,
        desired_manifest_commit=manifest_commit,
        components=selected_components,
        hosts=hosts,
    )
    if not dry_run:
        records.write(record)
    stored = _stored_files(config.root, release)
    results: list[dict[str, Any]] = []
    for name in hosts:
        spec = config.hosts[name]
        payload = {
            "run_id": run_id,
            "dry_run": dry_run,
            "host": {"name": name, "os_id": spec.os_id},
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
            results.append(runner(spec, _agent_program(payload), timeout=timeout))
            continue
        try:
            locks.acquire(name, metadata)
        except LockContentionError as exc:
            results.append({"host": name, "reachable": True, "result": "lock-contention", "error": str(exc)})
            continue
        try:
            results.append(runner(spec, _agent_program(payload), timeout=timeout))
        finally:
            locks.release(name, run_id)
    exit_code = exit_code_for(results)
    report = {
        "schema_version": 1,
        "command": "pull",
        "run_id": run_id,
        "controller": controller,
        "dry_run": dry_run,
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
