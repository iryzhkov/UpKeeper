#!/usr/bin/env python3
"""Run the hosted job targets sequentially and print complete wall timings."""

import os
import subprocess
import sys
import time

TARGETS = (
    ("ci-manifest",),
    ("ci-go-checks",),
    ("ci-race", "GROUP=author-baseline"),
    ("ci-race", "GROUP=author-catalog"),
    ("ci-race", "GROUP=author-other"),
    ("ci-race", "GROUP=other-packages"),
    ("ci-go-gate",),
    ("ci-python",),
)


def run() -> int:
    started = time.monotonic()
    if os.environ.get("UPKEEPER_POLICY_INPUTS_READY") != "1":
        status = subprocess.call(
            ["bash", "scripts/with-policy-test-inputs.sh", sys.executable, __file__]
        )
        print(f"TIMING ci-local (including policy inputs) wall_s={time.monotonic() - started:.3f}"
              f" exit={status}", flush=True)
        return status
    results = {}
    for target in TARGETS:
        name = target[0]
        if name in {"ci-go-checks", "ci-race"} and results["ci-manifest"] != "success":
            results[name] = "skipped"
            print(f"TIMING {' '.join(target)} wall_s=0.000 result=skipped", flush=True)
            continue
        env = os.environ.copy()
        if name == "ci-go-gate":
            env.update(MANIFEST_RESULT=results["ci-manifest"],
                       CHECKS_RESULT=results["ci-go-checks"], RACE_RESULT=results["ci-race"])
        begin = time.monotonic()
        status = subprocess.call(["make", *target], env=env)
        print(f"TIMING {' '.join(target)} wall_s={time.monotonic() - begin:.3f} exit={status}",
              flush=True)
        # Matrix fail-fast is disabled: finish every shard and retain any failure.
        outcome = "success" if status == 0 else "failure"
        if results.get(name) != "failure":
            results[name] = outcome
    return 0 if all(value == "success" for value in results.values()) else 1


if __name__ == "__main__":
    sys.exit(run())
