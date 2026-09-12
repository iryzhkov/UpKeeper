"""Frozen F05 agent-environment contract and read-only assessment helpers.

UpKeeper owns deployment progression.  This module supplies the immutable payload
that is included in its plan hash plus fail-closed drift, probe, and recovery
inventory projections.  It never writes a managed target.
"""

from __future__ import annotations

import hashlib
import json
from collections.abc import Mapping, Sequence
from dataclasses import asdict, dataclass
from enum import StrEnum
from pathlib import Path
from typing import Any

_HEX = frozenset("0123456789abcdef")
_PHASES = ("install", "activate", "probe", "rollback")
_ASSET_KINDS = frozenset(
    {
        "mcp-service",
        "mcp-client",
        "skill",
        "plugin",
        "harness-setting",
        "global-instruction",
    }
)
_RECOVERY_KINDS = frozenset({"lease", "t3-thread", "huyang-journal", "worktree"})
_SECRET_REF_PREFIX = "secretref:"  # noqa: S105
_REQUIRED_SAFETY_CLAIMS = frozenset(
    {
        "explicit-effect-authority",
        "instruction-safety",
        "preserve-unexplained-changes",
        "skill-safety",
    }
)


def _digest(value: str, field: str) -> str:
    if len(value) != 64 or any(char not in _HEX for char in value):
        raise ValueError(f"{field} must be a lowercase SHA-256 digest")
    return value


def _safe_text(value: str, field: str) -> str:
    if not value or "\x00" in value or len(value) > 1024:
        raise ValueError(f"{field} must be nonempty bounded text")
    return value


def _absolute(value: str, field: str) -> str:
    path = Path(value)
    if not path.is_absolute() or ".." in path.parts or str(path) != value:
        raise ValueError(f"{field} must be an absolute normalized path")
    return str(path)


def _relative(value: str, field: str) -> str:
    path = Path(value)
    if (
        not value
        or path.is_absolute()
        or ".." in path.parts
        or "." in path.parts
        or str(path) != value
    ):
        raise ValueError(f"{field} must be a normalized relative path")
    return str(path)


def _strict(raw: Mapping[str, Any], fields: frozenset[str], context: str) -> dict[str, Any]:
    value = dict(raw)
    unknown = set(value) - fields
    missing = fields - set(value)
    if unknown or missing:
        raise ValueError(
            f"{context} fields mismatch: missing={sorted(missing)} unknown={sorted(unknown)}"
        )
    return value


@dataclass(frozen=True)
class ManagedAsset:
    asset_id: str
    kind: str
    provenance: str
    source_revision: str
    sha256: str
    size_bytes: int
    staged_path: str
    active_path: str
    activation_order: int

    def __post_init__(self) -> None:
        _safe_text(self.asset_id, "asset_id")
        if self.kind not in _ASSET_KINDS:
            raise ValueError("unsupported managed asset kind")
        _safe_text(self.provenance, "provenance")
        _safe_text(self.source_revision, "source_revision")
        _digest(self.sha256, "asset sha256")
        if self.size_bytes < 0:
            raise ValueError("asset size_bytes must be nonnegative")
        _absolute(self.staged_path, "staged_path")
        _absolute(self.active_path, "active_path")
        if Path(self.staged_path) == Path(self.active_path):
            raise ValueError("install staging must be distinct from activation")
        if self.activation_order < 0:
            raise ValueError("activation_order must be nonnegative")


@dataclass(frozen=True)
class PriorAsset:
    """Exact active identity in the coherent rollback environment."""

    asset_id: str
    sha256: str
    size_bytes: int
    active_path: str

    def __post_init__(self) -> None:
        _safe_text(self.asset_id, "prior asset_id")
        _digest(self.sha256, "prior asset sha256")
        if self.size_bytes < 0:
            raise ValueError("prior asset size_bytes must be nonnegative")
        _absolute(self.active_path, "prior active_path")


