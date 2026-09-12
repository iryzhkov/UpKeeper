"""Deterministic cross-component fault-campaign primitives.

This module models authority and evidence.  It does not execute deployments, restart
services, mutate repositories, or contact any control-plane component.
"""

from __future__ import annotations

import hashlib
import json
from collections.abc import Mapping
from dataclasses import asdict, dataclass
from enum import StrEnum
from typing import Any

from .validation import validate_git_commit, validate_run_id


class CampaignError(RuntimeError):
    """A campaign transition was refused."""


class RevisionConflict(CampaignError):
    """The caller did not use the current campaign revision."""


class RecoveryRequired(CampaignError):
    """Evidence is ambiguous and an operator must reconcile it."""


class Owner(StrEnum):
    STEWARD = "t3-steward"
    T3 = "T3"
    HUYANG = "Huyang"
    UPKEEPER = "UpKeeper"
    GIT = "Git"
    TARGET = "target"


class EffectState(StrEnum):
    PREPARED = "PREPARED"
    STARTED = "STARTED"
    CONFIRMED = "CONFIRMED"
    UNKNOWN = "UNKNOWN"
    RECONCILED = "RECONCILED"


class CampaignStatus(StrEnum):
    ACTIVE = "ACTIVE"
    CONVERGED = "CONVERGED"
    RECOVERY_REQUIRED = "RECOVERY_REQUIRED"


@dataclass(frozen=True)
class CampaignIdentity:
    schema_version: int
    seed: int
    campaign_id: str
    workflow_id: str
    run_id: str
    attempt_id: str
    thread_id: str
    workspace_id: str
    deployment_id: str
    plan_hash: str
    target_id: str

    def __post_init__(self) -> None:
        if self.schema_version != 1 or self.seed < 0:
            raise ValueError("campaign identity requires schema version 1 and a nonnegative seed")
        validate_run_id(self.run_id)
        required = (
            self.campaign_id,
            self.workflow_id,
            self.attempt_id,
            self.thread_id,
            self.workspace_id,
            self.deployment_id,
            self.plan_hash,
            self.target_id,
        )
        if any(not item or "\x00" in item for item in required):
            raise ValueError("every causal identity must be nonempty and NUL-free")


@dataclass(frozen=True)
class CustodyBoundary:
    owner: Owner
    resources: tuple[str, ...]
    effect_authority: tuple[str, ...]

    def __post_init__(self) -> None:
        if not self.resources or tuple(sorted(set(self.resources))) != self.resources:
            raise ValueError("custody resources must be nonempty, sorted, and unique")
        if tuple(sorted(set(self.effect_authority))) != self.effect_authority:
            raise ValueError("effect authority must be sorted and unique")


REQUIRED_CUSTODY: Mapping[Owner, tuple[tuple[str, ...], tuple[str, ...]]] = {
    Owner.STEWARD: (
        ("lease", "schedule", "workflow"),
        ("admit-attempt", "renew-lease"),
    ),
    Owner.T3: (
        ("attempt", "settlement", "thread"),
        ("settle-attempt",),
    ),
    Owner.HUYANG: (
        ("source-revision", "source-transaction", "workspace"),
        ("edit-source", "verify-source"),
    ),
    Owner.UPKEEPER: (
        ("deployment-journal", "deployment-lock", "rollback"),
        ("apply-target", "rollback-target"),
    ),
    Owner.GIT: (
        ("commit", "durable-ref", "repository", "worktree"),
        ("create-commit", "move-local-ref"),
    ),
    Owner.TARGET: (
        ("observed-runtime",),
        (),
    ),
}


def authoritative_custody() -> tuple[CustodyBoundary, ...]:
    return tuple(
        CustodyBoundary(owner, resources, authority)
        for owner, (resources, authority) in REQUIRED_CUSTODY.items()
    )


@dataclass(frozen=True)
class CampaignEvent:
    schema_version: int
    revision: int
    kind: str
    previous_sha256: str | None
    data: Mapping[str, Any]
    event_sha256: str


@dataclass(frozen=True)
class EffectReceipt:
    effect_id: str
    campaign_id: str
    owner: Owner
    action: str
    input_revision: str
    result_sha256: str

    def __post_init__(self) -> None:
        _validate_digest(self.result_sha256)


@dataclass(frozen=True)
class ObservationEnvelope:
    observation_id: str
    campaign_id: str
    producer: Owner
    subject_owner: Owner
    subject_revision: int
    sequence: int
    evidence_sha256: str
    available: bool = True

    def __post_init__(self) -> None:
        if not self.observation_id or self.subject_revision < 0 or self.sequence < 0:
            raise ValueError("invalid observation identity")
        _validate_digest(self.evidence_sha256)


