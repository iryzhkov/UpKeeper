#!/usr/bin/env python3
"""Require the ci.yml push runs for the peeled tag commit to have passed, before release.

Every completed push run for the commit must have succeeded, or each failed run
must be superseded by a later successful run on the same ref. A run that is
still queued or in progress could still fail, so the gate waits for it. The
decision and the runs it rests on are printed to the release log.

The gate fails closed on evidence it cannot trust: a listing that fills the
retrieval cap may omit older failed runs, a malformed listing proves nothing,
and a failure is superseded only when both runs carry a usable ref and a
parsable creation time.
"""

import json
import os
import subprocess
import sys
import time
from datetime import datetime

RUN_FIELDS = "databaseId,headSha,status,conclusion,event,headBranch,createdAt"

# gh lists the newest runs first, at most this many. A listing of this size may
# have cut older runs off, so it cannot authorize a release.
RUN_LIMIT = 1000


def ref_of(run: dict) -> str | None:
    """The run's branch, or None unless it names one exactly.

    A blank, whitespace-only or padded value names no branch. It is refused
    rather than stripped, so malformed metadata can never be turned into the
    "same ref" evidence that lets a later success supersede a failure.
    """
    ref = run.get("headBranch")
    if not isinstance(ref, str) or not ref or ref != ref.strip() or any(c.isspace() for c in ref):
        return None
    return ref


def created_at(run: dict) -> datetime | None:
    """The run's creation time, or None unless it is an ISO 8601 time with a zone."""
    value = run.get("createdAt")
    if not isinstance(value, str) or "T" not in value:
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed if parsed.tzinfo is not None else None


def attributable(run: object) -> bool:
    """Whether the entry names its commit and event well enough to filter it out.

    An empty, blank or space-padded value cannot prove that the run belongs to
    another commit or event, so such an entry is malformed, never unrelated.
    """
    if not isinstance(run, dict):
        return False
    return all(isinstance(value, str) and value != "" and value == value.strip()
               for value in (run.get("headSha"), run.get("event")))


def describe(run: dict) -> str:
    state = run.get("conclusion") or run.get("status")
    return (f"run {run.get('databaseId')} on {run.get('headBranch')} "
            f"created {run.get('createdAt')}: {state}")


def decide(runs: object, commit: str) -> tuple[str, list[str]]:
    """Return "pass", "fail" or "wait", with one reason line per run considered."""
    if not isinstance(runs, list) or not all(attributable(r) for r in runs):
        return "fail", ["the run listing is malformed: every entry needs a non-empty headSha "
                        "and event"]
    candidates = [r for r in runs if r.get("headSha") == commit and r.get("event") == "push"]
    malformed = [r for r in candidates if not isinstance(r.get("status"), str)
                 or (r.get("status") == "completed"
                     and not isinstance(r.get("conclusion"), (str, type(None))))]
    if malformed:
        return "fail", [describe(r) + "; malformed status or conclusion" for r in malformed]
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
        ref, created = ref_of(run), created_at(run)
        if ref is None or created is None:
            reasons.append(describe(run) + "; no usable ref or creation time, so it cannot "
                           "be superseded")
            unsuperseded.append(run)
            continue
        later = [s for s in successes if ref_of(s) == ref
                 and (successor := created_at(s)) is not None and successor > created]
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
             "--commit", commit, "--event", "push", "--limit", str(RUN_LIMIT),
             "--json", RUN_FIELDS],
            text=True, timeout=60,
        )
        try:
            runs = json.loads(output)
        except ValueError:
            runs = None
        if isinstance(runs, list) and len(runs) >= RUN_LIMIT:
            decision, reasons = "fail", [
                f"the listing reached the retrieval cap of {RUN_LIMIT} runs, so the run "
                "history is incomplete and an older failed run may be missing"]
        else:
            decision, reasons = decide(runs, commit)
        if decision != "wait":
            print(f"ci.yml release gate decision for {commit}: {decision}", flush=True)
            for reason in reasons:
                print(f"  {reason}", flush=True)
            if decision == "pass":
                return 0
            print(f"ci.yml gate refused for {commit}: the push runs do not prove that "
                  "every failed run was superseded by a later successful run on the same "
                  "ref", file=sys.stderr)
            return 1
        print(f"Waiting for ci.yml push gate for {commit}: {'; '.join(reasons)}", flush=True)
        time.sleep(15)
    print(f"Timed out waiting for ci.yml gate for {commit}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
