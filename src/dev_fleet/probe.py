"""Bounded, read-only host discovery over local Python or strict SSH."""

from __future__ import annotations

import json
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import asdict
from pathlib import Path
from typing import Any

from .config import FleetConfig, HostSpec
from .redaction import redact

REMOTE_PROBE = r"""
import getpass
import hashlib
import json
import os
import platform
import shutil
import subprocess
import sys
from pathlib import Path


def run(arguments, timeout=6, environment=None):
    try:
        completed = subprocess.run(
            arguments,
            capture_output=True,
            check=False,
            env=environment,
            text=True,
            timeout=timeout,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        return {"ok": False, "rc": None, "error": type(exc).__name__}
    output = (completed.stdout or completed.stderr).strip()
    return {"ok": completed.returncode == 0, "rc": completed.returncode, "output": output[:512]}


def first_line(result):
    return result.get("output", "").splitlines()[0] if result.get("output") else None


def os_release():
    values = {}
    try:
        for line in Path("/etc/os-release").read_text(encoding="utf-8").splitlines():
            if "=" in line:
                key, value = line.split("=", 1)
                values[key] = value.strip().strip('"')
    except OSError:
        pass
    return values


def git_info(path):
    directory = Path(path)
    if not directory.exists():
        return {"present": False}
    head = run(["git", "-C", str(directory), "rev-parse", "HEAD"])
    if not head["ok"]:
        return {"present": True, "git": False}
    branch = run(["git", "-C", str(directory), "symbolic-ref", "--short", "-q", "HEAD"])
    status = run(["git", "-C", str(directory), "status", "--porcelain=v1", "--untracked-files=normal"])
    entries = len(status.get("output", "").splitlines()) if status["ok"] else None
    return {
        "present": True,
        "git": True,
        "head": first_line(head),
        "branch": first_line(branch) or "detached",
        "dirty_entries": entries,
    }


def metadata(path):
    candidate = Path(path)
    try:
        stat = candidate.stat()
    except OSError:
        return {"exists": False}
    return {
        "exists": True,
        "kind": "directory" if candidate.is_dir() else "file",
        "mode": format(stat.st_mode & 0o777, "04o"),
        "size": stat.st_size,
    }


def source_access(urls):
    environment = dict(os.environ)
    environment["GIT_TERMINAL_PROMPT"] = "0"
    environment["GIT_SSH_COMMAND"] = (
        "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=5"
    )
    for url in urls:
        result = run(["git", "ls-remote", url, "HEAD"], timeout=10, environment=environment)
        if result["ok"]:
            return {"reachable": True, "via": "ssh" if url.startswith("git@") else "https"}
    return {"reachable": False, "via": None}


def active_bridges():
    values = []
    proc = Path("/proc")
    if not proc.is_dir():
        return values
    for child in proc.iterdir():
        if not child.name.isdigit():
            continue
        try:
            command = (child / "cmdline").read_bytes()
            if b"agent99-bridge" not in command:
                continue
            values.append(
                {
                    "pid": int(child.name),
                    "executable": os.path.realpath(child / "exe"),
                    "cwd": os.path.realpath(child / "cwd"),
                }
            )
        except OSError:
            continue
    return sorted(values, key=lambda item: item["pid"])


def forgejo_access():
    result = run(
        [
            "ssh",
            "-n",
            "-T",
            "-o",
            "BatchMode=yes",
            "-o",
            "StrictHostKeyChecking=yes",
            "-o",
            "ConnectTimeout=5",
            "git@git.ryzhkov.dev",
        ],
        timeout=8,
    )
    message = result.get("output", "").lower()
    return result["ok"] or "authenticated" in message or "successfully" in message


home = Path.home()
release = os_release()
machine_id = None
try:
    machine_id = hashlib.sha256(Path("/etc/machine-id").read_bytes()).hexdigest()
except OSError:
    pass
python_version = platform.python_version()
ansible = run(["ansible", "--version"]) if shutil.which("ansible") else {"ok": False}
systemd = run(["systemctl", "--user", "is-system-running"])
linger = run(["loginctl", "show-user", getpass.getuser(), "-p", "Linger", "--value"])
service = run(
    [
        "systemctl",
        "--user",
        "show",
        "t3-steward.service",
        "-p",
        "LoadState",
        "-p",
        "ActiveState",
        "-p",
        "SubState",
        "-p",
        "UnitFileState",
    ]
)
service_values = {}
for line in service.get("output", "").splitlines():
    if "=" in line:
        key, value = line.split("=", 1)
        service_values[key] = value

components = {
    "t3-steward": {
        "command": shutil.which("t3-steward"),
        "version": first_line(run(["t3-steward", "version"])) if shutil.which("t3-steward") else None,
        "service": service_values,
        "config": metadata(home / ".config/t3-steward"),
        "state": metadata(home / ".local/state/t3-steward/state.db"),
    },
    "agent99": {
        "checkout": git_info(home / ".local/share/nvim/lazy/agent99"),
        "active_bridges": active_bridges(),
    },
    "nvim-configuration": {"checkout": git_info(home / ".config/nvim")},
    "homelab-cli": {
        "checkout": git_info(home / "Work/homelab-cli"),
        "commands": {
            name: shutil.which(name)
            for name in ("feed", "t3-backlog")
        },
    },
    "agent-config": {
        "config": metadata(home / ".config/agents"),
        "codex_skills": metadata(home / ".codex/skills"),
        "claude_skills": metadata(home / ".claude/skills"),
    },
}
sources = {
    "t3-steward": source_access(
        ["git@github.com:iryzhkov/t3-steward.git", "https://github.com/iryzhkov/t3-steward.git"]
    ),
    "agent99": source_access(
        ["git@github.com:iryzhkov/agent99.git", "https://github.com/iryzhkov/agent99.git"]
    ),
    "nvim-configuration": source_access(
        [
            "git@github.com:iryzhkov/nvim-configuration.git",
            "https://github.com/iryzhkov/nvim-configuration.git",
        ]
    ),
    "homelab-cli": source_access(
        ["git@github.com:iryzhkov/homelab-cli.git", "https://github.com/iryzhkov/homelab-cli.git"]
    ),
}
locks_root = home / ".local/state/dev-fleet/locks"
locks = []
if locks_root.is_dir():
    locks = sorted(
        entry.name
        for entry in locks_root.iterdir()
        if entry.is_dir() and entry.name.endswith(".lock") and "/" not in entry.name
    )
print(
    json.dumps(
        {
            "hostname": platform.node(),
            "user": getpass.getuser(),
            "architecture": platform.machine(),
            "machine_id_sha256": machine_id,
            "os": {"id": release.get("ID"), "version": release.get("VERSION_ID", "unknown")},
            "python": {"version": python_version, "executable": sys.executable},
            "ansible": {"available": bool(shutil.which("ansible")), "version": first_line(ansible)},
            "systemd_user": {"state": first_line(systemd), "linger": first_line(linger)},
            "disk": {"home_free_bytes": shutil.disk_usage(home).free},
            "writable_existing_roots": {
                str(path.relative_to(home)): path.is_dir() and os.access(path, os.W_OK)
                for path in (home / ".local", home / ".local/bin", home / ".local/state")
            },
            "forgejo_access": forgejo_access(),
            "source_access": sources,
            "components": components,
            "locks": locks,
        },
        sort_keys=True,
    )
)
"""