@dataclass(frozen=True)
class ProjectInstructionRef:
    repository: str
    repository_revision: str
    path: str
    sha256: str

    def __post_init__(self) -> None:
        _safe_text(self.repository, "instruction repository")
        if len(self.repository_revision) != 40 or any(
            char not in _HEX for char in self.repository_revision
        ):
            raise ValueError("repository_revision must be a full Git object id")
        _relative(self.path, "instruction path")
        _digest(self.sha256, "instruction sha256")


@dataclass(frozen=True)
class BehavioralProbe:
    probe_id: str
    argv: tuple[str, ...]
    timeout_seconds: int
    safety_claims: tuple[str, ...]

    def __post_init__(self) -> None:
        _safe_text(self.probe_id, "probe_id")
        if not self.argv or not Path(self.argv[0]).is_absolute():
            raise ValueError(
                "probe argv must be a nonempty argument vector with absolute executable"
            )
        for item in self.argv:
            _safe_text(item, "probe argv item")
        if not 1 <= self.timeout_seconds <= 300:
            raise ValueError("probe timeout must be between 1 and 300 seconds")
        if not self.safety_claims or tuple(sorted(set(self.safety_claims))) != self.safety_claims:
            raise ValueError("safety_claims must be nonempty, unique, and sorted")
        for claim in self.safety_claims:
            _safe_text(claim, "safety claim")


@dataclass(frozen=True)
class RecoverySource:
    kind: str
    owner: str
    root: str

    def __post_init__(self) -> None:
        if self.kind not in _RECOVERY_KINDS:
            raise ValueError("unsupported recovery source kind")
        _safe_text(self.owner, "recovery owner")
        _absolute(self.root, "recovery root")


@dataclass(frozen=True)
class AgentEnvironmentSpec:
    schema_version: int
    adapter: str
    host_profile: str
    worker_ids: tuple[str, ...]
    phases: tuple[str, ...]
    assets: tuple[ManagedAsset, ...]
    project_instructions: tuple[ProjectInstructionRef, ...]
    probes: tuple[BehavioralProbe, ...]
    recovery_sources: tuple[RecoverySource, ...]
    secret_refs: tuple[str, ...]
    previous_environment_revision: str
    prior_assets: tuple[PriorAsset, ...]

    def __post_init__(self) -> None:
        if self.schema_version != 1:
            raise ValueError("AgentEnvironmentSpec schema_version must be 1")
        if self.adapter != "local-single-worker-v1":
            raise ValueError("F05 supports only the explicit local-single-worker-v1 adapter")
        _safe_text(self.host_profile, "host_profile")
        if len(self.worker_ids) != 1:
            raise ValueError("the provisional F05 adapter requires exactly one worker")
        _safe_text(self.worker_ids[0], "worker_id")
        if self.phases != _PHASES:
            raise ValueError("environment phases must be install, activate, probe, rollback")
        for label, values in (
            ("asset", tuple(item.asset_id for item in self.assets)),
            ("probe", tuple(item.probe_id for item in self.probes)),
            ("recovery source", tuple(item.kind for item in self.recovery_sources)),
        ):
            if not values or tuple(sorted(set(values))) != values:
                raise ValueError(f"{label} identities must be nonempty, unique, and sorted")
        orders = tuple(item.activation_order for item in self.assets)
        if orders != tuple(sorted(orders)):
            raise ValueError("assets must be sorted by activation_order")
        if len({item.active_path for item in self.assets}) != len(self.assets):
            raise ValueError("active paths must be unique")
        if len({item.staged_path for item in self.assets}) != len(self.assets):
            raise ValueError("staged paths must be unique")
        if any(
            _paths_overlap(staged.staged_path, active.active_path)
            for staged in self.assets
            for active in self.assets
        ):
            raise ValueError("all install staging paths must be distinct from active paths")
        _digest(self.previous_environment_revision, "previous_environment_revision")
        prior_ids = tuple(item.asset_id for item in self.prior_assets)
        if prior_ids != tuple(item.asset_id for item in self.assets):
            raise ValueError("prior assets must exactly cover desired assets in canonical order")
        if tuple(item.active_path for item in self.prior_assets) != tuple(
            item.active_path for item in self.assets
        ):
            raise ValueError("prior assets must restore the same active paths")
        instruction_ids = tuple(_instruction_id(item) for item in self.project_instructions)
        if not instruction_ids or tuple(sorted(set(instruction_ids))) != instruction_ids:
            raise ValueError("project instruction identities must be nonempty, unique, and sorted")
        safety_claims = {
            claim for probe in self.probes for claim in probe.safety_claims
        }
        if not _REQUIRED_SAFETY_CLAIMS <= safety_claims:
            raise ValueError("fixed probes do not cover required instruction and skill safety")
        for ref in self.secret_refs:
            _safe_text(ref, "secret reference")
            suffix = ref.removeprefix(_SECRET_REF_PREFIX)
            if not ref.startswith(_SECRET_REF_PREFIX) or not suffix or any(
                not (char.isalnum() or char in "-._/") for char in suffix
            ):
                raise ValueError("only opaque secretref: identifiers are allowed")
        if tuple(sorted(set(self.secret_refs))) != self.secret_refs:
            raise ValueError("secret_refs must be unique and sorted")

    @property
    def revision(self) -> str:
        return hashlib.sha256(canonical_environment_bytes(self)).hexdigest()


