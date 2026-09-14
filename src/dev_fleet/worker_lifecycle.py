"""Persistent execution worker lifecycle for a pinned Steward installation.

Installing the Steward binary does not change the worker process that is already
running, so a host that runs the persistent worker keeps serving the previous
release against a coordinator that already speaks the new one. This module owns
the decision to restart that worker, and the refusal to restart it while its
durable journal still owns dispatched execution.

The host commands are injected, because the caller is the remote pull agent and
owns its own bounded subprocess helpers.
"""

from __future__ import annotations

import json

WORKER_UNIT = "t3-steward-worker.service"


class WorkerRestartFailed(Exception):
    """The worker could not be inspected or did not come back after a restart."""


def restart_persistent_worker(
    binary,
    actions,
    *,
    run,
    must,
    unit_loaded,
    unit_active,
    dry_run=False,
    unit=WORKER_UNIT,
):
    """Restart the persistent worker onto the installed binary when it is idle.

    A host without the worker unit is left untouched. A worker whose journal owns
    dispatched execution is reported and left running, never forced.
    """
    if not unit_loaded(unit):
        return
    summary = run([str(binary), "worker", "inspect-journal"], timeout=30)
    if not summary["ok"]:
        raise WorkerRestartFailed("worker journal inspection failed: " + summary["output"][-200:])
    try:
        dispatched = int(json.loads(summary["output"])["dispatched"])
    except (ValueError, TypeError, KeyError):
        raise WorkerRestartFailed("worker journal inspection returned no usable summary") from None
    if dispatched:
        actions.append(
            f"worker restart deferred: {dispatched} dispatched attempt(s) in the journal"
        )
        return
    if dry_run:
        actions.append(f"would restart {unit} onto the pinned steward")
        return
    must(["systemctl", "--user", "restart", unit], what="restart persistent worker")
    if not unit_active(unit):
        raise WorkerRestartFailed("persistent worker is inactive after restart")
    actions.append(f"restarted {unit} onto the pinned steward")