class ProbeError(RuntimeError):
    """The target could not return a valid probe result."""


def local_machine_id_sha256() -> str | None:
    try:
        import hashlib

        return hashlib.sha256(Path("/etc/machine-id").read_bytes()).hexdigest()
    except OSError:
        return None


def _decode(completed: subprocess.CompletedProcess[str], host: str) -> dict[str, Any]:
    if completed.returncode != 0:
        return {
            "reachable": False,
            "error": "probe-failed",
            "returncode": completed.returncode,
        }
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError:
        return {"reachable": False, "error": "invalid-probe-output"}
    if not isinstance(payload, dict):
        return {"reachable": False, "error": "invalid-probe-output"}
    return {"reachable": True, **redact(payload)}


def probe_host(spec: HostSpec, timeout: int) -> dict[str, Any]:
    is_local = local_machine_id_sha256() == spec.machine_id_sha256
    if is_local:
        command = [sys.executable, "-c", REMOTE_PROBE]
    else:
        destination = f"{spec.user}@{spec.address}"
        command = [
            "ssh",
            "-o",
            "BatchMode=yes",
            "-o",
            "StrictHostKeyChecking=yes",
            "-o",
            f"ConnectTimeout={min(timeout, 10)}",
            destination,
            "--",
            "python3",
            "-",
        ]
    try:
        completed = subprocess.run(
            command,
            input=None if is_local else REMOTE_PROBE,
            capture_output=True,
            check=False,
            text=True,
            timeout=timeout,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        return {"reachable": False, "error": type(exc).__name__}
    result = _decode(completed, spec.name)
    result["transport"] = "local" if is_local else "ssh"
    return result


def probe_fleet(config: FleetConfig, selected: list[str], timeout: int = 20) -> dict[str, Any]:
    if timeout < 5 or timeout > 60:
        raise ValueError("timeout must be between 5 and 60 seconds")
    observations: dict[str, Any] = {}
    workers = min(4, len(selected))
    with ThreadPoolExecutor(max_workers=workers) as executor:
        futures = {
            executor.submit(probe_host, config.hosts[name], timeout): name for name in selected
        }
        for future in as_completed(futures):
            name = futures[future]
            try:
                observations[name] = future.result()
            except Exception as exc:  # defensive isolation per host
                observations[name] = {"reachable": False, "error": type(exc).__name__}
    return {name: observations[name] for name in selected}


def inventory_view(spec: HostSpec) -> dict[str, Any]:
    return asdict(spec)
