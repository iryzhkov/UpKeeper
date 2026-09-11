"""Bounded homelab-infra/Ansible backend for UpKeeper."""

from __future__ import annotations

import hashlib
import os
import subprocess
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from pathlib import Path

from .upkeeper import (
    AmbiguousOutcome,
    BackendFailure,
    BackendResult,
    DeploymentSpec,
    Observation,
    Phase,
    Reconciliation,
)

PHASE_TAGS = {
    Phase.INSTALL: "upkeeper_install",
    Phase.ACTIVATE: "upkeeper_activate",
    Phase.RUN: "upkeeper_run",
    Phase.OBSERVE: "upkeeper_observe",
    Phase.ROLLBACK: "upkeeper_rollback",
}


@dataclass(frozen=True)
class CommandResult:
    returncode: int
    stdout: bytes
    stderr: bytes


CommandRunner = Callable[[Sequence[str], Path, int], CommandResult]
Observer = Callable[[DeploymentSpec], Observation]


def subprocess_runner(argv: Sequence[str], cwd: Path, timeout: int) -> CommandResult:
    """Run an already validated argument vector without a shell."""
    try:
        result = subprocess.run(
            list(argv),
            cwd=cwd,
            check=False,
            capture_output=True,
            timeout=timeout,
        )
    except subprocess.TimeoutExpired as exc:
        raise AmbiguousOutcome("Ansible timed out; target state must be observed") from exc
    return CommandResult(result.returncode, result.stdout, result.stderr)


class AnsibleBackend:
    """Apply one fixed playbook at one exact clean homelab-infra revision."""

    def __init__(
        self,
        repository_root: Path,
        *,
        observer: Observer,
        runner: CommandRunner = subprocess_runner,
        timeout_seconds: int = 300,
    ) -> None:
        self.repository_root = repository_root.expanduser().resolve()
        self.observer = observer
        self.runner = runner
        if timeout_seconds < 1 or timeout_seconds > 3600:
            raise ValueError("timeout_seconds must be between 1 and 3600")
        self.timeout_seconds = timeout_seconds

    def check(self, spec: DeploymentSpec) -> Observation:
        self._verify_repository(spec)
        observation = self.observer(spec)
        return observation

    def execute(self, spec: DeploymentSpec, phase: Phase, effect_id: str) -> BackendResult:
        self._verify_repository(spec)
        if not effect_id.startswith("effect-") or len(effect_id) != 71:
            raise ValueError("invalid UpKeeper effect identity")
        inventory = self.repository_root / spec.backend.inventory
        playbook = self.repository_root / spec.backend.playbook
        argv = [
            "ansible-playbook",
            "-i",
            os.fspath(inventory),
            os.fspath(playbook),
            "--limit",
            ",".join(spec.hosts),
            "--tags",
            PHASE_TAGS[phase],
            "--extra-vars",
            f"upkeeper_effect_id={effect_id}",
        ]
        result = self.runner(argv, self.repository_root, self.timeout_seconds)
        receipt = {
            "backend": spec.backend.kind,
            "returncode": result.returncode,
            "stdout_sha256": hashlib.sha256(result.stdout).hexdigest(),
            "stderr_sha256": hashlib.sha256(result.stderr).hexdigest(),
        }
        if result.returncode != 0:
            raise BackendFailure(
                f"Ansible {phase.value} exited {result.returncode}; observe before compensation"
            )
        return BackendResult(self.observer(spec), receipt)

    def reconcile(self, spec: DeploymentSpec, phase: Phase, effect_id: str) -> Reconciliation:
        self._verify_repository(spec)
        observation = self.observer(spec)
        if phase is Phase.ROLLBACK:
            applied = _is_revision(observation, spec.previous_revision, require_health=True)
        elif phase is Phase.OBSERVE:
            applied = _is_revision(observation, spec.desired_revision, require_health=True)
        elif phase is Phase.INSTALL:
            applied = observation.installed_revision == spec.desired_revision
        elif phase is Phase.ACTIVATE:
            applied = observation.activated_revision == spec.desired_revision
        else:
            applied = observation.running_revision == spec.desired_revision
        return Reconciliation(
            applied=applied,
            observation=observation,
            evidence={"effect_id": effect_id, "source": "independent-observer"},
        )

    def _verify_repository(self, spec: DeploymentSpec) -> None:
        if not self.repository_root.is_dir():
            raise BackendFailure("homelab-infra repository root is unavailable")
        head = self._git("rev-parse", "--verify", "HEAD")
        if head != spec.backend.repository_revision:
            raise BackendFailure("homelab-infra HEAD differs from the fenced revision")
        if self._git("status", "--porcelain", "--untracked-files=normal"):
            raise BackendFailure("homelab-infra worktree is not clean")
        remotes = self._git("remote").splitlines()
        urls = {self._git("remote", "get-url", "--all", remote) for remote in remotes if remote}
        flattened = {url for group in urls for url in group.splitlines()}
        if spec.backend.repository not in flattened:
            raise BackendFailure("homelab-infra repository identity is not an exact remote")
        for relative in (spec.backend.inventory, spec.backend.playbook):
            path = (self.repository_root / relative).resolve()
            if self.repository_root not in path.parents or not path.is_file():
                raise BackendFailure(f"bounded Ansible input is unavailable: {relative}")

    def _git(self, *args: str) -> str:
        result = subprocess.run(
            ["git", "-C", os.fspath(self.repository_root), *args],
            check=False,
            capture_output=True,
            text=True,
            timeout=15,
        )
        if result.returncode != 0:
            raise BackendFailure(f"git verification failed: {' '.join(args)}")
        return result.stdout.strip()


def _is_revision(observation: Observation, revision: str, *, require_health: bool) -> bool:
    return (
        observation.installed_revision == revision
        and observation.activated_revision == revision
        and observation.running_revision == revision
        and (observation.healthy is True if require_health else True)
    )
