"""Revision-fenced UpKeeper mutation lifecycle.

Read-only doctor/status does not import this module. Mutating callers supply an
explicit state root, exact plan hash, expected event revision, lock store, and backend.
"""

from __future__ import annotations

import hashlib
import json
import os
from collections.abc import Mapping
from dataclasses import asdict, dataclass
from datetime import UTC, datetime
from enum import StrEnum
from pathlib import Path
from typing import Any, Protocol

from .adoption import AdoptionSpec, adoption_from_dict, adoption_to_dict
from .locks import LocalLockStore, LockContentionError, LockMetadata
from .redaction import redact
from .validation import (
    validate_component_name,
    validate_git_commit,
    validate_host_name,
    validate_run_id,
)


class UpKeeperError(RuntimeError):
    """A deployment transition was refused or failed."""


class RevisionConflict(UpKeeperError):
    """The caller did not inspect the current immutable run revision."""


class RecoveryRequired(UpKeeperError):
    """An ambiguous outcome must be observed before retry."""


class BackendFailure(UpKeeperError):
    """The backend returned a known unsuccessful result."""


class AmbiguousOutcome(UpKeeperError):
    """The backend may have performed an effect before losing its response."""


class Phase(StrEnum):
    INSTALL = "install"
    ACTIVATE = "activate"
    RUN = "run"
    OBSERVE = "observe"
    ROLLBACK = "rollback"


class EffectState(StrEnum):
    PREPARED = "PREPARED"
    STARTED = "STARTED"
    CONFIRMED = "CONFIRMED"
    UNKNOWN = "UNKNOWN"
    RECONCILED = "RECONCILED"
    COMPENSATED = "COMPENSATED"


@dataclass(frozen=True)
class BackendSpec:
    kind: str
    repository: str
    repository_revision: str
    inventory: str
    playbook: str

    def __post_init__(self) -> None:
        if self.kind != "homelab-ansible-v1":
            raise ValueError("backend kind must be homelab-ansible-v1")
        if not self.repository or "\x00" in self.repository:
            raise ValueError("backend repository identity is required")
        validate_git_commit(self.repository_revision)
        _validate_relative_file(self.inventory)
        _validate_relative_file(self.playbook)


@dataclass(frozen=True)
class DeploymentSpec:
    schema_version: int
    deployment_id: str
    run_id: str
    controller: str
    component: str
    hosts: tuple[str, ...]
    desired_revision: str
    previous_revision: str | None
    authority_ref: str
    policy_evaluation_ref: str
    backend: BackendSpec
    adoption: AdoptionSpec | None = None

    def __post_init__(self) -> None:
        if self.schema_version != 1:
            raise ValueError("deployment schema_version must be 1")
        suffix = self.deployment_id.removeprefix("deployment-")
        if (
            not self.deployment_id.startswith("deployment-")
            or len(suffix) != 32
            or any(char not in "0123456789abcdef" for char in suffix)
        ):
            raise ValueError("deployment_id must be deployment- plus 32 lowercase hex characters")
        validate_run_id(self.run_id)
        validate_host_name(self.controller)
        validate_component_name(self.component)
        checked = tuple(sorted(validate_host_name(host) for host in self.hosts))
        if not checked or checked != self.hosts or len(set(checked)) != len(checked):
            raise ValueError("hosts must be nonempty, unique, and sorted")
        validate_git_commit(self.desired_revision)
        if self.previous_revision is not None:
            validate_git_commit(self.previous_revision)
        if self.adoption is None:
            if self.previous_revision is None:
                raise ValueError("previous_revision is required without an adoption contract")
            if self.desired_revision == self.previous_revision:
                raise ValueError("desired_revision must differ from previous_revision")
        else:
            if self.adoption.source_revision != self.desired_revision:
                raise ValueError("adoption source_revision must equal desired_revision")
            if self.previous_revision is None and self.adoption.mode != "bootstrap-absent":
                raise ValueError("missing previous_revision requires bootstrap-absent adoption")
            if self.previous_revision is not None and self.adoption.mode == "bootstrap-absent":
                raise ValueError("bootstrap-absent adoption requires no previous_revision")
            if (
                self.previous_revision == self.desired_revision
                and self.adoption.mode != "adopt-running"
            ):
                raise ValueError("equal revisions require adopt-running adoption")
        if not self.authority_ref or not self.policy_evaluation_ref:
            raise ValueError("authority and policy evaluation references are required")