class DriftState(StrEnum):
    MATCH = "MATCH"
    MISMATCH = "MISMATCH"
    MISSING = "MISSING"
    UNAVAILABLE = "UNAVAILABLE"
    UNEXPLAINED = "UNEXPLAINED"


@dataclass(frozen=True)
class DriftObservation:
    subject: str
    state: DriftState
    expected_sha256: str | None
    observed_sha256: str | None
    evidence: str


@dataclass(frozen=True)
class ProbeResult:
    probe_id: str
    passed: bool
    safety_claims: tuple[str, ...]
    evidence_sha256: str

    def __post_init__(self) -> None:
        _safe_text(self.probe_id, "probe result id")
        if not isinstance(self.passed, bool):
            raise ValueError("probe result passed must be boolean")
        if tuple(sorted(set(self.safety_claims))) != self.safety_claims:
            raise ValueError("probe result safety_claims must be unique and sorted")
        for claim in self.safety_claims:
            _safe_text(claim, "probe result safety claim")
        _digest(self.evidence_sha256, "probe evidence sha256")


@dataclass(frozen=True)
class RecoveryObservation:
    kind: str
    owner: str
    status: str
    identities: tuple[str, ...]
    evidence_sha256: str

    def __post_init__(self) -> None:
        if self.kind not in _RECOVERY_KINDS:
            raise ValueError("unsupported recovery observation kind")
        _digest(self.evidence_sha256, "recovery evidence sha256")


def canonical_environment_bytes(spec: AgentEnvironmentSpec) -> bytes:
    return (json.dumps(asdict(spec), sort_keys=True, separators=(",", ":")) + "\n").encode()


def environment_to_dict(spec: AgentEnvironmentSpec) -> dict[str, Any]:
    value = asdict(spec)
    for field in (
        "worker_ids",
        "phases",
        "assets",
        "project_instructions",
        "probes",
        "recovery_sources",
        "secret_refs",
        "prior_assets",
    ):
        value[field] = list(value[field])
    for probe in value["probes"]:
        probe["argv"] = list(probe["argv"])
        probe["safety_claims"] = list(probe["safety_claims"])
    return value


def _instruction_id(ref: ProjectInstructionRef) -> str:
    return f"{ref.repository}@{ref.repository_revision}:{ref.path}"


def _paths_overlap(left: str, right: str) -> bool:
    left_path = Path(left)
    right_path = Path(right)
    return (
        left_path == right_path
        or left_path in right_path.parents
        or right_path in left_path.parents
    )


