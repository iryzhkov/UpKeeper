"""Refresh a gated release and converge this fleet host locally, without inbound SSH."""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path

from .config import load_config
from .pull import pull_fleet, run_host
from .redaction import redact
from .rollback import load_historical_release
from .scheduled import ScheduledRunner
from .self_pull import checkout_revision, local_host

ORIGINS = {
    "https://github.com/iryzhkov/UpKeeper.git",
    "git@github.com:iryzhkov/UpKeeper.git",
}


def refresh(root: Path, runner=subprocess.run) -> str:
    before = checkout_revision(root)
    env = {**os.environ, "GIT_TERMINAL_PROMPT": "0",
           "GIT_SSH_COMMAND": "ssh -o BatchMode=yes -o StrictHostKeyChecking=yes"}
    def command(args):
        result = runner(args, capture_output=True, text=True, check=False, timeout=120, env=env)
        if result.returncode:
            raise ValueError(f"self refresh failed: {args[0]} {args[1]}")
        return result.stdout.strip()
    def git(*args):
        return command(["git", "-C", str(root), *args])
    if git("branch", "--show-current") != "main":
        raise ValueError("self refresh requires main")
    if git("remote", "get-url", "origin") not in ORIGINS:
        raise ValueError("self refresh requires the canonical UpKeeper GitHub origin")
    git("fetch", "--quiet", "--no-tags", "origin", "refs/heads/main")
    candidate = git("rev-parse", "FETCH_HEAD")
    git("merge-base", "--is-ancestor", before, candidate)
    checks = json.loads(command([
        "gh", "run", "list", "--repo", "iryzhkov/UpKeeper", "--workflow", "ci.yml",
        "--event", "push", "--commit", candidate, "--limit", "1",
        "--json", "headSha,status,conclusion",
    ]))
    if not checks or checks[0] != {
        "headSha": candidate, "status": "completed", "conclusion": "success"
    }:
        raise ValueError("candidate main has no successful GitHub push gate; checkout unchanged")
    if checkout_revision(root) != before:
        raise ValueError("checkout changed during refresh; preserving it")
    git("merge", "--ff-only", "--quiet", candidate)
    if checkout_revision(root) != candidate:
        raise ValueError("refreshed checkout did not verify")
    return candidate


def converge(root: Path, dry_run=False):
    revision = checkout_revision(root)
    config = load_config(root)
    host = local_host(config)
    release, revision, stored = load_historical_release(root, revision)
    if checkout_revision(root) != revision:
        raise ValueError("checkout changed during self validation")
    return pull_fleet(
        config, release, hosts=[host], self_only=True, dry_run=dry_run,
        manifest_commit=revision, stored_files=stored,
        runner=run_host if dry_run else ScheduledRunner(
            Path.home() / ".local/state/dev-fleet/scheduled-holds"
        ),
    )


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--converge", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    root = Path(__file__).resolve().parents[2]
    try:
        local_host(load_config(root))  # Exactly one local inventory identity, before network.
        if not args.dry_run and not args.converge:
            refresh(root)
            # Import and run the newly gated code, never old imports with a new manifest.
            os.execv(sys.executable, [  # noqa: S606 -- fixed interpreter and gated source
                sys.executable, str(root / "scripts/upkeeper-self-pull"), "--converge",
            ])
        report, code = converge(root, args.dry_run)
    except Exception as exc:
        report, code = {"command": "self-schedule", "error": str(exc), "exit_code": 30}, 30
    print(json.dumps(redact(report), indent=2, sort_keys=True))
    return code