@dataclass(frozen=True)
class Observation:
    installed_revision: str | None
    activated_revision: str | None
    running_revision: str | None
    healthy: bool | None
    evidence: Mapping[str, Any]
    installed_artifact_sha256: str | None = None
    activated_artifact_sha256: str | None = None
    configuration_sha256: str | None = None


@dataclass(frozen=True)
class BackendResult:
    observation: Observation
    receipt: Mapping[str, Any]


@dataclass(frozen=True)
class Reconciliation:
    applied: bool | None
    observation: Observation
    evidence: Mapping[str, Any]


class DeploymentBackend(Protocol):
    def check(self, spec: DeploymentSpec) -> Observation: ...

    def execute(self, spec: DeploymentSpec, phase: Phase, effect_id: str) -> BackendResult: ...

    def reconcile(self, spec: DeploymentSpec, phase: Phase, effect_id: str) -> Reconciliation: ...


@dataclass(frozen=True)
class RunEvent:
    schema_version: int
    revision: int
    kind: str
    created_at: str
    previous_event_sha256: str | None
    data: Mapping[str, Any]
    event_sha256: str


@dataclass(frozen=True)
class RunSnapshot:
    spec: DeploymentSpec
    plan_hash: str
    events: tuple[RunEvent, ...]

    @property
    def revision(self) -> int:
        return self.events[-1].revision

    @property
    def terminal(self) -> bool:
        return self.events[-1].kind in {"COMPLETED", "ROLLED_BACK"}

    def effects(self) -> dict[str, RunEvent]:
        latest: dict[str, RunEvent] = {}
        for event in self.events:
            if event.kind != "EFFECT":
                continue
            effect_id = event.data.get("effect_id")
            if isinstance(effect_id, str):
                latest[effect_id] = event
        return latest


