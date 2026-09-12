"""Frozen release and rollback identities for foundation self-adoption."""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

from .validation import validate_git_commit

_HEX = frozenset("0123456789abcdef")


def _digest(value: str, field: str) -> str:
    if len(value) != 64 or any(char not in _HEX for char in value):
        raise ValueError(f"{field} must be a lowercase SHA-256 digest")
    return value


def _absolute(value: str, field: str) -> Path:
    path = Path(value)
    if not path.is_absolute() or "\x00" in value:
        raise ValueError(f"{field} must be an absolute path")
    return path


@dataclass(frozen=True)
class FileIdentity:
    path: str
    sha256: str
    size_bytes: int

    def __post_init__(self) -> None:
        _absolute(self.path, "file path")
        _digest(self.sha256, "file sha256")
        if self.size_bytes < 0:
            raise ValueError("file size_bytes must be nonnegative")


@dataclass(frozen=True)
class RollbackUnit:
    executor: FileIdentity
    artifact: FileIdentity | None
    configuration: FileIdentity | None
    runbook: FileIdentity


@dataclass(frozen=True)
class AdoptionSpec:
    schema_version: int
    mode: str
    source_repository: str
    source_revision: str
    artifact: FileIdentity
    install_path: str
    configuration: FileIdentity | None
    health_command: tuple[str, ...]
    state_schema: str
    rollback: RollbackUnit

    def __post_init__(self) -> None:
        if self.schema_version != 1:
            raise ValueError("adoption schema_version must be 1")
        if self.mode not in {"adopt-running", "bootstrap-absent", "upgrade"}:
            raise ValueError("unsupported adoption mode")
        if not self.source_repository or "\x00" in self.source_repository:
            raise ValueError("source repository identity is required")
        validate_git_commit(self.source_revision)
        _absolute(self.install_path, "install path")
        if not self.health_command or any(
            not value or "\x00" in value for value in self.health_command
        ):
            raise ValueError("health_command must be a nonempty argument vector")
        _absolute(self.health_command[0], "health executable")
        if not self.state_schema or "\x00" in self.state_schema:
            raise ValueError("state_schema is required")
        if (
            self.mode != "bootstrap-absent"
            and self.rollback.artifact is None
            and self.rollback.configuration is None
        ):
            raise ValueError("adoption rollback must preserve an artifact or configuration")

        managed = [self.install_path]
        if self.configuration is not None:
            managed.append(self.configuration.path)
        protected = [self.rollback.executor.path, self.rollback.runbook.path]
        if self.rollback.artifact is not None:
            protected.append(self.rollback.artifact.path)
        if self.rollback.configuration is not None:
            protected.append(self.rollback.configuration.path)
        for path in protected:
            if any(_overlaps(path, target) for target in managed):
                raise ValueError("rollback unit must remain outside managed release paths")


def adoption_to_dict(spec: AdoptionSpec) -> dict[str, Any]:
    value = asdict(spec)
    value["health_command"] = list(spec.health_command)
    return value


def adoption_from_dict(raw: Mapping[str, Any]) -> AdoptionSpec:
    value = dict(raw)
    value["artifact"] = FileIdentity(**value["artifact"])
    if value.get("configuration") is not None:
        value["configuration"] = FileIdentity(**value["configuration"])
    rollback = dict(value["rollback"])
    rollback["executor"] = FileIdentity(**rollback["executor"])
    rollback["runbook"] = FileIdentity(**rollback["runbook"])
    if rollback.get("artifact") is not None:
        rollback["artifact"] = FileIdentity(**rollback["artifact"])
    if rollback.get("configuration") is not None:
        rollback["configuration"] = FileIdentity(**rollback["configuration"])
    value["rollback"] = RollbackUnit(**rollback)
    value["health_command"] = tuple(value["health_command"])
    return AdoptionSpec(**value)


def _overlaps(left: str, right: str) -> bool:
    left_path, right_path = Path(left), Path(right)
    return (
        left_path == right_path
        or left_path in right_path.parents
        or right_path in left_path.parents
    )
