"""Command-line interface.

`doctor` and `status` are the read-only Phase 1 commands. `push` and `pull` are the
UpKeeper release commands: push captures this machine as the release manifest and
publishes it, pull converges named hosts on that manifest.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .config import load_config
from .constants import EXIT_PREFLIGHT
from .probe import probe_fleet
from .pull import PullError, pull_fleet
from .push import PushError, configured_remotes, push_release
from .redaction import redact, redact_text
from .release import COMPONENT_ORDER, CaptureError, load_release
from .reporting import _controller, _manifest_commit, build_doctor_report, build_status_report
from .validation import ValidationError, parse_host_selector, validate_host_name


def _root() -> Path:
    return Path(__file__).resolve().parents[2]


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog=Path(sys.argv[0]).name if sys.argv else "upkeeper",
        description="Developer fleet discovery (doctor, status) and UpKeeper releases (push, pull)",
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

    push = subparsers.add_parser(
        "push", help="capture this machine as the release manifest, commit it and push the branch"
    )
    push.add_argument(
        "remotes", nargs="*", metavar="REMOTE", help="git remotes to push to (default: all)"
    )
    push.add_argument("--no-commit", action="store_true", help="write the manifest only")
    push.add_argument("--no-push", action="store_true", help="commit but do not push")
    push.add_argument("--json", action="store_true", dest="as_json")

    pull = subparsers.add_parser("pull", help="converge hosts on the committed release manifest")
    group = pull.add_mutually_exclusive_group(required=True)
    group.add_argument("--hosts", nargs="+", metavar="HOST", help="exact canonical inventory names")
    group.add_argument("--all", action="store_true", help="every inventory host")
    pull.add_argument(
        "--components", nargs="+", choices=COMPONENT_ORDER, help="limit to these components"
    )
    pull.add_argument("--dry-run", action="store_true", help="observe and report; change nothing")
    pull.add_argument("--timeout", type=int, default=1800, help="seconds per host")
    pull.add_argument("--json", action="store_true", dest="as_json")
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


def _print_push(report: dict[str, Any]) -> None:
    for line in report["summary"]:
        print(f"  {line}")
    if report.get("committed"):
        print(f"committed {report['committed'][:12]}")
    elif "head" in report:
        print("nothing new to commit")
    for pushed in report["pushed"]:
        print(f"pushed {pushed}")


def _print_pull(report: dict[str, Any]) -> None:
    mode = " (dry run)" if report["dry_run"] else ""
    print(
        f"run {report['run_id']} controller={report['controller']} "
        f"exit={report['exit_code']}{mode}"
    )
    for host in report["hosts"]:
        if not host.get("reachable"):
            print(f"{host['host']}: offline ({host.get('error', 'unreachable')})")
            if host.get("stderr"):
                print(f"  {host['stderr'].strip().splitlines()[-1]}")
            continue
        print(f"{host['host']}: {host['result']}")
        for component in host.get("components", []):
            print(f"  {component['name']}: {component['state']}")
            for action in component.get("actions", []):
                print(f"    - {action}")
            if component.get("error"):
                print(f"    ! {component['error']}")
        if host.get("harness"):
            missing = [name for name, ok in host["harness"].items() if ok is not True]
            if missing:
                print(f"  harness without huyang registration: {', '.join(missing)}")


def _run_push(arguments: argparse.Namespace) -> int:
    root = _root()
    config = load_config(root)
    remotes = arguments.remotes or configured_remotes(root)
    report = push_release(
        root,
        controller=_controller(config),
        remotes=remotes,
        commit=not arguments.no_commit,
        push=not (arguments.no_commit or arguments.no_push),
    )
    if arguments.as_json:
        _print_json(report)
    else:
        _print_push(report)
    return 0


def _run_pull(arguments: argparse.Namespace) -> int:
    root = _root()
    config = load_config(root)
    release = load_release(root)
    if arguments.all:
        hosts = sorted(config.hosts)
    else:
        hosts = [validate_host_name(host, set(config.hosts)) for host in arguments.hosts]
        if len(hosts) != len(set(hosts)):
            raise ValidationError("host list contains duplicates")
    if arguments.timeout < 60 or arguments.timeout > 7200:
        raise ValidationError("timeout must be between 60 and 7200 seconds")
    report, exit_code = pull_fleet(
        config,
        release,
        hosts=hosts,
        components=arguments.components,
        dry_run=arguments.dry_run,
        timeout=arguments.timeout,
        manifest_commit=_manifest_commit(root),
    )
    if arguments.as_json:
        _print_json(report)
    else:
        _print_pull(report)
    return exit_code


def main(argv: list[str] | None = None) -> int:
    arguments = _parser().parse_args(argv)
    try:
        if arguments.command == "push":
            return _run_push(arguments)
        if arguments.command == "pull":
            return _run_pull(arguments)
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
    except (ValidationError, ValueError, CaptureError, PushError, PullError) as exc:
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
            print(f"{_parser().prog}: {message}", file=sys.stderr)
        return EXIT_PREFLIGHT


if __name__ == "__main__":
    raise SystemExit(main())
