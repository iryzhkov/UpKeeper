"""Command-line interface. Phase 1 exposes read-only operations only."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

from .config import load_config
from .constants import EXIT_PREFLIGHT
from .probe import probe_fleet
from .redaction import redact, redact_text
from .reporting import build_doctor_report, build_status_report
from .validation import ValidationError, parse_host_selector


def _root() -> Path:
    """Resolve status data explicitly for source checkouts and installed UpKeeper."""
    override = os.environ.get("DEV_FLEET_CONFIG_ROOT")
    candidates = []
    if override:
        configured = Path(override)
        if not configured.is_absolute():
            raise ValidationError("DEV_FLEET_CONFIG_ROOT must be an absolute path")
        candidates.append(configured)
    candidates.extend(
        [
            Path(__file__).resolve().parents[2],
            Path.home() / ".config" / "dev-fleet",
        ]
    )
    for candidate in candidates:
        resolved = candidate.resolve()
        if (
            (resolved / "inventory" / "hosts.yml").is_file()
            and (resolved / "desired" / "components.yml").is_file()
        ):
            return resolved
    raise ValidationError(
        "fleet configuration unavailable; install inventory/hosts.yml and "
        "desired/components.yml under ~/.config/dev-fleet or set DEV_FLEET_CONFIG_ROOT"
    )


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="fleet",
        description="Read-only developer fleet discovery (Phase 1)",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    for name in ("doctor", "status"):
        command = subparsers.add_parser(name)
        command.add_argument("--json", action="store_true", dest="as_json")
        command.add_argument(
            "--limit",
            metavar="HOST[,HOST...]",
            help="exact canonical inventory names; no patterns or arbitrary expressions",
        )
        command.add_argument("--timeout", type=int, default=20, choices=range(5, 61))
    return parser


def _print_json(report: dict[str, Any]) -> None:
    print(json.dumps(redact(report), indent=2, sort_keys=True))


def _print_doctor(report: dict[str, Any]) -> None:
    print(
        f"run {report['run_id']} controller={report['controller']} "
        f"doctor={report['result']} exit={report['exit_code']}"
    )
    for host in report["hosts"]:
        if not host["reachable"]:
            print(f"{host['name']}: offline ({host.get('error', 'unreachable')})")
            continue
        failed = host.get("failed_checks", [])
        suffix = f" failed={','.join(failed)}" if failed else ""
        print(f"{host['name']}: {host['result']}{suffix}")


def _print_status(report: dict[str, Any]) -> None:
    print(
        f"run {report['run_id']} controller={report['controller']} "
        f"status={report['result']} exit={report['exit_code']}"
    )
    for host in report["hosts"]:
        if not host["reachable"]:
            print(f"{host['name']}: offline ({host.get('error', 'unreachable')})")
            continue
        summaries = ", ".join(
            f"{component['name']}={component['state']}" for component in host["components"]
        )
        print(f"{host['name']}: {host['result']} [{summaries}]")


def main(argv: list[str] | None = None) -> int:
    arguments = _parser().parse_args(argv)
    try:
        config = load_config(_root())
        selected = parse_host_selector(arguments.limit, set(config.hosts))
        observations = probe_fleet(config, selected, timeout=arguments.timeout)
        if arguments.command == "doctor":
            report, exit_code = build_doctor_report(config, observations)
            printer = _print_doctor
        else:
            report, exit_code = build_status_report(config, observations)
            printer = _print_status
        if arguments.as_json:
            _print_json(report)
        else:
            printer(report)
        return exit_code
    except (ValidationError, ValueError) as exc:
        message = redact_text(str(exc))
        if getattr(arguments, "as_json", False):
            _print_json(
                {
                    "schema_version": 1,
                    "command": getattr(arguments, "command", None),
                    "result": "invalid-input",
                    "exit_code": EXIT_PREFLIGHT,
                    "error": message,
                }
            )
        else:
            print(f"fleet: {message}", file=sys.stderr)
        return EXIT_PREFLIGHT


if __name__ == "__main__":
    raise SystemExit(main())
