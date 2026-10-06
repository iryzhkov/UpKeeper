#!/usr/bin/env python3
"""Require the ci.yml push runs for the peeled tag commit to have passed, before release.

Every completed push run for the commit must have succeeded, or each failed run
must be superseded by a later successful run on the same ref. A run that is
still queued or in progress could still fail, so the gate waits for it. The
decision and the runs it rests on are printed to the release log.
"""

import json
import os
import subprocess
import sys
import time

RUN_FIELDS = "databaseId,headSha,status,conclusion,event,headBranch,createdAt"


def describe(run: dict) -> str:
    state = run.get("conclusion") or run.get("status")
    return (f"run {run.get('databaseId')} on {run.get('headBranch')} "
            f"created {run.get('createdAt')}: {state}")


def decide(runs: list, commit: str) -> tuple[str, list[str]]:
    """Return "pass", "fail" or "wait", with one reason line per run considered."""
    candidates = [r for r in runs if r.get("headSha") == commit and r.get("event") == "push"]
    if not candidates:
        return "wait", ["no ci.yml push run for this commit yet"]
    reasons = []
    pending = [r for r in candidates if r.get("status") != "completed"]
    for run in pending:
        reasons.append(describe(run) + "; waiting for it to complete")
    completed = [r for r in candidates if r.get("status") == "completed"]
    successes = [r for r in completed if r.get("conclusion") == "success"]
    unsuperseded = []
    for run in completed:
        if run.get("conclusion") == "success":
            reasons.append(describe(run))
            continue
        later = [s for s in successes if s.get("headBranch") == run.get("headBranch")
                 and str(s.get("createdAt", "")) > str(run.get("createdAt", ""))]
        if later:
            reasons.append(describe(run) + f"; superseded by run {later[0].get('databaseId')}")
        else:
            reasons.append(describe(run) + "; no later successful run on the same ref")
            unsuperseded.append(run)
    if pending:
        return "wait", reasons
    if unsuperseded or not successes:
        return "fail", reasons
    return "pass", reasons


def passed(runs: list, commit: str) -> bool:
    return decide(runs, commit)[0] == "pass"


def main() -> int:
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    repository = os.environ["GITHUB_REPOSITORY"]
    deadline = time.monotonic() + 45 * 60
    while time.monotonic() < deadline:
        output = subprocess.check_output(
            ["gh", "run", "list", "--repo", repository, "--workflow", "ci.yml",
             "--commit", commit, "--event", "push", "--limit", "100",
             "--json", RUN_FIELDS],
            text=True, timeout=60,
        )
        decision, reasons = decide(json.loads(output), commit)
        if decision != "wait":
            print(f"ci.yml release gate decision for {commit}: {decision}", flush=True)
            for reason in reasons:
                print(f"  {reason}", flush=True)
            if decision == "pass":
                return 0
            print(f"ci.yml gate refused for {commit}: a failed push run was not "
                  "superseded by a later successful run on the same ref", file=sys.stderr)
            return 1
        print(f"Waiting for ci.yml push gate for {commit}: {'; '.join(reasons)}", flush=True)
        time.sleep(15)
    print(f"Timed out waiting for ci.yml gate for {commit}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