@dataclass(frozen=True)
class GitRefEvidence:
    ref: str
    tip: str
    reachable_commits: tuple[str, ...]
    durable: bool
    successor_addressable: bool

    def __post_init__(self) -> None:
        if not self.ref.startswith("refs/"):
            raise ValueError("Git evidence requires a fully qualified ref")
        validate_git_commit(self.tip)
        if not self.reachable_commits:
            raise ValueError("Git evidence requires an explicit reachability set")
        for commit in self.reachable_commits:
            validate_git_commit(commit)


@dataclass(frozen=True)
class Residue:
    residue_id: str
    owner: Owner
    kind: str
    routine: bool
    dirty: bool = False
    missing: bool = False
    detached_attempt_succeeded: bool = False
    produced_commits: tuple[str, ...] = ()

    def __post_init__(self) -> None:
        if not self.residue_id or not self.kind:
            raise ValueError("residue identity and kind are required")
        for commit in self.produced_commits:
            validate_git_commit(commit)
        if self.detached_attempt_succeeded and not self.produced_commits:
            raise ValueError("a successful detached attempt must identify produced commits")


class FaultCampaign:
    """Hash-chained, revision-fenced authority and evidence ledger."""

    def __init__(
        self,
        identity: CampaignIdentity,
        custody: tuple[CustodyBoundary, ...],
        events: tuple[CampaignEvent, ...] = (),
    ) -> None:
        self.identity = identity
        self.custody = custody
        self._validate_custody()
        self._events = events or (
            _event(
                0,
                "CAMPAIGN_FROZEN",
                None,
                {
                    "identity": asdict(identity),
                    "custody": [asdict(item) for item in custody],
                },
            ),
        )
        self._validate_chain()

    @property
    def events(self) -> tuple[CampaignEvent, ...]:
        return self._events

    @property
    def revision(self) -> int:
        return self._events[-1].revision

    @property
    def status(self) -> CampaignStatus:
        if any(event.kind == "RECOVERY_REQUIRED" for event in self._events):
            return CampaignStatus.RECOVERY_REQUIRED
        effects = self.effects()
        if effects and all(
            event.data["state"] == EffectState.CONFIRMED
            or (
                event.data["state"] == EffectState.RECONCILED
                and event.data.get("applied") is True
            )
            for event in effects.values()
        ):
            return CampaignStatus.CONVERGED
        return CampaignStatus.ACTIVE

    def restart(self) -> FaultCampaign:
        """Rebuild solely from durable events, simulating any component restart."""
        return FaultCampaign(self.identity, self.custody, self._events)

    def effects(self) -> dict[str, CampaignEvent]:
        latest: dict[str, CampaignEvent] = {}
        for event in self._events:
            if event.kind == "EFFECT":
                latest[str(event.data["effect_id"])] = event
        return latest

    def prepare_effect(
        self,
        *,
        expected_revision: int,
        owner: Owner,
        action: str,
        input_revision: str,
    ) -> str:
        self._expect(expected_revision)
        self._require_authority(owner, action)
        effect_id = _effect_id(self.identity, owner, action, input_revision)
        existing = self.effects().get(effect_id)
        if existing is not None:
            if (
                existing.data["owner"] != owner
                or existing.data["action"] != action
                or existing.data["input_revision"] != input_revision
            ):
                raise RecoveryRequired("effect identity collision")
            return effect_id
        self._append(
            "EFFECT",
            {
                "effect_id": effect_id,
                "owner": owner,
                "action": action,
                "input_revision": input_revision,
                "state": EffectState.PREPARED,
            },
        )
        return effect_id

    def start_effect(self, effect_id: str, *, expected_revision: int) -> None:
        self._expect(expected_revision)
        effect = self._effect(effect_id)
        state = EffectState(effect.data["state"])
        if state is EffectState.CONFIRMED:
            return
        if state is EffectState.STARTED:
            raise RecoveryRequired("STARTED effect must be reconciled before replay")
        if state is EffectState.RECONCILED and effect.data.get("applied") is True:
            return
        if state is EffectState.UNKNOWN:
            raise RecoveryRequired("UNKNOWN effect must be reconciled before replay")
        self._append_effect(effect, EffectState.STARTED)

    def confirm_effect(
        self, receipt: EffectReceipt, *, expected_revision: int
    ) -> None:
        self._expect(expected_revision)
        effect = self._effect(receipt.effect_id)
        self._validate_receipt(effect, receipt)
        state = EffectState(effect.data["state"])
        if state is EffectState.CONFIRMED:
            previous = effect.data.get("receipt")
            if previous != asdict(receipt):
                raise RecoveryRequired("duplicated effect has contradictory receipts")
            return
        if state is not EffectState.STARTED:
            raise RecoveryRequired("only a recorded STARTED effect may be confirmed")
        self._append_effect(effect, EffectState.CONFIRMED, {"receipt": asdict(receipt)})

    def mark_unknown(
        self, effect_id: str, *, expected_revision: int, reason: str
    ) -> None:
        self._expect(expected_revision)
        effect = self._effect(effect_id)
        state = EffectState(effect.data["state"])
        if state is EffectState.UNKNOWN:
            return
        if state is not EffectState.STARTED:
            raise RevisionConflict("only a STARTED effect can become UNKNOWN")
        self._append_effect(effect, EffectState.UNKNOWN, {"reason": reason})

    def reconcile(
        self,
        effect_id: str,
        *,
        expected_revision: int,
        applied: bool | None,
        receipt: EffectReceipt | None,
        evidence_sha256: str,
    ) -> None:
        self._expect(expected_revision)
        _validate_digest(evidence_sha256)
        effect = self._effect(effect_id)
        if EffectState(effect.data["state"]) is not EffectState.UNKNOWN:
            raise RevisionConflict("reconciliation requires the current UNKNOWN effect")
        if applied is None:
            self.require_recovery(
                expected_revision=self.revision,
                reason=f"{effect_id}: observation cannot determine whether effect applied",
            )
            return
        if applied:
            if receipt is None:
                raise RecoveryRequired("applied reconciliation requires an exact receipt")
            self._validate_receipt(effect, receipt)
        elif receipt is not None:
            raise RecoveryRequired("not-applied reconciliation cannot include an effect receipt")
        extra: dict[str, Any] = {
            "applied": applied,
            "evidence_sha256": evidence_sha256,
        }
        if receipt is not None:
            extra["receipt"] = asdict(receipt)
        self._append_effect(effect, EffectState.RECONCILED, extra)

    def accept_observation(
        self,
        observation: ObservationEnvelope,
        *,
        expected_revision: int,
        expected_subject_revision: int,
    ) -> str:
        self._expect(expected_revision)
        if observation.campaign_id != self.identity.campaign_id:
            raise RecoveryRequired("observation has a foreign causal identity")
        duplicates = [
            event
            for event in self._events
            if event.kind == "OBSERVATION"
            and event.data["observation"]["observation_id"] == observation.observation_id
        ]
        payload = asdict(observation)
        if duplicates:
            if duplicates[-1].data["observation"] != payload:
                raise RecoveryRequired("duplicate observation identity has conflicting content")
            return "duplicate"
        if not observation.available:
            self.require_recovery(
                expected_revision=self.revision,
                reason=f"{observation.observation_id}: observation unavailable",
            )
            return "missing"
        if observation.subject_revision < expected_subject_revision:
            self._append("OBSERVATION", {"disposition": "stale", "observation": payload})
            return "stale"
        if observation.subject_revision > expected_subject_revision:
            self.require_recovery(
                expected_revision=self.revision,
                reason=f"{observation.observation_id}: future observation is ambiguous",
            )
            return "future"
        self._append("OBSERVATION", {"disposition": "current", "observation": payload})
        return "current"

    def require_recovery(self, *, expected_revision: int, reason: str) -> None:
        self._expect(expected_revision)
        if not reason:
            raise ValueError("recovery reason is required")
        self._append("RECOVERY_REQUIRED", {"reason": reason})

    def cleanup(
        self,
        residue: Residue,
        *,
        expected_revision: int,
        git_refs: tuple[GitRefEvidence, ...] = (),
    ) -> bool:
        self._expect(expected_revision)
        if self.status is not CampaignStatus.CONVERGED:
            raise RecoveryRequired("campaign must converge before routine cleanup")
        if not residue.routine or residue.dirty:
            raise RecoveryRequired("ambiguous or foreign residue must be preserved")
        if residue.missing:
            self._append(
                "RESIDUE_CLEANED",
                {"residue_id": residue.residue_id, "already_absent": True},
            )
            return True
        if residue.detached_attempt_succeeded:
            qualified = [
                item for item in git_refs if item.durable and item.successor_addressable
            ]
            unreachable = [
                commit
                for commit in residue.produced_commits
                if not any(commit in item.reachable_commits for item in qualified)
            ]
            if unreachable:
                raise RecoveryRequired(
                    "successful detached attempt has commits unreachable from a "
                    "durable successor-addressable Git ref"
                )
        self._append(
            "RESIDUE_CLEANED",
            {"residue_id": residue.residue_id, "already_absent": False},
        )
        return True

    def _validate_custody(self) -> None:
        if tuple(item.owner for item in self.custody) != tuple(Owner):
            raise ValueError("custody must name every owner exactly once in canonical order")
        resources: set[str] = set()
        actions: set[str] = set()
        for boundary in self.custody:
            expected = REQUIRED_CUSTODY[boundary.owner]
            if (boundary.resources, boundary.effect_authority) != expected:
                raise ValueError(f"custody for {boundary.owner} is not authoritative")
            if resources.intersection(boundary.resources):
                raise ValueError("a resource cannot have multiple custodians")
            if actions.intersection(boundary.effect_authority):
                raise ValueError("an effect cannot have multiple authorities")
            resources.update(boundary.resources)
            actions.update(boundary.effect_authority)

    def _validate_chain(self) -> None:
        previous: str | None = None
        for revision, event in enumerate(self._events):
            if (
                event.schema_version != 1
                or event.revision != revision
                or event.previous_sha256 != previous
                or event.event_sha256 != _event_digest(event)
            ):
                raise RecoveryRequired(f"campaign event chain failed at revision {revision}")
            previous = event.event_sha256
        frozen = self._events[0]
        expected = {
            "identity": asdict(self.identity),
            "custody": [asdict(item) for item in self.custody],
        }
        if frozen.kind != "CAMPAIGN_FROZEN" or frozen.data != expected:
            raise RecoveryRequired("campaign identity or custody changed after freezing")

    def _expect(self, revision: int) -> None:
        if revision != self.revision:
            raise RevisionConflict(
                f"expected campaign revision {revision}, current is {self.revision}"
            )

    def _require_authority(self, owner: Owner, action: str) -> None:
        boundary = next(item for item in self.custody if item.owner is owner)
        if action not in boundary.effect_authority:
            raise CampaignError(f"{owner} has no authority for {action}")

    def _effect(self, effect_id: str) -> CampaignEvent:
        try:
            return self.effects()[effect_id]
        except KeyError:
            raise RevisionConflict(f"unknown effect {effect_id}") from None

    def _validate_receipt(self, effect: CampaignEvent, receipt: EffectReceipt) -> None:
        expected = {
            "effect_id": effect.data["effect_id"],
            "campaign_id": self.identity.campaign_id,
            "owner": effect.data["owner"],
            "action": effect.data["action"],
            "input_revision": effect.data["input_revision"],
        }
        actual = {
            "effect_id": receipt.effect_id,
            "campaign_id": receipt.campaign_id,
            "owner": receipt.owner,
            "action": receipt.action,
            "input_revision": receipt.input_revision,
        }
        if actual != expected:
            raise RecoveryRequired("receipt does not correlate to the frozen effect")

    def _append_effect(
        self,
        effect: CampaignEvent,
        state: EffectState,
        extra: Mapping[str, Any] | None = None,
    ) -> None:
        data = {
            "effect_id": effect.data["effect_id"],
            "owner": effect.data["owner"],
            "action": effect.data["action"],
            "input_revision": effect.data["input_revision"],
            "state": state,
        }
        if extra:
            data.update(extra)
        self._append("EFFECT", data)

    def _append(self, kind: str, data: Mapping[str, Any]) -> None:
        self._events += (
            _event(self.revision + 1, kind, self._events[-1].event_sha256, data),
        )


def _effect_id(
    identity: CampaignIdentity, owner: Owner, action: str, input_revision: str
) -> str:
    payload = (
        f"{identity.campaign_id}\x00{identity.attempt_id}\x00{owner.value}"
        f"\x00{action}\x00{input_revision}"
    ).encode()
    return "effect-" + hashlib.sha256(payload).hexdigest()


def _event(
    revision: int,
    kind: str,
    previous_sha256: str | None,
    data: Mapping[str, Any],
) -> CampaignEvent:
    provisional = CampaignEvent(1, revision, kind, previous_sha256, data, "")
    return CampaignEvent(
        provisional.schema_version,
        provisional.revision,
        provisional.kind,
        provisional.previous_sha256,
        provisional.data,
        _event_digest(provisional),
    )


def _event_digest(event: CampaignEvent) -> str:
    value = asdict(event)
    value.pop("event_sha256")
    payload = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(payload).hexdigest()


def _validate_digest(value: str) -> None:
    if len(value) != 64 or any(char not in "0123456789abcdef" for char in value):
        raise ValueError("expected a lowercase SHA-256 digest")
