"""Build stable doctor and status documents from raw probes."""

from __future__ import annotations

import re
import subprocess
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from .config import FleetConfig
from .constants import EXIT_DRIFT, EXIT_PARTIAL, EXIT_PREFLIGHT, EXIT_SUCCESS, SCHEMA_VERSION
from .probe import local_machine_id_sha256
from .run_ids import new_run_id

T3_COMMIT_RE = re.compile(r"commit ([0-9a-f]{7,40})")


def _now() -> str:
    return datetime.now(UTC).isoformat().replace("+00:00", "Z")


def _manifest_commit(root: Path) -> str | None:
    try:
        completed = subprocess.run(
            ["git", "-C", str(root), "rev-parse", "HEAD"],
            capture_output=True,
            check=False,
            text=True,
            timeout=3,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    value = completed.stdout.strip()
    return value if completed.returncode == 0 and len(value) == 40 else None


def _controller(config: FleetConfig) -> str:
    current = local_machine_id_sha256()
    for name, spec in config.hosts.items():
        if spec.machine_id_sha256 == current:
            return name
    return "unknown"


def _base(command: str, config: FleetConfig) -> dict[str, Any]:
    return {
        "schema_version": SCHEMA_VERSION,
        "command": command,
        "run_id": new_run_id(),
        "generated_at": _now(),
        "controller": _controller(config),
        "desired_manifest_commit": _manifest_commit(config.root),
    }


def _check(name: str, passed: bool, actual: Any, expected: Any = None) -> dict[str, Any]:
    value = {"name": name, "passed": bool(passed), "actual": actual}
    if expected is not None:
        value["expected"] = expected
    return value


def build_doctor_report(
    config: FleetConfig, observations: dict[str, dict[str, Any]]
) -> tuple[dict[str, Any], int]:
    report = _base("doctor", config)
    hosts: list[dict[str, Any]] = []
    any_offline = False
    any_failed = False

    for name, observed in observations.items():
        spec = config.hosts[name]
        entry: dict[str, Any] = {
            "name": name,
            "address": spec.address,
            "transport": observed.get("transport"),
            "reachable": observed.get("reachable", False),
            "checks": [],
        }
        if not observed.get("reachable"):
            any_offline = True
            entry["result"] = "offline"
            entry["error"] = observed.get("error", "unreachable")
            hosts.append(entry)
            continue

        checks = [
            _check(
                "canonical-hostname",
                observed.get("hostname") == spec.canonical_hostname,
                observed.get("hostname"),
                spec.canonical_hostname,
            ),
            _check(
                "machine-identity",
                observed.get("machine_id_sha256") == spec.machine_id_sha256,
                observed.get("machine_id_sha256"),
                spec.machine_id_sha256,
            ),
            _check("user", observed.get("user") == spec.user, observed.get("user"), spec.user),
            _check(
                "architecture",
                observed.get("architecture") == spec.architecture,
                observed.get("architecture"),
                spec.architecture,
            ),
            _check(
                "os",
                observed.get("os", {}).get("id") == spec.os_id,
                observed.get("os"),
                {"id": spec.os_id, "version": spec.os_version},
            ),
            _check(
                "python", bool(observed.get("python", {}).get("version")), observed.get("python")
            ),
            _check(
                "ansible-controller",
                observed.get("ansible", {}).get("available") is True,
                observed.get("ansible"),
            ),
            _check(
                "systemd-user",
                observed.get("systemd_user", {}).get("state") == "running",
                observed.get("systemd_user", {}).get("state"),
                "running",
            ),
            _check(
                "linger",
                observed.get("systemd_user", {}).get("linger") == "yes",
                observed.get("systemd_user", {}).get("linger"),
                "yes",
            ),
            _check(
                "forgejo-access",
                observed.get("forgejo_access") is True,
                observed.get("forgejo_access"),
                True,
            ),
            _check(
                "stable-address", spec.stable_address_verified, spec.stable_address_verified, True
            ),
        ]
        for component, access in observed.get("source_access", {}).items():
            checks.append(
                _check(f"source-access:{component}", access.get("reachable") is True, access, True)
            )
        entry["checks"] = checks
        entry["locks"] = observed.get("locks", [])
        failed = [check["name"] for check in checks if not check["passed"]]
        entry["result"] = "failed" if failed else "healthy"
        entry["failed_checks"] = failed
        if failed:
            any_failed = True
        hosts.append(entry)

    if any_offline:
        result, exit_code = "partial", EXIT_PARTIAL
    elif any_failed:
        result, exit_code = "failed", EXIT_PREFLIGHT
    else:
        result, exit_code = "healthy", EXIT_SUCCESS
    report.update({"result": result, "exit_code": exit_code, "hosts": hosts})
    return report, exit_code


def _component_status(
    name: str, desired: dict[str, Any], observed: dict[str, Any]
) -> dict[str, Any]:
    pin = desired.get("pin")
    desired_commit = pin.get("value") if isinstance(pin, dict) else None
    installed_commit = None
    installed_version = None
    activated: Any = None
    running: Any = None

    if name == "t3-steward":
        version_line = observed.get("version")
        installed_version = version_line
        if isinstance(version_line, str):
            match = T3_COMMIT_RE.search(version_line)
            installed_commit = match.group(1) if match else None
        service = observed.get("service", {})
        activated = {
            "service_state": service.get("ActiveState"),
            "unit_state": service.get("UnitFileState"),
        }
        running = {"service_state": service.get("ActiveState"), "commit": None}
    elif name in {"agent99", "nvim-configuration", "homelab-cli"}:
        checkout = observed.get("checkout", {})
        installed_commit = checkout.get("head")
        activated = {"checkout_present": checkout.get("present", False)}
        if name == "agent99":
            bridges = observed.get("active_bridges", [])
            running = {"active_processes": len(bridges), "commit": None}
        else:
            running = None
    else:
        activated = {"config_present": observed.get("config", {}).get("exists", False)}

    if desired_commit is None:
        state = "decision-required"
    elif installed_commit is None:
        state = "unknown"
    elif desired_commit == installed_commit or (
        len(installed_commit) >= 7 and desired_commit.startswith(installed_commit)
    ):
        state = "converged"
    else:
        state = "drift"

    return {
        "name": name,
        "state": state,
        "desired": {"commit": desired_commit, "version": pin.get("version") if pin else None},
        "installed": {"commit": installed_commit, "version": installed_version},
        "activated": activated,
        "running": running,
        "dirty_entries": observed.get("checkout", {}).get("dirty_entries"),
    }


def build_status_report(
    config: FleetConfig, observations: dict[str, dict[str, Any]]
) -> tuple[dict[str, Any], int]:
    report = _base("status", config)
    hosts: list[dict[str, Any]] = []
    any_offline = False
    any_drift = False

    for name, observed in observations.items():
        if not observed.get("reachable"):
            any_offline = True
            hosts.append(
                {
                    "name": name,
                    "reachable": False,
                    "result": "offline",
                    "error": observed.get("error", "unreachable"),
                    "components": [],
                    "locks": [],
                }
            )
            continue

        observed_components = observed.get("components", {})
        components = [
            _component_status(component_name, desired, observed_components.get(component_name, {}))
            for component_name, desired in sorted(config.components.items())
        ]
        host_drift = any(item["state"] != "converged" for item in components)
        any_drift = any_drift or host_drift
        hosts.append(
            {
                "name": name,
                "reachable": True,
                "result": "drift" if host_drift else "converged",
                "identity": {
                    "hostname": observed.get("hostname"),
                    "user": observed.get("user"),
                    "architecture": observed.get("architecture"),
                    "machine_id_sha256": observed.get("machine_id_sha256"),
                },
                "components": components,
                "locks": observed.get("locks", []),
            }
        )

    if any_offline:
        result, exit_code = "partial", EXIT_PARTIAL
    elif any_drift:
        result, exit_code = "drift", EXIT_DRIFT
    else:
        result, exit_code = "converged", EXIT_SUCCESS
    report.update({"result": result, "exit_code": exit_code, "hosts": hosts})
    return report, exit_code
