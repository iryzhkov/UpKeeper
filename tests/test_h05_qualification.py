from __future__ import annotations

import json
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

from dev_fleet.h05_qualification import (
    FAULT_REPETITIONS,
    H04_FIXTURE_SHA256,
    QUOTA_MIN_SPAN_SECONDS,
    QUOTA_REQUIRED_HEALTHY_SAMPLES,
    SOAK_HOURS,
    SOAK_MIN_OBSERVATIONS,
    QualificationError,
    init_soak,
    observe_quota,
    observe_soak,
    release_decision,
    representative_workflows,
    run_accelerated,
)

FIXTURE = Path(__file__).parent / "fixtures" / "h04-cross-component-faults-v1.json"
REVISION = "a" * 40


def test_accelerated_campaign_meets_counts_and_safety_contract() -> None:
    evidence = run_accelerated(fixture=FIXTURE, source_revision=REVISION)
    faults = json.loads(FIXTURE.read_text(encoding="utf-8"))["scenarios"]

    assert evidence["h04_fixture_sha256"] == H04_FIXTURE_SHA256
    assert evidence["workflow_count"] == 30
    assert evidence["completed_workflows"] == 30
    assert evidence["fault_injections"] == len(faults) * FAULT_REPETITIONS == 42
    assert set(evidence["fault_repetitions"]) == {item["fault_id"] for item in faults}
    assert set(evidence["fault_repetitions"].values()) == {3}
    assert evidence["duplicate_external_effects"] == 0
    assert evidence["lost_work"] == 0
    assert evidence["starved_workflows"] == 0
    assert evidence["leaked_sessions"] == 0
    assert evidence["leaked_worktrees"] == 0
    assert evidence["leaked_locks"] == 0
    assert evidence["unreconciled_ambiguous_states"] == 0
    assert evidence["backup_restoration"]["exact"] is True
    assert evidence["recovery_objective_seconds"]["passed"] is True
    assert evidence["open_severity_1"] == evidence["open_severity_2"] == 0
    assert evidence["passed"] is True


def test_representative_workload_is_balanced_and_unique() -> None:
    workflows = representative_workflows()
    assert len(workflows) == 30
    assert len({item.workflow_id for item in workflows}) == 30
    assert set(item.kind for item in workflows) == {
        "read-only-discovery",
        "source-verification",
        "bounded-rollout",
        "rollback",
        "ambiguous-effect-reconciliation",
        "detached-attempt-cleanup",
    }


def test_soak_requires_genuine_elapsed_time_minimum_samples_and_bounded_gaps(
    tmp_path: Path,
) -> None:
    state = tmp_path / "soak.json"
    start = datetime(2026, 9, 11, tzinfo=UTC)
    init_soak(state, source_revision=REVISION, now=start)

    for index in range(SOAK_MIN_OBSERVATIONS):
        # Spans exactly 72h while staying below the 20-minute observation-gap fence.
        at = start + timedelta(hours=SOAK_HOURS * (index + 1) / SOAK_MIN_OBSERVATIONS)
        value = observe_soak(state, now=at)

    assert value["status"] == "PASSED"
    assert value["elapsed_seconds"] == SOAK_HOURS * 3600
    assert len(value["observations"]) == SOAK_MIN_OBSERVATIONS
    assert all(item["gap_seconds"] <= value["max_gap_seconds"] for item in value["observations"])


def test_soak_fails_closed_on_gap_clock_reversal_and_revision_change(tmp_path: Path) -> None:
    state = tmp_path / "soak.json"
    start = datetime(2026, 9, 11, tzinfo=UTC)
    init_soak(state, source_revision=REVISION, now=start)
    failed = observe_soak(state, now=start + timedelta(minutes=21))
    assert failed["status"] == "FAILED"
    with pytest.raises(QualificationError, match="another source revision"):
        init_soak(state, source_revision="b" * 40, now=start)

    other = tmp_path / "clock.json"
    init_soak(other, source_revision=REVISION, now=start)
    observe_soak(other, now=start + timedelta(minutes=1))
    with pytest.raises(QualificationError, match="backwards"):
        observe_soak(other, now=start)


def _quota_status(*, healthy: bool) -> dict[str, object]:
    return {
        "status": {
            "quotaPools": {"closed": 0 if healthy else 1},
            "runtime": {
                "health": "healthy" if healthy else "degraded",
                "freshQuotaPools": 1 if healthy else 0,
                "staleQuotaPools": 0 if healthy else 1,
                "reconciliationIssues": [] if healthy else ["quota:stale"],
            },
        }
    }


def test_quota_requires_independent_sustained_healthy_samples_and_rejects_waiver(
    tmp_path: Path,
) -> None:
    state = tmp_path / "quota.json"
    start = datetime(2026, 9, 11, tzinfo=UTC)
    value = observe_quota(state, status=_quota_status(healthy=False), now=start)
    assert value["status"] == "PENDING"
    for index in range(QUOTA_REQUIRED_HEALTHY_SAMPLES):
        value = observe_quota(
            state,
            status=_quota_status(healthy=True),
            now=start + timedelta(seconds=index * QUOTA_MIN_SPAN_SECONDS / 2),
        )
    assert value["status"] == "PASSED"
    assert value["manual_operator_waiver_accepted"] is False

    value = observe_quota(
        state,
        status=_quota_status(healthy=False),
        now=start + timedelta(seconds=QUOTA_MIN_SPAN_SECONDS + 1),
    )
    assert value["status"] == "PENDING"
    assert value["healthy_sample_count"] == 0


def test_release_decision_is_revision_fenced_and_fail_closed() -> None:
    accelerated = {
        "source_revision": REVISION,
        "passed": True,
        "open_severity_1": 0,
        "open_severity_2": 0,
        "evidence_sha256": "1" * 64,
    }
    soak = {"source_revision": REVISION, "status": "PASSED", "evidence_sha256": "2" * 64}
    quota = {
        "status": "PASSED",
        "manual_operator_waiver_accepted": False,
        "evidence_sha256": "3" * 64,
    }
    passed = release_decision(
        accelerated=accelerated, soak=soak, quota=quota, source_revision=REVISION
    )
    assert passed["decision"] == "FOUNDATION_HARDENED"
    assert passed["ordinary_unattended_scheduling_allowed"] is True

    soak["status"] = "RUNNING"
    refused = release_decision(
        accelerated=accelerated, soak=soak, quota=quota, source_revision=REVISION
    )
    assert refused["decision"] == "NOT_RELEASED"
    assert refused["ordinary_unattended_scheduling_allowed"] is False
    assert refused["unmet_gates"] == ["72-hour sustained mixed-workload soak"]
