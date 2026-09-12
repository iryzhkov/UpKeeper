from __future__ import annotations

import json
from dataclasses import replace
from pathlib import Path

import pytest

from dev_fleet.fault_campaign import (
    CampaignError,
    CampaignIdentity,
    CampaignStatus,
    EffectReceipt,
    FaultCampaign,
    GitRefEvidence,
    ObservationEnvelope,
    Owner,
    RecoveryRequired,
    Residue,
    RevisionConflict,
    authoritative_custody,
)

FIXTURE = Path(__file__).parent / "fixtures" / "h04-cross-component-faults-v1.json"
COMMIT_A = "a" * 40
COMMIT_B = "b" * 40
DIGEST = "c" * 64


def identity() -> CampaignIdentity:
    return CampaignIdentity(
        1,
        20260911,
        "h04-cross-component-20260911",
        "workflow-h04",
        "20260911T230000Z-facefeed",
        "attempt-h04",
        "thread-h04",
        "workspace-h04",
        "deployment-" + "d" * 32,
        DIGEST,
        "normandy",
    )


def campaign() -> FaultCampaign:
    return FaultCampaign(identity(), authoritative_custody())


def prepared_effect(value: FaultCampaign, owner: Owner = Owner.UPKEEPER) -> tuple[str, str]:
    action = "apply-target" if owner is Owner.UPKEEPER else "edit-source"
    revision = COMMIT_A
    effect_id = value.prepare_effect(
        expected_revision=value.revision,
        owner=owner,
        action=action,
        input_revision=revision,
    )
    value.start_effect(effect_id, expected_revision=value.revision)
    return effect_id, action


def receipt(
    value: FaultCampaign,
    effect_id: str,
    action: str,
    owner: Owner = Owner.UPKEEPER,
) -> EffectReceipt:
    return EffectReceipt(
        effect_id,
        value.identity.campaign_id,
        owner,
        action,
        COMMIT_A,
        DIGEST,
    )


def confirmed_campaign() -> FaultCampaign:
    value = campaign()
    effect_id, action = prepared_effect(value)
    value.confirm_effect(receipt(value, effect_id, action), expected_revision=value.revision)
    assert value.status is CampaignStatus.CONVERGED
    return value


def observation(
    value: FaultCampaign,
    *,
    observation_id: str = "observation-1",
    subject_revision: int = 7,
    sequence: int = 1,
    available: bool = True,
) -> ObservationEnvelope:
    return ObservationEnvelope(
        observation_id,
        value.identity.campaign_id,
        Owner.TARGET,
        Owner.UPKEEPER,
        subject_revision,
        sequence,
        DIGEST,
        available,
    )


def test_fixture_freezes_seed_owners_and_every_required_fault() -> None:
    data = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert data["schema_version"] == 1
    assert data["seed"] == 20260911
    assert data["owners"] == [owner.value for owner in Owner]
    assert {item["fault_id"] for item in data["scenarios"]} == {
        "steward-restart-during-rollout",
        "upkeeper-restart-during-steward-update",
        "lease-expiry-live-t3-thread",
        "huyang-unavailable",
        "dirty-worktree",
        "missing-worktree",
        "interrupted-verification",
        "interrupted-settlement",
        "worker-reboot",
        "delayed-observation",
        "stale-observation",
        "duplicated-observation",
        "missing-observation",
        "detached-success-unreachable-commit",
    }


def test_custody_is_complete_disjoint_and_sole_authority() -> None:
    value = campaign()
    assert [item.owner for item in value.custody] == list(Owner)
    assert value.events[0].data["identity"]["seed"] == 20260911

    with pytest.raises(CampaignError, match="no authority"):
        value.prepare_effect(
            expected_revision=value.revision,
            owner=Owner.T3,
            action="apply-target",
            input_revision=COMMIT_A,
        )

    duplicated = list(authoritative_custody())
    duplicated[1] = replace(
        duplicated[1],
        effect_authority=("apply-target", "settle-attempt"),
    )
    with pytest.raises(ValueError, match="authoritative"):
        FaultCampaign(identity(), tuple(duplicated))


def test_effect_is_recorded_before_execution_and_duplicate_calls_are_idempotent() -> None:
    value = campaign()
    effect_id = value.prepare_effect(
        expected_revision=value.revision,
        owner=Owner.UPKEEPER,
        action="apply-target",
        input_revision=COMMIT_A,
    )
    assert value.effects()[effect_id].data["state"] == "PREPARED"

    prepared_revision = value.revision
    value.start_effect(effect_id, expected_revision=prepared_revision)
    started_revision = value.revision
    with pytest.raises(RecoveryRequired, match="reconciled"):
        value.start_effect(effect_id, expected_revision=started_revision)
    assert value.revision == started_revision

    exact_receipt = receipt(value, effect_id, "apply-target")
    value.confirm_effect(exact_receipt, expected_revision=value.revision)
    confirmed_revision = value.revision
    value.confirm_effect(exact_receipt, expected_revision=confirmed_revision)
    assert value.revision == confirmed_revision
    assert value.status is CampaignStatus.CONVERGED

    assert (
        value.prepare_effect(
            expected_revision=value.revision,
            owner=Owner.UPKEEPER,
            action="apply-target",
            input_revision=COMMIT_A,
        )
        == effect_id
    )
    assert value.revision == confirmed_revision


