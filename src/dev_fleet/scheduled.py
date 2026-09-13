"""Daily Normandy convergence over the existing pull transaction."""
from __future__ import annotations

import argparse
import json
import os
import subprocess
from pathlib import Path

from .config import load_config
from .pull import pull_fleet, run_host
from .redaction import redact
from .rollback import load_historical_release
from .self_pull import checkout_revision, local_host


class ScheduledRunner:
    """A durable marker precedes dispatch and survives process death or lost output."""

    def __init__(self, directory: Path, runner=run_host):
        self.directory = directory
        self.runner = runner

    def __call__(self, spec, program, *, timeout):
        self.directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        marker = self.directory / (spec.name + ".hold")
        try:
            descriptor = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        except FileExistsError:
            return {"host": spec.name, "reachable": True, "result": "failed",
                    "error": "scheduled hold requires operator observation: " + str(marker)}
        with os.fdopen(descriptor, "w") as handle:
            handle.write("Scheduled dispatch may have run. Observe the host before clearing.\n")
            handle.flush()
            os.fsync(handle.fileno())
        directory_fd = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
        result = self.runner(spec, program, timeout=timeout)
        # Only a complete, understood result allows the next daily observation.
        if result.get("reachable") and result.get("result") in {
            "converged", "updated", "ahead", "deferred", "lock-contention"
        }:
            marker.unlink()
        return result


def notification(report):
    hosts = report.get("hosts", [])
    changed = any(
        host.get("result") == "updated"
        or host.get("legacy_updater", {}).get("state") == "updated"
        or any(c.get("state") == "updated" for c in host.get("components", []))
        for host in hosts
    )
    failed = bool(report.get("exit_code"))
    if not changed and not failed:
        return None
    return ("fail" if failed else "info",
            "UpKeeper scheduled convergence " + ("needs attention" if failed else "changed hosts"))


def post_report(report, runner=subprocess.run):
    notice = notification(report)
    if notice is None:
        return
    severity, title = notice
    summary = {key: report[key] for key in (
        "run_id", "controller", "desired_manifest_commit", "exit_code", "error"
    ) if key in report}
    summary["hosts"] = [
        {"host": host.get("host"), "result": host.get("result"),
         "error": host.get("error"),
         "components": [
             {"name": entry.get("name"), "state": entry.get("state"),
              "error": entry.get("error")}
             for entry in host.get("components", []) if entry.get("state") != "converged"
         ]}
        for host in report.get("hosts", [])
    ]
    body = json.dumps(redact(summary), indent=2, sort_keys=True)
    # One attempt only: a lost feed response does not justify duplicate posts.
    result = runner(
        ["feed", "post", title, "--source", "upkeeper-pull", "--tag", "citadel upkeeper",
         "--severity", severity, "--body-file", "-"],
        input=body, capture_output=True, text=True, timeout=30, check=False,
    )
    if result.returncode:
        raise RuntimeError("fleet feed post failed; inspect the journal and run record")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)
    root = Path(__file__).resolve().parents[2]
    try:
        revision = checkout_revision(root)
        config = load_config(root)
        if local_host(config) != "normandy":
            raise ValueError("the fleet convergence timer belongs on normandy only")
        release, revision, stored = load_historical_release(root, revision)
        if checkout_revision(root) != revision:
            raise ValueError("checkout changed during scheduled validation")
        report, code = pull_fleet(
            config, release, hosts=sorted(config.hosts), dry_run=args.dry_run,
            manifest_commit=revision, stored_files=stored, skip_unreachable=True,
            runner=run_host if args.dry_run else ScheduledRunner(
                Path.home() / ".local/state/dev-fleet/scheduled-holds"
            ),
        )
    except Exception as exc:
        report = {"command": "scheduled-pull", "exit_code": 30, "error": str(exc)}
        code = 30
    print(json.dumps(redact(report), indent=2, sort_keys=True))
    if not args.dry_run:
        try:
            post_report(report)
        except Exception as exc:
            print(json.dumps({"feed_error": type(exc).__name__}))
            return code or 20
    return code