def environment_from_dict(raw: Mapping[str, Any]) -> AgentEnvironmentSpec:
    fields = frozenset(
        {
            "schema_version",
            "adapter",
            "host_profile",
            "worker_ids",
            "phases",
            "assets",
            "project_instructions",
            "probes",
            "recovery_sources",
            "secret_refs",
            "previous_environment_revision",
            "prior_assets",
        }
    )
    value = _strict(raw, fields, "AgentEnvironmentSpec")
    value["worker_ids"] = tuple(value["worker_ids"])
    value["phases"] = tuple(value["phases"])
    value["secret_refs"] = tuple(value["secret_refs"])
    value["assets"] = tuple(ManagedAsset(**item) for item in value["assets"])
    value["project_instructions"] = tuple(
        ProjectInstructionRef(**item) for item in value["project_instructions"]
    )
    value["probes"] = tuple(
        BehavioralProbe(
            **{
                **item,
                "argv": tuple(item["argv"]),
                "safety_claims": tuple(item["safety_claims"]),
            }
        )
        for item in value["probes"]
    )
    value["recovery_sources"] = tuple(RecoverySource(**item) for item in value["recovery_sources"])
    value["prior_assets"] = tuple(PriorAsset(**item) for item in value["prior_assets"])
    return AgentEnvironmentSpec(**value)


def classify_asset_drift(
    spec: AgentEnvironmentSpec,
    observed: Mapping[str, tuple[str | None, str]],
    unexplained_paths: Sequence[str] = (),
) -> tuple[DriftObservation, ...]:
    """Project observations without reading or changing targets.

    observed maps an asset id to (digest, evidence). A digest of None means the
    authoritative observer reports the target missing. Missing observation means
    unavailable, while any unrelated path is retained as unexplained.
    """
    results: list[DriftObservation] = []
    for asset in spec.assets:
        item = observed.get(asset.asset_id)
        if item is None:
            results.append(
                DriftObservation(asset.asset_id, DriftState.UNAVAILABLE, asset.sha256, None, "")
            )
        elif item[0] is None:
            results.append(
                DriftObservation(asset.asset_id, DriftState.MISSING, asset.sha256, None, item[1])
            )
        else:
            state = DriftState.MATCH if item[0] == asset.sha256 else DriftState.MISMATCH
            results.append(DriftObservation(asset.asset_id, state, asset.sha256, item[0], item[1]))
    for path in sorted(set(unexplained_paths)):
        results.append(DriftObservation(path, DriftState.UNEXPLAINED, None, None, "preserved"))
    return tuple(results)


def behavior_is_equivalent_or_stricter(
    probes: Sequence[BehavioralProbe],
    baseline: Sequence[ProbeResult],
    candidate: Sequence[ProbeResult],
) -> bool:
    required = {item.probe_id: set(item.safety_claims) for item in probes}
    old = {item.probe_id: item for item in baseline}
    new = {item.probe_id: item for item in candidate}
    if set(old) != set(required) or set(new) != set(required):
        return False
    return all(
        old[name].passed
        and new[name].passed
        and required[name] <= set(old[name].safety_claims)
        and set(old[name].safety_claims) <= set(new[name].safety_claims)
        for name in required
    )


