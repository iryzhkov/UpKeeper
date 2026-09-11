"""Deterministic, non-executing rollout plans for UpKeeper candidates."""

from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass
from enum import StrEnum

from .validation import validate_host_name


class RolloutStage(StrEnum):
    INSTALL_ONLY = "install-only"
    CANARY = "canary"
    SOAK = "soak"
    BATCH = "bounded-batch"
    FLEET = "fleet"


@dataclass(frozen=True)
class RolloutStep:
    stage: RolloutStage
    hosts: tuple[str, ...]
    batch: int | None = None


@dataclass(frozen=True)
class RolloutPlan:
    schema_version: int
    hosts: tuple[str, ...]
    canary_hosts: tuple[str, ...]
    batch_size: int
    soak_seconds: int

    def __post_init__(self) -> None:
        hosts = tuple(sorted(validate_host_name(host) for host in self.hosts))
        canaries = tuple(sorted(validate_host_name(host) for host in self.canary_hosts))
        if self.schema_version != 1:
            raise ValueError("rollout schema_version must be 1")
        if not hosts or hosts != self.hosts or len(set(hosts)) != len(hosts):
            raise ValueError("rollout hosts must be nonempty, unique, and sorted")
        if not canaries or canaries != self.canary_hosts or len(set(canaries)) != len(canaries):
            raise ValueError("canary_hosts must be nonempty, unique, and sorted")
        if not set(canaries).issubset(hosts):
            raise ValueError("canary_hosts must be a subset of hosts")
        if self.batch_size < 1 or self.batch_size > 100:
            raise ValueError("batch_size must be between 1 and 100")
        if self.soak_seconds < 1:
            raise ValueError("soak_seconds must be positive")

    @property
    def plan_hash(self) -> str:
        payload = json.dumps(asdict(self), sort_keys=True, separators=(",", ":")).encode()
        return hashlib.sha256(payload).hexdigest()

    def steps(self) -> tuple[RolloutStep, ...]:
        remaining = [host for host in self.hosts if host not in self.canary_hosts]
        steps = [
            RolloutStep(RolloutStage.INSTALL_ONLY, self.hosts),
            RolloutStep(RolloutStage.CANARY, self.canary_hosts),
            RolloutStep(RolloutStage.SOAK, self.canary_hosts),
        ]
        for index, start in enumerate(range(0, len(remaining), self.batch_size), start=1):
            steps.append(
                RolloutStep(
                    RolloutStage.BATCH,
                    tuple(remaining[start : start + self.batch_size]),
                    index,
                )
            )
        steps.append(RolloutStep(RolloutStage.FLEET, self.hosts))
        return tuple(steps)
