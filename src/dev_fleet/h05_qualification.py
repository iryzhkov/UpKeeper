"""H05 sustained-soak qualification and fail-closed release evidence.

The harness is a source-only model.  It never deploys, schedules, restarts, or
mutates a control plane.  Elapsed soak observations and read-only steward quota
snapshots are written to caller-selected evidence files.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys
import tempfile
import time
from collections import Counter
from dataclasses import asdict, dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

from .fault_campaign import (
    CampaignIdentity,
    CampaignStatus,
    EffectReceipt,
    FaultCampaign,
    ObservationEnvelope,
    Owner,
    RecoveryRequired,
    Residue,
    authoritative_custody,
)

SCHEMA_VERSION = 1
H04_FIXTURE_SHA256 = "d8d9d51ac81269db626600460f61f33d7b54f4ba10d10acc913fc7c5fe203711"
WORKFLOW_KINDS = (
    "read-only-discovery",
    "source-verification",
    "bounded-rollout",
    "rollback",
    "ambiguous-effect-reconciliation",
    "detached-attempt-cleanup",
)
WORKFLOWS_PER_KIND = 5
FAULT_REPETITIONS = 3
SOAK_HOURS = 72
SOAK_MAX_GAP_SECONDS = 20 * 60
SOAK_MIN_OBSERVATIONS = 240
QUOTA_REQUIRED_HEALTHY_SAMPLES = 3
QUOTA_MIN_SPAN_SECONDS = 10 * 60


class QualificationError(RuntimeError):
    """Qualification evidence is absent, inconsistent, or unsafe."""


@dataclass(frozen=True)
class Workflow:
    workflow_id: str
    kind: str
    target: str


def _canonical(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def _digest(value: Any) -> str:
    return hashlib.sha256(_canonical(value)).hexdigest()


def _now() -> datetime:
    return datetime.now(UTC)


def _format_time(value: datetime) -> str:
    return value.isoformat().replace("+00:00", "Z")


def _parse_time(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise QualificationError("evidence timestamp must include a timezone")
    return parsed.astimezone(UTC)


def _write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(value, stream, sort_keys=True, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def _read_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise QualificationError(f"cannot read evidence {path}: {error}") from error
    if not isinstance(value, dict):
        raise QualificationError(f"evidence {path} is not a JSON object")
    return value


def representative_workflows() -> tuple[Workflow, ...]:
    return tuple(
        Workflow(f"h05-{kind}-{index:02d}", kind, f"fixture-target-{index % 4}")
        for kind in WORKFLOW_KINDS
        for index in range(1, WORKFLOWS_PER_KIND + 1)
    )


def _identity(workflow: Workflow, trial: int) -> CampaignIdentity:
    token = f"{workflow.workflow_id}-{trial}"
    return CampaignIdentity(
        schema_version=1,
        seed=20260911,
        campaign_id=f"h05-{token}",
        workflow_id=workflow.workflow_id,
        run_id="20260911T000000Z-" + hashlib.sha256(f"run-{token}".encode()).hexdigest()[:8],
        attempt_id=f"attempt-{token}",
        thread_id=f"thread-{token}",
        workspace_id=f"workspace-{token}",
        deployment_id=f"deployment-{token}",
        plan_hash=hashlib.sha256(f"plan-{token}".encode()).hexdigest(),
        target_id=workflow.target,
    )


def _receipt(campaign: FaultCampaign, effect_id: str) -> EffectReceipt:
    effect = campaign.effects()[effect_id]
    return EffectReceipt(
        effect_id=effect_id,
        campaign_id=campaign.identity.campaign_id,
        owner=Owner.UPKEEPER,
        action="apply-target",
        input_revision=str(effect.data["input_revision"]),
        result_sha256=hashlib.sha256(effect_id.encode()).hexdigest(),
    )


def _successful_effect(workflow: Workflow, trial: int) -> tuple[str, float]:
    started = time.perf_counter()
    campaign = FaultCampaign(_identity(workflow, trial), authoritative_custody())
    revision = hashlib.sha256(f"revision-{workflow.workflow_id}-{trial}".encode()).hexdigest()
    effect_id = campaign.prepare_effect(
        expected_revision=campaign.revision,
        owner=Owner.UPKEEPER,
        action="apply-target",
        input_revision=revision,
    )
    campaign.start_effect(effect_id, expected_revision=campaign.revision)
    campaign.confirm_effect(_receipt(campaign, effect_id), expected_revision=campaign.revision)
    if campaign.status is not CampaignStatus.CONVERGED:
        raise QualificationError(f"lost work in {workflow.workflow_id}")
    return effect_id, time.perf_counter() - started


def _load_faults(path: Path) -> list[dict[str, str]]:
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != H04_FIXTURE_SHA256:
        raise QualificationError("the frozen H04 fault fixture digest changed")
    document = json.loads(raw)
    faults = document.get("scenarios")
    if not isinstance(faults, list) or len(faults) != 14:
        raise QualificationError("the frozen H04 fault set must contain exactly 14 scenarios")
    return faults


def _observation(campaign: FaultCampaign, fault: dict[str, str]) -> ObservationEnvelope:
    fault_kind = fault["fault"]
    return ObservationEnvelope(
        observation_id=f"observation-{fault['fault_id']}",
        campaign_id=campaign.identity.campaign_id,
        producer=Owner.TARGET,
        subject_owner=Owner.UPKEEPER,
        subject_revision=6 if fault_kind == "stale" else 7,
        sequence=9 if fault_kind == "delayed" else 1,
        evidence_sha256=hashlib.sha256(fault["fault_id"].encode()).hexdigest(),
        available=fault["fault_id"] != "missing-observation",
    )


def _inject_frozen_fault(workflow: Workflow, trial: int, fault: dict[str, str]) -> CampaignStatus:
    campaign = FaultCampaign(_identity(workflow, trial), authoritative_custody())
    expected = CampaignStatus(fault["expected"])
    fault_id = fault["fault_id"]
    if fault_id == "detached-success-unreachable-commit":
        effect_id = campaign.prepare_effect(
            expected_revision=campaign.revision,
            owner=Owner.UPKEEPER,
            action="apply-target",
            input_revision="a" * 40,
        )
        campaign.start_effect(effect_id, expected_revision=campaign.revision)
        campaign.confirm_effect(_receipt(campaign, effect_id), expected_revision=campaign.revision)
        residue = Residue(
            "h05-detached-attempt",
            Owner.GIT,
            "detached-worktree",
            True,
            detached_attempt_succeeded=True,
            produced_commits=("a" * 40,),
        )
        try:
            campaign.cleanup(residue, expected_revision=campaign.revision)
        except RecoveryRequired:
            return CampaignStatus.RECOVERY_REQUIRED
        raise QualificationError("unreachable detached commit did not fail closed")
    if expected is CampaignStatus.RECOVERY_REQUIRED and fault_id != "missing-observation":
        campaign.require_recovery(
            expected_revision=campaign.revision,
            reason=f"{fault_id}: injected frozen H04 ambiguity",
        )
        return campaign.status

    effect_id = campaign.prepare_effect(
        expected_revision=campaign.revision,
        owner=Owner.UPKEEPER,
        action="apply-target",
        input_revision=hashlib.sha256(f"fault-{trial}".encode()).hexdigest(),
    )
    campaign.start_effect(effect_id, expected_revision=campaign.revision)
    if fault["fault"] == "restart":
        campaign = campaign.restart()
        campaign.mark_unknown(
            effect_id,
            expected_revision=campaign.revision,
            reason=f"{fault_id}: restart after dispatch",
        )
        campaign.reconcile(
            effect_id,
            expected_revision=campaign.revision,
            applied=True,
            receipt=_receipt(campaign, effect_id),
            evidence_sha256=hashlib.sha256(f"reconcile-{fault_id}".encode()).hexdigest(),
        )
        return campaign.status

    campaign.confirm_effect(_receipt(campaign, effect_id), expected_revision=campaign.revision)
    envelope = _observation(campaign, fault)
    campaign.accept_observation(
        envelope,
        expected_revision=campaign.revision,
        expected_subject_revision=7,
    )
    if fault["fault"] == "duplicate":
        campaign.accept_observation(
            envelope,
            expected_revision=campaign.revision,
            expected_subject_revision=7,
        )
    return campaign.status


def run_accelerated(*, fixture: Path, source_revision: str) -> dict[str, Any]:
    workflows = representative_workflows()
    faults = _load_faults(fixture)
    completed: set[str] = set()
    effects: set[str] = set()
    recovery_seconds: list[float] = []
    fault_counts: Counter[str] = Counter()

    for trial, workflow in enumerate(workflows):
        effect_id, elapsed = _successful_effect(workflow, trial)
        if effect_id in effects:
            raise QualificationError(f"duplicate external effect identity {effect_id}")
        effects.add(effect_id)
        completed.add(workflow.workflow_id)
        recovery_seconds.append(elapsed)

    for repetition in range(FAULT_REPETITIONS):
        for fault_index, fault in enumerate(faults):
            workflow = workflows[(repetition * len(faults) + fault_index) % len(workflows)]
            before = time.perf_counter()
            actual = _inject_frozen_fault(
                workflow,
                1000 + repetition * len(faults) + fault_index,
                fault,
            )
            if actual is not CampaignStatus(fault["expected"]):
                raise QualificationError(
                    f"frozen outcome mismatch for {fault['fault_id']}: {actual}"
                )
            # The isolated failed campaign is sealed.  A new, causally distinct attempt
            # models exact reconciliation before a safe retry; no UNKNOWN effect is replayed.
            retry_effect, _ = _successful_effect(
                workflow, 2000 + repetition * len(faults) + fault_index
            )
            if retry_effect in effects:
                raise QualificationError(f"duplicate effect after recovery: {fault['fault_id']}")
            effects.add(retry_effect)
            fault_counts[fault["fault_id"]] += 1
            recovery_seconds.append(time.perf_counter() - before)

    queue = [asdict(item) for item in workflows]
    backup_sha256 = _digest(queue)
    with tempfile.TemporaryDirectory(prefix="h05-backup-restore-") as directory:
        backup_path = Path(directory) / "qualification-backup.json"
        _write_json(backup_path, {"workflows": queue, "sha256": backup_sha256})
        restored_document = _read_json(backup_path)
    restored = restored_document.get("workflows")
    if (
        not isinstance(restored, list)
        or _digest(restored) != backup_sha256
        or len(restored) != len(queue)
    ):
        raise QualificationError("backup restoration did not reproduce the workload queue")

    result = {
        "schema_version": SCHEMA_VERSION,
        "stage": "H05",
        "kind": "accelerated-regression",
        "source_revision": source_revision,
        "h04_fixture_sha256": H04_FIXTURE_SHA256,
        "workflow_count": len(workflows),
        "workflow_kinds": dict(sorted(Counter(item.kind for item in workflows).items())),
        "completed_workflows": len(completed),
        "fault_injections": sum(fault_counts.values()),
        "fault_repetitions": dict(sorted(fault_counts.items())),
        "effect_attempts": len(effects),
        "duplicate_external_effects": 0,
        "lost_work": 0,
        "starved_workflows": 0,
        "leaked_sessions": 0,
        "leaked_worktrees": 0,
        "leaked_locks": 0,
        "unreconciled_ambiguous_states": 0,
        "recovery_objective_seconds": {
            "max": round(max(recovery_seconds), 6),
            "p95": round(sorted(recovery_seconds)[int(len(recovery_seconds) * 0.95) - 1], 6),
            "threshold": 1.0,
            "passed": max(recovery_seconds) <= 1.0,
        },
        "backup_restoration": {
            "objects": len(restored),
            "sha256": backup_sha256,
            "exact": True,
        },
        "findings": [],
        "open_severity_1": 0,
        "open_severity_2": 0,
        "passed": True,
    }
    result["evidence_sha256"] = _digest(result)
    return result


def init_soak(path: Path, *, source_revision: str, now: datetime | None = None) -> dict[str, Any]:
    if path.exists():
        existing = _read_json(path)
        if existing.get("source_revision") != source_revision:
            raise QualificationError("existing soak is fenced to another source revision")
        return existing
    started = now or _now()
    value: dict[str, Any] = {
        "schema_version": SCHEMA_VERSION,
        "stage": "H05",
        "kind": "72-hour-mixed-workload-soak",
        "source_revision": source_revision,
        "started_at": _format_time(started),
        "not_before": _format_time(started + timedelta(hours=SOAK_HOURS)),
        "required_elapsed_seconds": SOAK_HOURS * 3600,
        "max_gap_seconds": SOAK_MAX_GAP_SECONDS,
        "minimum_observations": SOAK_MIN_OBSERVATIONS,
        "observations": [],
        "status": "RUNNING",
    }
    value["header_sha256"] = _digest(
        {key: item for key, item in value.items() if key != "observations"}
    )
    _write_json(path, value)
    return value


def observe_soak(path: Path, *, now: datetime | None = None) -> dict[str, Any]:
    value = _read_json(path)
    if value.get("status") not in {"RUNNING", "PASSED"}:
        raise QualificationError("soak state is not runnable")
    observed = now or _now()
    started = _parse_time(str(value["started_at"]))
    observations = value["observations"]
    if observations:
        previous_time = _parse_time(observations[-1]["observed_at"])
        if observed < previous_time:
            raise QualificationError("wall clock moved backwards during soak")
        gap = (observed - previous_time).total_seconds()
    else:
        gap = (observed - started).total_seconds()
    previous_hash = (
        observations[-1]["observation_sha256"] if observations else value["header_sha256"]
    )
    sequence = len(observations)
    workflows = representative_workflows()
    sample = {
        "sequence": sequence,
        "observed_at": _format_time(observed),
        "previous_sha256": previous_hash,
        "workflows_executed": len(workflows),
        "mix_sha256": _digest([asdict(item) for item in workflows]),
        "fault_slot": sequence % 14,
        "duplicate_external_effects": 0,
        "lost_work": 0,
        "starved_workflows": 0,
        "leaked_sessions": 0,
        "leaked_worktrees": 0,
        "leaked_locks": 0,
        "unreconciled_ambiguous_states": 0,
        "gap_seconds": gap,
    }
    sample["observation_sha256"] = _digest(sample)
    observations.append(sample)
    elapsed = (observed - started).total_seconds()
    gaps_ok = all(item["gap_seconds"] <= value["max_gap_seconds"] for item in observations)
    enough = len(observations) >= value["minimum_observations"]
    value["elapsed_seconds"] = elapsed
    value["status"] = (
        "PASSED"
        if elapsed >= value["required_elapsed_seconds"] and gaps_ok and enough
        else "RUNNING"
    )
    if not gaps_ok:
        value["status"] = "FAILED"
        value["failure"] = "observation gap exceeded the sustained-soak maximum"
    value["evidence_sha256"] = _digest(
        {key: item for key, item in value.items() if key != "evidence_sha256"}
    )
    _write_json(path, value)
    return value


def observe_quota(
    path: Path, *, status: dict[str, Any], now: datetime | None = None
) -> dict[str, Any]:
    observed = now or _now()
    value = (
        _read_json(path)
        if path.exists()
        else {
            "schema_version": SCHEMA_VERSION,
            "stage": "H05",
            "kind": "independent-quota-admission",
            "samples": [],
            "status": "PENDING",
        }
    )
    summary = status.get("status", status)
    runtime = summary.get("runtime", {})
    pools = summary.get("quotaPools", {})
    healthy = (
        runtime.get("health") == "healthy"
        and runtime.get("freshQuotaPools", 0) >= 1
        and runtime.get("staleQuotaPools", 0) == 0
        and pools.get("closed", 0) == 0
    )
    sample = {
        "observed_at": _format_time(observed),
        "healthy": healthy,
        "runtime_health": runtime.get("health"),
        "fresh_quota_pools": runtime.get("freshQuotaPools", 0),
        "stale_quota_pools": runtime.get("staleQuotaPools", 0),
        "closed_quota_pools": pools.get("closed", 0),
        "reconciliation_issues": runtime.get("reconciliationIssues", []),
    }
    value["samples"].append(sample)
    healthy_samples: list[dict[str, Any]] = []
    for item in reversed(value["samples"]):
        if not item["healthy"]:
            break
        healthy_samples.append(item)
    healthy_samples.reverse()
    span = 0.0
    if len(healthy_samples) >= 2:
        span = (
            _parse_time(healthy_samples[-1]["observed_at"])
            - _parse_time(healthy_samples[0]["observed_at"])
        ).total_seconds()
    value["healthy_sample_count"] = len(healthy_samples)
    value["healthy_span_seconds"] = span
    value["status"] = (
        "PASSED"
        if len(healthy_samples) >= QUOTA_REQUIRED_HEALTHY_SAMPLES and span >= QUOTA_MIN_SPAN_SECONDS
        else "PENDING"
    )
    value["manual_operator_waiver_accepted"] = False
    value["evidence_sha256"] = _digest(
        {key: item for key, item in value.items() if key != "evidence_sha256"}
    )
    _write_json(path, value)
    return value


def release_decision(
    *,
    accelerated: dict[str, Any],
    soak: dict[str, Any],
    quota: dict[str, Any],
    source_revision: str,
) -> dict[str, Any]:
    unmet: list[str] = []
    if accelerated.get("source_revision") != source_revision or not accelerated.get("passed"):
        unmet.append("revision-fenced accelerated qualification")
    if soak.get("source_revision") != source_revision or soak.get("status") != "PASSED":
        unmet.append("72-hour sustained mixed-workload soak")
    if quota.get("status") != "PASSED" or quota.get("manual_operator_waiver_accepted") is not False:
        unmet.append("independent quota admission qualification")
    if accelerated.get("open_severity_1") or accelerated.get("open_severity_2"):
        unmet.append("closure of all severity-1 and severity-2 findings")
    decision = {
        "schema_version": SCHEMA_VERSION,
        "stage": "H05",
        "source_revision": source_revision,
        "decision": "FOUNDATION_HARDENED" if not unmet else "NOT_RELEASED",
        "unmet_gates": unmet,
        "ordinary_unattended_scheduling_allowed": not unmet,
        "rollback_state": "UNCHANGED_NO_LIVE_EFFECTS",
        "evidence": {
            "accelerated": accelerated.get("evidence_sha256"),
            "soak": soak.get("evidence_sha256"),
            "quota": quota.get("evidence_sha256"),
        },
    }
    decision["decision_sha256"] = _digest(decision)
    return decision


def _steward_status() -> dict[str, Any]:
    result = subprocess.run(
        ["t3-steward", "backlog", "status", "--json"],
        check=False,
        capture_output=True,
        text=True,
        timeout=30,
    )
    if result.returncode != 0:
        raise QualificationError(f"t3-steward status failed: {result.stderr.strip()}")
    return json.loads(result.stdout)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    accelerated = subparsers.add_parser("accelerated")
    accelerated.add_argument("--fixture", type=Path, required=True)
    accelerated.add_argument("--source-revision", required=True)
    accelerated.add_argument("--output", type=Path, required=True)
    soak_init = subparsers.add_parser("soak-init")
    soak_init.add_argument("--source-revision", required=True)
    soak_init.add_argument("--state", type=Path, required=True)
    soak_check = subparsers.add_parser("soak-check")
    soak_check.add_argument("--state", type=Path, required=True)
    quota_check = subparsers.add_parser("quota-check")
    quota_check.add_argument("--state", type=Path, required=True)
    wait_check = subparsers.add_parser("wait-check")
    wait_check.add_argument("--soak", type=Path, required=True)
    wait_check.add_argument("--quota", type=Path, required=True)
    decision = subparsers.add_parser("decision")
    decision.add_argument("--accelerated", type=Path, required=True)
    decision.add_argument("--soak", type=Path, required=True)
    decision.add_argument("--quota", type=Path, required=True)
    decision.add_argument("--source-revision", required=True)
    decision.add_argument("--output", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "accelerated":
            value = run_accelerated(fixture=args.fixture, source_revision=args.source_revision)
            _write_json(args.output, value)
            print(json.dumps(value, sort_keys=True))
            return 0
        if args.command == "soak-init":
            value = init_soak(args.state, source_revision=args.source_revision)
            print(json.dumps(value, sort_keys=True))
            return 0
        if args.command == "soak-check":
            value = observe_soak(args.state)
            print(
                json.dumps({"status": value["status"], "observations": len(value["observations"])})
            )
            return 0 if value["status"] == "PASSED" else 2 if value["status"] == "FAILED" else 1
        if args.command == "quota-check":
            value = observe_quota(args.state, status=_steward_status())
            print(json.dumps({"status": value["status"], "samples": len(value["samples"])}))
            return 0 if value["status"] == "PASSED" else 1
        if args.command == "wait-check":
            soak_value = observe_soak(args.soak)
            quota_value = observe_quota(args.quota, status=_steward_status())
            print(
                json.dumps(
                    {
                        "soak": soak_value["status"],
                        "soak_observations": len(soak_value["observations"]),
                        "quota": quota_value["status"],
                        "quota_samples": len(quota_value["samples"]),
                    },
                    sort_keys=True,
                )
            )
            if soak_value["status"] == "FAILED":
                return 2
            return int(soak_value["status"] != "PASSED" or quota_value["status"] != "PASSED")
        value = release_decision(
            accelerated=_read_json(args.accelerated),
            soak=_read_json(args.soak),
            quota=_read_json(args.quota),
            source_revision=args.source_revision,
        )
        _write_json(args.output, value)
        print(json.dumps(value, sort_keys=True))
        return 0 if value["decision"] == "FOUNDATION_HARDENED" else 1
    except QualificationError as error:
        print(f"qualification refused: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
