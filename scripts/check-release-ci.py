#!/usr/bin/env python3
"""Require a successful ci.yml push run for the peeled tag commit, before release."""

import json
import os
import subprocess
import sys
import time


def passed(runs: list, commit: str) -> bool:
    """Only the latest exact-commit push run counts, matching Refresh's gate."""
    candidates = [r for r in runs if r.get("headSha") == commit and r.get("event") == "push"]
    return bool(candidates and candidates[0].get("status") == "completed"
                and candidates[0].get("conclusion") == "success")


def main() -> int:
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    repository = os.environ["GITHUB_REPOSITORY"]
    deadline = time.monotonic() + 45 * 60
    while time.monotonic() < deadline:
        output = subprocess.check_output(
            ["gh", "run", "list", "--repo", repository, "--workflow", "ci.yml",
             "--commit", commit, "--event", "push", "--limit", "1",
             "--json", "headSha,status,conclusion,event"],
            text=True, timeout=60,
        )
        runs = json.loads(output)
        if passed(runs, commit):
            print(f"ci.yml push gate succeeded for {commit}", flush=True)
            return 0
        if runs and runs[0].get("status") == "completed":
            print(f"ci.yml gate refused for {commit}: {output}", file=sys.stderr)
            return 1
        print(f"Waiting for ci.yml push gate for {commit}", flush=True)
        time.sleep(15)
    print(f"Timed out waiting for ci.yml gate for {commit}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