class RunStore:
    """Immutable intent plus an fsynced, hash-chained event journal."""

    def __init__(self, root: Path) -> None:
        self.root = root.expanduser().resolve()

    def create(self, spec: DeploymentSpec) -> RunSnapshot:
        run_root = self._run_root(spec.run_id)
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        os.chmod(self.root, 0o700)
        try:
            run_root.mkdir(mode=0o700)
        except FileExistsError:
            existing = self.load(spec.run_id)
            if existing.spec == spec:
                return existing
            raise RevisionConflict("run ID already belongs to a different plan") from None
        try:
            intent = _canonical_json(_spec_dict(spec))
            _write_exclusive(run_root / "intent.json", intent)
            event = _new_event(
                0,
                "PLAN_FROZEN",
                None,
                {"plan_hash": _sha256(intent), "deployment_id": spec.deployment_id},
            )
            _write_exclusive(run_root / "000000.json", _event_bytes(event))
            _fsync_directory(run_root)
            _fsync_directory(self.root)
        except BaseException as exc:
            raise RecoveryRequired(f"run initialization requires inspection: {run_root}") from exc
        return self.load(spec.run_id)

    def load(self, run_id: str) -> RunSnapshot:
        run_root = self._run_root(run_id)
        try:
            raw_spec = json.loads((run_root / "intent.json").read_text(encoding="utf-8"))
            spec = _spec_from_dict(raw_spec)
        except (OSError, TypeError, ValueError, json.JSONDecodeError) as exc:
            raise RecoveryRequired(
                f"invalid run intent for {run_id}: {type(exc).__name__}"
            ) from exc
        plan_hash = _sha256(_canonical_json(_spec_dict(spec)))
        paths = sorted(run_root.glob("[0-9][0-9][0-9][0-9][0-9][0-9].json"))
        if not paths:
            raise RecoveryRequired(f"run {run_id} has no event journal")
        events: list[RunEvent] = []
        previous_hash: str | None = None
        for expected, path in enumerate(paths):
            try:
                payload = path.read_bytes()
                event = RunEvent(**json.loads(payload))
            except (OSError, TypeError, ValueError, json.JSONDecodeError) as exc:
                raise RecoveryRequired(f"invalid event {path.name}: {type(exc).__name__}") from exc
            if (
                path.name != f"{expected:06d}.json"
                or event.schema_version != 1
                or event.revision != expected
                or event.previous_event_sha256 != previous_hash
                or event.event_sha256 != _event_digest(event)
            ):
                raise RecoveryRequired(f"run {run_id} event chain failed at {path.name}")
            events.append(event)
            previous_hash = _sha256(payload)
        if events[0].kind != "PLAN_FROZEN" or events[0].data.get("plan_hash") != plan_hash:
            raise RecoveryRequired(f"run {run_id} plan hash does not match frozen intent")
        return RunSnapshot(spec, plan_hash, tuple(events))

    def append(
        self,
        run_id: str,
        *,
        expected_revision: int,
        kind: str,
        data: Mapping[str, Any],
    ) -> RunSnapshot:
        run_root = self._run_root(run_id)
        transition_lock = run_root / ".transition.lock"
        try:
            descriptor = os.open(transition_lock, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            os.close(descriptor)
        except FileExistsError as exc:
            raise RecoveryRequired(f"run {run_id} has an unresolved transition lock") from exc
        try:
            snapshot = self.load(run_id)
            if snapshot.revision != expected_revision:
                raise RevisionConflict(
                    f"expected run revision {expected_revision}, current is {snapshot.revision}"
                )
            previous = run_root / f"{snapshot.revision:06d}.json"
            event = _new_event(
                expected_revision + 1,
                kind,
                _sha256(previous.read_bytes()),
                redact(dict(data)),
            )
            _write_exclusive(run_root / f"{event.revision:06d}.json", _event_bytes(event))
            _fsync_directory(run_root)
            return self.load(run_id)
        finally:
            try:
                transition_lock.unlink()
                _fsync_directory(run_root)
            except FileNotFoundError:
                pass

    def _run_root(self, run_id: str) -> Path:
        return self.root / validate_run_id(run_id)


class UpKeeper:
    """Coordinator for one revision-fenced deployment saga."""

    def __init__(self, store: RunStore, locks: LocalLockStore, backend: DeploymentBackend) -> None:
        self.store = store
        self.locks = locks
        self.backend = backend

    def plan(self, spec: DeploymentSpec) -> RunSnapshot:
        return self.store.create(spec)

    def check(self, run_id: str, *, expected_revision: int) -> RunSnapshot:
        snapshot = self._expect(run_id, expected_revision)
        observation = self.backend.check(snapshot.spec)
        if not _matches_previous(observation, snapshot.spec):
            raise BackendFailure("preflight does not match the previous healthy revision")
        return self.store.append(
            run_id,
            expected_revision=expected_revision,
            kind="CHECKED",
            data={"observation": _observation_dict(observation)},
        )

    def apply(self, run_id: str, *, expected_revision: int, approved_plan_hash: str) -> RunSnapshot:
        snapshot = self._expect(run_id, expected_revision)
        if snapshot.plan_hash != approved_plan_hash:
            raise RevisionConflict("approval does not match the frozen plan hash")
        if snapshot.terminal:
            return snapshot
        if not any(event.kind == "CHECKED" for event in snapshot.events):
            raise RevisionConflict("apply requires a revision-fenced successful check")
        self._ensure_locks(snapshot.spec)
        try:
            for phase in (Phase.INSTALL, Phase.ACTIVATE, Phase.RUN, Phase.OBSERVE):
                snapshot = self._execute_or_resume(snapshot, phase)
                if phase is Phase.OBSERVE:
                    observation = _latest_observation(snapshot, phase)
                    if observation is None or not _matches_desired(observation, snapshot.spec):
                        return self._rollback(snapshot, "health gate failed")
            snapshot = self.store.append(
                run_id,
                expected_revision=snapshot.revision,
                kind="COMPLETED",
                data={"desired_revision": snapshot.spec.desired_revision},
            )
            self._release_locks(snapshot.spec)
            return snapshot
        except RecoveryRequired:
            raise
        except BaseException:
            current = self.store.load(run_id)
            if _confirmed_mutation(current):
                return self._rollback(current, "known backend failure")
            self._release_locks(current.spec)
            raise

    def reconcile(self, run_id: str, *, expected_revision: int) -> RunSnapshot:
        snapshot = self._expect(run_id, expected_revision)
        effect = _latest_ambiguous_effect(snapshot)
        if effect is None:
            raise RevisionConflict("run has no ambiguous effect to reconcile")
        phase = Phase(str(effect.data["phase"]))
        result = self.backend.reconcile(snapshot.spec, phase, str(effect.data["effect_id"]))
        if result.applied is None:
            raise RecoveryRequired("backend cannot determine whether effect applied")
        return self._effect_event(
            snapshot,
            str(effect.data["effect_id"]),
            phase,
            str(effect.data["input_hash"]),
            EffectState.RECONCILED,
            {
                "applied": result.applied,
                "observation": _observation_dict(result.observation),
                "evidence": dict(result.evidence),
            },
        )

    def _execute_or_resume(self, snapshot: RunSnapshot, phase: Phase) -> RunSnapshot:
        effect_id = _effect_id(snapshot.spec, phase)
        latest = snapshot.effects().get(effect_id)
        if latest is not None:
            state = EffectState(str(latest.data["state"]))
            if state in {EffectState.CONFIRMED, EffectState.COMPENSATED}:
                return snapshot
            if state is EffectState.RECONCILED and latest.data.get("applied") is True:
                return snapshot
            if state in {EffectState.STARTED, EffectState.UNKNOWN}:
                raise RecoveryRequired(f"{effect_id} requires observation before retry")
        input_hash = _phase_input_hash(snapshot.spec, phase)
        if latest is None:
            snapshot = self._effect_event(
                snapshot, effect_id, phase, input_hash, EffectState.PREPARED
            )
        elif latest.data.get("input_hash") != input_hash:
            raise RevisionConflict(f"{effect_id} input changed after preparation")
        snapshot = self._effect_event(snapshot, effect_id, phase, input_hash, EffectState.STARTED)
        try:
            result = self.backend.execute(snapshot.spec, phase, effect_id)
        except AmbiguousOutcome as exc:
            self._effect_event(
                snapshot,
                effect_id,
                phase,
                input_hash,
                EffectState.UNKNOWN,
                {"reason": str(exc)},
            )
            raise RecoveryRequired(f"{effect_id} outcome is unknown") from exc
        except BackendFailure as exc:
            self.store.append(
                snapshot.spec.run_id,
                expected_revision=snapshot.revision,
                kind="PHASE_FAILED",
                data={"effect_id": effect_id, "phase": phase.value, "reason": str(exc)},
            )
            raise
        return self._effect_event(
            snapshot,
            effect_id,
            phase,
            input_hash,
            EffectState.CONFIRMED,
            {
                "observation": _observation_dict(result.observation),
                "receipt": dict(result.receipt),
            },
        )

    def _rollback(self, snapshot: RunSnapshot, reason: str) -> RunSnapshot:
        phase = Phase.ROLLBACK
        effect_id = _effect_id(snapshot.spec, phase)
        input_hash = _phase_input_hash(snapshot.spec, phase)
        latest = snapshot.effects().get(effect_id)
        if latest is not None:
            state = EffectState(str(latest.data["state"]))
            if state is EffectState.COMPENSATED:
                return snapshot
            if state in {EffectState.STARTED, EffectState.UNKNOWN}:
                raise RecoveryRequired(f"rollback {effect_id} requires reconciliation")
            if state is EffectState.RECONCILED and latest.data.get("applied") is True:
                observation = _observation_from_event(latest)
                if observation is None or not _matches_previous(observation, snapshot.spec):
                    raise RecoveryRequired(
                        f"rollback {effect_id} reconciliation lacks healthy rollback proof"
                    )
                snapshot = self._effect_event(
                    snapshot,
                    effect_id,
                    phase,
                    input_hash,
                    EffectState.COMPENSATED,
                    {
                        "observation": _observation_dict(observation),
                        "reconciled": True,
                    },
                )
                snapshot = self.store.append(
                    snapshot.spec.run_id,
                    expected_revision=snapshot.revision,
                    kind="ROLLED_BACK",
                    data={
                        "reason": reason,
                        "restored_revision": snapshot.spec.previous_revision,
                    },
                )
                self._release_locks(snapshot.spec)
                return snapshot
        snapshot = self._effect_event(
            snapshot, effect_id, phase, input_hash, EffectState.PREPARED, {"reason": reason}
        )
        snapshot = self._effect_event(snapshot, effect_id, phase, input_hash, EffectState.STARTED)
        try:
            result = self.backend.execute(snapshot.spec, phase, effect_id)
        except (AmbiguousOutcome, BackendFailure) as exc:
            self._effect_event(
                snapshot,
                effect_id,
                phase,
                input_hash,
                EffectState.UNKNOWN,
                {"reason": str(exc)},
            )
            raise RecoveryRequired(f"rollback {effect_id} requires reconciliation") from exc
        if not _matches_previous(result.observation, snapshot.spec):
            self._effect_event(
                snapshot,
                effect_id,
                phase,
                input_hash,
                EffectState.UNKNOWN,
                {"reason": "rollback does not prove previous healthy revision"},
            )
            raise RecoveryRequired(f"rollback {effect_id} is not independently confirmed")
        snapshot = self._effect_event(
            snapshot,
            effect_id,
            phase,
            input_hash,
            EffectState.COMPENSATED,
            {
                "observation": _observation_dict(result.observation),
                "receipt": dict(result.receipt),
            },
        )
        snapshot = self.store.append(
            snapshot.spec.run_id,
            expected_revision=snapshot.revision,
            kind="ROLLED_BACK",
            data={"reason": reason, "restored_revision": snapshot.spec.previous_revision},
        )
        self._release_locks(snapshot.spec)
        return snapshot

    def _effect_event(
        self,
        snapshot: RunSnapshot,
        effect_id: str,
        phase: Phase,
        input_hash: str,
        state: EffectState,
        extra: Mapping[str, Any] | None = None,
    ) -> RunSnapshot:
        data: dict[str, Any] = {
            "effect_id": effect_id,
            "phase": phase.value,
            "input_hash": input_hash,
            "state": state.value,
        }
        if extra:
            data.update(extra)
        return self.store.append(
            snapshot.spec.run_id,
            expected_revision=snapshot.revision,
            kind="EFFECT",
            data=data,
        )

    def _expect(self, run_id: str, expected_revision: int) -> RunSnapshot:
        snapshot = self.store.load(run_id)
        if snapshot.revision != expected_revision:
            raise RevisionConflict(
                f"expected run revision {expected_revision}, current is {snapshot.revision}"
            )
        return snapshot

    def _ensure_locks(self, spec: DeploymentSpec) -> None:
        metadata = LockMetadata.new(
            run_id=spec.run_id,
            controller=spec.controller,
            controller_pid=os.getpid(),
            desired_manifest_commit=spec.backend.repository_revision,
            components=[spec.component],
        )
        acquired: list[str] = []
        try:
            for host in spec.hosts:
                inspection = self.locks.inspect(host)
                if (
                    inspection.state == "held"
                    and inspection.metadata is not None
                    and inspection.metadata.run_id == spec.run_id
                ):
                    continue
                self.locks.acquire(host, metadata)
                acquired.append(host)
        except BaseException:
            for host in reversed(acquired):
                self.locks.release(host, spec.run_id)
            raise

    def _release_locks(self, spec: DeploymentSpec) -> None:
        for host in reversed(spec.hosts):
            inspection = self.locks.inspect(host)
            if (
                inspection.state == "held"
                and inspection.metadata is not None
                and inspection.metadata.run_id == spec.run_id
            ):
                self.locks.release(host, spec.run_id)
            elif inspection.state != "absent":
                raise LockContentionError(f"{host} lock ownership became ambiguous")


def _validate_relative_file(value: str) -> None:
    path = Path(value)
    if (
        not value
        or path.is_absolute()
        or ".." in path.parts
        or "." in path.parts
        or path.suffix not in {".yml", ".yaml"}
        or str(path) != value
    ):
        raise ValueError(f"unsafe backend path: {value!r}")


def _spec_dict(spec: DeploymentSpec) -> dict[str, Any]:
    value = asdict(spec)
    value["hosts"] = list(spec.hosts)
    if spec.adoption is None:
        value.pop("adoption")
    else:
        value["adoption"] = adoption_to_dict(spec.adoption)
    return value


def _spec_from_dict(raw: Mapping[str, Any]) -> DeploymentSpec:
    value = dict(raw)
    value["hosts"] = tuple(value["hosts"])
    value["backend"] = BackendSpec(**value["backend"])
    if value.get("adoption") is not None:
        value["adoption"] = adoption_from_dict(value["adoption"])
    return DeploymentSpec(**value)


def _canonical_json(value: Mapping[str, Any]) -> bytes:
    return (json.dumps(redact(dict(value)), sort_keys=True, separators=(",", ":")) + "\n").encode()


def _event_digest(event: RunEvent) -> str:
    value = asdict(event)
    value.pop("event_sha256")
    return _sha256(_canonical_json(value))


def _new_event(
    revision: int,
    kind: str,
    previous_event_sha256: str | None,
    data: Mapping[str, Any],
) -> RunEvent:
    event = RunEvent(1, revision, kind, _now(), previous_event_sha256, data, "")
    return RunEvent(
        event.schema_version,
        event.revision,
        event.kind,
        event.created_at,
        event.previous_event_sha256,
        event.data,
        _event_digest(event),
    )


def _event_bytes(event: RunEvent) -> bytes:
    return _canonical_json(asdict(event))


def _write_exclusive(path: Path, payload: bytes) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(payload)
        stream.flush()
        os.fsync(stream.fileno())


def _fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _sha256(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def _now() -> str:
    return datetime.now(UTC).isoformat().replace("+00:00", "Z")


def _effect_id(spec: DeploymentSpec, phase: Phase) -> str:
    return "effect-" + _sha256(f"{spec.deployment_id}\x00{spec.run_id}\x00{phase.value}".encode())


def _phase_input_hash(spec: DeploymentSpec, phase: Phase) -> str:
    return _sha256(_canonical_json({"plan": _spec_dict(spec), "phase": phase.value}))


def _observation_dict(observation: Observation) -> dict[str, Any]:
    return {
        "installed_revision": observation.installed_revision,
        "activated_revision": observation.activated_revision,
        "running_revision": observation.running_revision,
        "healthy": observation.healthy,
        "evidence": dict(observation.evidence),
        "installed_artifact_sha256": observation.installed_artifact_sha256,
        "activated_artifact_sha256": observation.activated_artifact_sha256,
        "configuration_sha256": observation.configuration_sha256,
    }


def _observation_from_event(event: RunEvent) -> Observation | None:
    raw = event.data.get("observation")
    if not isinstance(raw, Mapping):
        return None
    return Observation(
        raw.get("installed_revision"),
        raw.get("activated_revision"),
        raw.get("running_revision"),
        raw.get("healthy"),
        raw.get("evidence", {}),
        raw.get("installed_artifact_sha256"),
        raw.get("activated_artifact_sha256"),
        raw.get("configuration_sha256"),
    )


def _latest_observation(snapshot: RunSnapshot, phase: Phase) -> Observation | None:
    event = snapshot.effects().get(_effect_id(snapshot.spec, phase))
    return _observation_from_event(event) if event is not None else None


def _matches_previous(observation: Observation, spec: DeploymentSpec) -> bool:
    if spec.previous_revision is None:
        return (
            spec.adoption is not None
            and spec.adoption.mode == "bootstrap-absent"
            and observation.installed_revision is None
            and observation.activated_revision is None
            and observation.running_revision is None
            and observation.healthy is not True
        )
    if not (
        observation.installed_revision == spec.previous_revision
        and observation.activated_revision == spec.previous_revision
        and observation.running_revision == spec.previous_revision
        and observation.healthy is True
    ):
        return False
    if spec.adoption is None:
        return True
    if spec.adoption.mode == "adopt-running":
        artifact = spec.adoption.artifact
        configuration = spec.adoption.configuration
    else:
        artifact = spec.adoption.rollback.artifact
        configuration = spec.adoption.rollback.configuration
        if artifact is None:
            return False
    return (
        observation.installed_artifact_sha256 == artifact.sha256
        and observation.activated_artifact_sha256 == artifact.sha256
        and (configuration is None or observation.configuration_sha256 == configuration.sha256)
    )


def _matches_desired(observation: Observation, spec: DeploymentSpec) -> bool:
    if not (
        observation.installed_revision == spec.desired_revision
        and observation.activated_revision == spec.desired_revision
        and observation.running_revision == spec.desired_revision
        and observation.healthy is True
    ):
        return False
    if spec.adoption is None:
        return True
    return (
        observation.installed_artifact_sha256 == spec.adoption.artifact.sha256
        and observation.activated_artifact_sha256 == spec.adoption.artifact.sha256
        and (
            spec.adoption.configuration is None
            or observation.configuration_sha256 == spec.adoption.configuration.sha256
        )
    )


def _confirmed_mutation(snapshot: RunSnapshot) -> bool:
    return any(
        event.data.get("phase") != Phase.ROLLBACK.value
        and (
            event.data.get("state") == EffectState.CONFIRMED.value
            or (
                event.data.get("state") == EffectState.RECONCILED.value
                and event.data.get("applied") is True
            )
        )
        for event in snapshot.effects().values()
    )


def _latest_ambiguous_effect(snapshot: RunSnapshot) -> RunEvent | None:
    candidates = [
        event
        for event in snapshot.effects().values()
        if event.data.get("state") in {EffectState.STARTED.value, EffectState.UNKNOWN.value}
    ]
    return max(candidates, key=lambda event: event.revision) if candidates else None