def environment_phase_verdict(
    spec: AgentEnvironmentSpec,
    phase: str,
    raw: Any,
) -> bool | None:
    """Validate exact, phase-specific environment evidence.

    ``None`` is ambiguity: missing/malformed evidence or an unexplained path must
    retain the target lock for observation.  ``False`` is an explained mismatch
    that may use the separately journaled rollback transition.
    """
    if phase not in {"install", "activate", "run", "observe", "rollback"}:
        raise ValueError(f"unsupported environment phase: {phase}")
    if not isinstance(raw, Mapping):
        return None
    try:
        value = _strict(
            raw,
            frozenset(
                {
                    "schema_version",
                    "host_profile",
                    "worker_id",
                    "installed_environment_revision",
                    "active_environment_revision",
                    "worker_ready",
                    "installed_assets",
                    "active_assets",
                    "project_instructions",
                    "baseline_probes",
                    "candidate_probes",
                    "unexplained_paths",
                    "preserved_unexplained_paths",
                }
            ),
            "environment evidence",
        )
        if value["schema_version"] != 1:
            return None
        if value["host_profile"] != spec.host_profile or value["worker_id"] != spec.worker_ids[0]:
            return False
        unexplained = value["unexplained_paths"]
        preserved = value["preserved_unexplained_paths"]
        if (
            not isinstance(unexplained, list)
            or any(not isinstance(path, str) or not path for path in unexplained)
            or not isinstance(preserved, list)
            or any(not isinstance(path, str) or not path for path in preserved)
        ):
            return None
        if unexplained != sorted(set(unexplained)) or preserved != sorted(set(preserved)):
            return None
        for path in (*unexplained, *preserved):
            _absolute(path, "unexplained path")
        if unexplained and (phase != "rollback" or preserved != unexplained):
            return None
        if phase != "rollback" and preserved:
            return None
        baseline = _probe_results(value["baseline_probes"])
        candidate = _probe_results(value["candidate_probes"])
        if baseline is None or candidate is None:
            return None
        instructions = value["project_instructions"]
        if not isinstance(instructions, Mapping) or dict(instructions) != {
            _instruction_id(item): item.sha256 for item in spec.project_instructions
        }:
            return False

        forward = phase != "rollback"
        installed_revision = spec.revision if forward else spec.previous_environment_revision
        active_revision = (
            spec.previous_environment_revision
            if phase == "install"
            else spec.revision
            if forward
            else spec.previous_environment_revision
        )
        installed_assets = spec.assets if forward else spec.prior_assets
        active_assets = spec.prior_assets if phase == "install" else installed_assets
        checks = (
            value["installed_environment_revision"] == installed_revision,
            value["active_environment_revision"] == active_revision,
            _asset_map_matches(value["installed_assets"], installed_assets),
            _asset_map_matches(value["active_assets"], active_assets),
        )
        if any(item is None for item in checks):
            return None
        if not all(checks):
            return False

        if phase == "activate":
            if value["worker_ready"] is not False:
                return False
        elif value["worker_ready"] is not True:
            return False

        if phase == "rollback":
            return _baseline_passes(spec.probes, baseline)
        return behavior_is_equivalent_or_stricter(spec.probes, baseline, candidate)
    except (KeyError, TypeError, ValueError):
        return None


def _asset_map_matches(raw: Any, assets: Sequence[ManagedAsset | PriorAsset]) -> bool | None:
    if not isinstance(raw, Mapping):
        return None
    expected = {item.asset_id: item.sha256 for item in assets}
    if set(raw) != set(expected) or any(not isinstance(key, str) for key in raw):
        return None
    if any(value is not None and not isinstance(value, str) for value in raw.values()):
        return None
    return dict(raw) == expected


def _probe_results(raw: Any) -> tuple[ProbeResult, ...] | None:
    if not isinstance(raw, list) or any(not isinstance(item, Mapping) for item in raw):
        return None
    try:
        results = tuple(
            ProbeResult(
                probe_id=value["probe_id"],
                passed=value["passed"],
                safety_claims=tuple(value["safety_claims"]),
                evidence_sha256=value["evidence_sha256"],
            )
            for item in raw
            for value in (
                _strict(
                    item,
                    frozenset(
                        {"probe_id", "passed", "safety_claims", "evidence_sha256"}
                    ),
                    "probe evidence",
                ),
            )
        )
        identities = tuple(item.probe_id for item in results)
        if identities != tuple(sorted(set(identities))):
            return None
        return results
    except (KeyError, TypeError, ValueError):
        return None


def _baseline_passes(
    probes: Sequence[BehavioralProbe], baseline: Sequence[ProbeResult]
) -> bool:
    required = {item.probe_id: set(item.safety_claims) for item in probes}
    observed = {item.probe_id: item for item in baseline}
    return set(observed) == set(required) and all(
        observed[name].passed
        and required[name] <= set(observed[name].safety_claims)
        for name in required
    )


def project_recovery_inventory(
    spec: AgentEnvironmentSpec,
    observations: Mapping[str, RecoveryObservation],
) -> tuple[RecoveryObservation, ...]:
    """Return owner-attributed startup evidence; absence is explicit unavailable state."""
    projected = []
    for source in spec.recovery_sources:
        observation = observations.get(source.kind)
        if observation is None:
            projected.append(
                RecoveryObservation(source.kind, source.owner, "unavailable", (), "0" * 64)
            )
        elif observation.owner != source.owner:
            raise ValueError("recovery observation owner does not match the frozen source")
        else:
            projected.append(observation)
    return tuple(projected)