@pytest.mark.parametrize("restart_owner", [Owner.STEWARD, Owner.UPKEEPER, Owner.TARGET])
def test_restarts_replay_hash_chain_and_do_not_duplicate_effects(restart_owner: Owner) -> None:
    value = campaign()
    effect_id, action = prepared_effect(value)
    restarted = value.restart()
    assert restarted.events == value.events
    assert restart_owner in Owner
    with pytest.raises(RecoveryRequired, match="reconciled"):
        restarted.start_effect(effect_id, expected_revision=restarted.revision)
    restarted.mark_unknown(
        effect_id,
        expected_revision=restarted.revision,
        reason=f"{restart_owner.value} restarted after dispatch",
    )
    restarted.reconcile(
        effect_id,
        expected_revision=restarted.revision,
        applied=True,
        receipt=receipt(restarted, effect_id, action),
        evidence_sha256=DIGEST,
    )
    assert restarted.status is CampaignStatus.CONVERGED
    assert len(restarted.effects()) == 1


def test_unknown_not_applied_reconciliation_permits_one_safe_replay() -> None:
    value = campaign()
    effect_id, action = prepared_effect(value)
    value.mark_unknown(effect_id, expected_revision=value.revision, reason="worker reboot")
    value.reconcile(
        effect_id,
        expected_revision=value.revision,
        applied=False,
        receipt=None,
        evidence_sha256=DIGEST,
    )
    assert value.status is CampaignStatus.ACTIVE
    value.start_effect(effect_id, expected_revision=value.revision)
    value.confirm_effect(
        receipt(value, effect_id, action),
        expected_revision=value.revision,
    )
    assert value.status is CampaignStatus.CONVERGED


def test_revision_fence_and_receipt_correlation_fail_closed() -> None:
    value = campaign()
    effect_id, action = prepared_effect(value)
    with pytest.raises(RevisionConflict):
        value.mark_unknown(effect_id, expected_revision=value.revision - 1, reason="late writer")
    wrong = replace(receipt(value, effect_id, action), campaign_id="foreign-campaign")
    with pytest.raises(RecoveryRequired, match="correlate"):
        value.confirm_effect(wrong, expected_revision=value.revision)


def test_delayed_stale_duplicated_and_missing_observations() -> None:
    value = campaign()
    delayed = observation(value, observation_id="delayed", sequence=9)
    assert (
        value.accept_observation(
            delayed,
            expected_revision=value.revision,
            expected_subject_revision=7,
        )
        == "current"
    )
    revision = value.revision
    assert (
        value.accept_observation(
            delayed,
            expected_revision=revision,
            expected_subject_revision=7,
        )
        == "duplicate"
    )
    assert value.revision == revision

    stale = observation(value, observation_id="stale", subject_revision=6)
    assert (
        value.accept_observation(
            stale,
            expected_revision=value.revision,
            expected_subject_revision=7,
        )
        == "stale"
    )
    missing = observation(value, observation_id="missing", available=False)
    assert (
        value.accept_observation(
            missing,
            expected_revision=value.revision,
            expected_subject_revision=7,
        )
        == "missing"
    )
    assert value.status is CampaignStatus.RECOVERY_REQUIRED


@pytest.mark.parametrize(
    ("fault_id", "reason"),
    [
        ("lease-expiry-live-t3-thread", "lease expired while T3 thread remains live"),
        ("huyang-unavailable", "Huyang unavailable; workspace state is unknown"),
        ("dirty-worktree", "Git worktree is dirty"),
        ("missing-worktree", "registered Git worktree is missing"),
        ("interrupted-verification", "verification was interrupted"),
        ("interrupted-settlement", "T3 settlement was interrupted"),
    ],
)
def test_ambiguous_cross_component_faults_report_recovery_required(
    fault_id: str, reason: str
) -> None:
    value = campaign()
    value.require_recovery(expected_revision=value.revision, reason=f"{fault_id}: {reason}")
    assert value.status is CampaignStatus.RECOVERY_REQUIRED
    assert value.events[-1].data["reason"].startswith(fault_id)


