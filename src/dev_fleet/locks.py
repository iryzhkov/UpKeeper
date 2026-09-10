"""Atomic deployment-lock primitives.

No Phase 1 command acquires a lock. These primitives are gated for future mutating commands.
"""

from __future__ import annotations

import json
import os
from collections.abc import Callable
from dataclasses import asdict, dataclass
from datetime import UTC, datetime
from pathlib import Path

from .redaction import redact
from .validation import validate_component_name, validate_host_name, validate_run_id


class LockContentionError(RuntimeError):
    """A target already has a deployment lock."""


@dataclass(frozen=True)
class LockMetadata:
    schema_version: int
    run_id: str
    controller: str
    controller_pid: int
    desired_manifest_commit: str
    components: list[str]
    started_at: str

    @classmethod
    def new(
        cls,
        *,
        run_id: str,
        controller: str,
        controller_pid: int,
        desired_manifest_commit: str,
        components: list[str],
        now: datetime | None = None,
    ) -> LockMetadata:
        if controller_pid <= 0:
            raise ValueError("controller_pid must be positive")
        for component in components:
            validate_component_name(component)
        return cls(
            schema_version=1,
            run_id=validate_run_id(run_id),
            controller=validate_host_name(controller),
            controller_pid=controller_pid,
            desired_manifest_commit=desired_manifest_commit,
            components=sorted(set(components)),
            started_at=(now or datetime.now(UTC))
            .astimezone(UTC)
            .isoformat()
            .replace("+00:00", "Z"),
        )


@dataclass(frozen=True)
class LockInspection:
    host: str
    state: str
    metadata: LockMetadata | None
    reason: str | None = None


def ordered_hosts(hosts: list[str]) -> list[str]:
    checked = [validate_host_name(host) for host in hosts]
    if len(checked) != len(set(checked)):
        raise ValueError("host lock set contains duplicates")
    return sorted(checked)


def stale_lock_confirmed(
    metadata: LockMetadata,
    *,
    now: datetime,
    maximum_age_seconds: int,
    controller_is_alive: Callable[[str, int], bool | None],
) -> bool:
    if maximum_age_seconds <= 0:
        raise ValueError("maximum_age_seconds must be positive")
    started = datetime.fromisoformat(metadata.started_at.replace("Z", "+00:00"))
    if (now.astimezone(UTC) - started.astimezone(UTC)).total_seconds() <= maximum_age_seconds:
        return False
    # None means the controller could not be checked. Uncertainty is never stale.
    return controller_is_alive(metadata.controller, metadata.controller_pid) is False


class LocalLockStore:
    """Filesystem implementation used by tests and future target-side helpers."""

    def __init__(self, root: Path) -> None:
        self.root = root.expanduser().resolve()

    def _directory(self, host: str) -> Path:
        return self.root / f"{validate_host_name(host)}.lock"

    def inspect(self, host: str) -> LockInspection:
        directory = self._directory(host)
        if not directory.exists():
            return LockInspection(host=host, state="absent", metadata=None)
        if not directory.is_dir():
            return LockInspection(host=host, state="invalid", metadata=None, reason="not-directory")
        try:
            raw = json.loads((directory / "owner.json").read_text(encoding="utf-8"))
            metadata = LockMetadata(**raw)
            validate_run_id(metadata.run_id)
        except (OSError, TypeError, ValueError, json.JSONDecodeError) as exc:
            return LockInspection(
                host=host, state="invalid", metadata=None, reason=type(exc).__name__
            )
        return LockInspection(host=host, state="held", metadata=metadata)

    def acquire(self, host: str, metadata: LockMetadata) -> Path:
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        os.chmod(self.root, 0o700)
        directory = self._directory(host)
        try:
            directory.mkdir(mode=0o700)
        except FileExistsError as exc:
            raise LockContentionError(f"{host} is already locked") from exc
        owner = directory / "owner.json"
        try:
            descriptor = os.open(owner, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                json.dump(redact(asdict(metadata)), stream, sort_keys=True)
                stream.write("\n")
                stream.flush()
                os.fsync(stream.fileno())
        except BaseException:
            try:
                owner.unlink()
            except FileNotFoundError:
                pass
            directory.rmdir()
            raise
        return directory

    def release(self, host: str, run_id: str) -> None:
        expected = validate_run_id(run_id)
        inspection = self.inspect(host)
        if inspection.state != "held" or inspection.metadata is None:
            raise LockContentionError(f"{host} has no valid owned lock")
        if inspection.metadata.run_id != expected:
            raise LockContentionError(f"{host} lock belongs to another run")
        directory = self._directory(host)
        (directory / "owner.json").unlink()
        directory.rmdir()


def acquire_all(store: LocalLockStore, hosts: list[str], metadata: LockMetadata) -> list[str]:
    acquired: list[str] = []
    try:
        for host in ordered_hosts(hosts):
            store.acquire(host, metadata)
            acquired.append(host)
    except BaseException:
        for host in reversed(acquired):
            store.release(host, metadata.run_id)
        raise
    return acquired