def test_routine_residue_is_cleaned_only_after_convergence() -> None:
    value = campaign()
    residue = Residue("temp-verification-1", Owner.HUYANG, "temporary-verification", True)
    with pytest.raises(RecoveryRequired, match="converge"):
        value.cleanup(residue, expected_revision=value.revision)

    value = confirmed_campaign()
    assert value.cleanup(residue, expected_revision=value.revision)
    assert value.events[-1].kind == "RESIDUE_CLEANED"

    absent = Residue("settlement-marker-1", Owner.T3, "settlement-marker", True, missing=True)
    assert value.cleanup(absent, expected_revision=value.revision)
    assert value.events[-1].data["already_absent"] is True


def test_dirty_or_foreign_residue_is_never_automatically_cleaned() -> None:
    value = confirmed_campaign()
    for residue in (
        Residue("dirty-worktree", Owner.GIT, "worktree", True, dirty=True),
        Residue("foreign-path", Owner.TARGET, "unexplained-path", False),
    ):
        with pytest.raises(RecoveryRequired, match="preserved"):
            value.cleanup(residue, expected_revision=value.revision)


def test_successful_detached_attempt_waits_for_every_commit_to_be_durably_reachable() -> None:
    value = confirmed_campaign()
    residue = Residue(
        "attempt-worktree",
        Owner.GIT,
        "detached-worktree",
        True,
        detached_attempt_succeeded=True,
        produced_commits=(COMMIT_A, COMMIT_B),
    )
    temporary = GitRefEvidence(
        "refs/t3/ephemeral/h04",
        COMMIT_B,
        (COMMIT_A, COMMIT_B),
        False,
        False,
    )
    with pytest.raises(RecoveryRequired, match="unreachable"):
        value.cleanup(
            residue,
            expected_revision=value.revision,
            git_refs=(temporary,),
        )
    assert not any(event.kind == "RESIDUE_CLEANED" for event in value.events)

    incomplete = GitRefEvidence(
        "refs/heads/recover/h04",
        COMMIT_A,
        (COMMIT_A,),
        True,
        True,
    )
    with pytest.raises(RecoveryRequired, match="unreachable"):
        value.cleanup(
            residue,
            expected_revision=value.revision,
            git_refs=(incomplete,),
        )

    durable = GitRefEvidence(
        "refs/heads/recover/h04",
        COMMIT_B,
        (COMMIT_A, COMMIT_B),
        True,
        True,
    )
    assert value.cleanup(
        residue,
        expected_revision=value.revision,
        git_refs=(durable,),
    )
    assert value.events[-1].data["residue_id"] == "attempt-worktree"


@pytest.mark.parametrize(
    "scenario",
    json.loads(FIXTURE.read_text(encoding="utf-8"))["scenarios"],
    ids=lambda item: item["fault_id"],
)
def test_every_fixed_seed_scenario_reaches_its_frozen_outcome(scenario: dict[str, str]) -> None:
    value = campaign()
    fault_id = scenario["fault_id"]
    expected = CampaignStatus(scenario["expected"])

    if expected is CampaignStatus.RECOVERY_REQUIRED:
        if fault_id == "detached-success-unreachable-commit":
            value = confirmed_campaign()
            residue = Residue(
                "detached-fixture",
                Owner.GIT,
                "detached-worktree",
                True,
                detached_attempt_succeeded=True,
                produced_commits=(COMMIT_A,),
            )
            with pytest.raises(RecoveryRequired):
                value.cleanup(residue, expected_revision=value.revision)
            actual = CampaignStatus.RECOVERY_REQUIRED
        else:
            value.require_recovery(
                expected_revision=value.revision,
                reason=f"{fault_id}: deterministic injected ambiguity",
            )
            actual = value.status
    else:
        effect_id, action = prepared_effect(value)
        if scenario["fault"] == "restart":
            value = value.restart()
            value.mark_unknown(
                effect_id,
                expected_revision=value.revision,
                reason=f"{fault_id}: restart after dispatch",
            )
            value.reconcile(
                effect_id,
                expected_revision=value.revision,
                applied=True,
                receipt=receipt(value, effect_id, action),
                evidence_sha256=DIGEST,
            )
        else:
            value.confirm_effect(
                receipt(value, effect_id, action),
                expected_revision=value.revision,
            )
            envelope = observation(
                value,
                observation_id=fault_id,
                subject_revision=6 if scenario["fault"] == "stale" else 7,
                sequence=9 if scenario["fault"] == "delayed" else 1,
            )
            value.accept_observation(
                envelope,
                expected_revision=value.revision,
                expected_subject_revision=7,
            )
            if scenario["fault"] == "duplicate":
                value.accept_observation(
                    envelope,
                    expected_revision=value.revision,
                    expected_subject_revision=7,
                )
        actual = value.status

    assert actual is expected
