"""Independent local observation for foundation component adoption."""

from __future__ import annotations

import hashlib
import subprocess
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path

from .upkeeper import BackendFailure, DeploymentSpec, Observation


@dataclass(frozen=True)
class ProbeResult:
    returncode: int


def _run(argv: Sequence[str], timeout_seconds: int) -> ProbeResult:
    try:
        result = subprocess.run(
            list(argv),
            check=False,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=timeout_seconds,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise BackendFailure("foundation health observation was ambiguous") from exc
    return ProbeResult(result.returncode)


class FoundationObserver:
    """Observe exact local bytes and health without changing target state."""

    def __init__(self, *, timeout_seconds: int = 30) -> None:
        if timeout_seconds < 1 or timeout_seconds > 300:
            raise ValueError("timeout_seconds must be between 1 and 300")
        self.timeout_seconds = timeout_seconds

    def __call__(self, spec: DeploymentSpec) -> Observation:
        adoption = spec.adoption
        if adoption is None:
            raise BackendFailure("foundation observation requires adoption intent")

        installed = Path(adoption.install_path)
        if not installed.is_file():
            return Observation(None, None, None, False, {"probe": "absent"})

        installed_digest = _sha256(installed)
        config_digest = (
            _sha256(Path(adoption.configuration.path))
            if adoption.configuration is not None and Path(adoption.configuration.path).is_file()
            else None
        )
        desired_exact = installed_digest == adoption.artifact.sha256 and (
            adoption.configuration is None or config_digest == adoption.configuration.sha256
        )
        rollback = adoption.rollback
        rollback_exact = (
            spec.previous_revision is not None
            and rollback.artifact is not None
            and installed_digest == rollback.artifact.sha256
            and (
                rollback.configuration is None
                or config_digest == rollback.configuration.sha256
            )
        )
        healthy_probe = _run(adoption.health_command, self.timeout_seconds).returncode == 0

        if desired_exact:
            revision = spec.desired_revision
        elif rollback_exact:
            revision = spec.previous_revision
        else:
            revision = _marker_revision(installed)
        installed_revision = revision if installed_digest is not None else None
        activated_revision = revision if revision is not None else None
        running_revision = revision if revision is not None and healthy_probe else None
        return Observation(
            installed_revision,
            activated_revision,
            running_revision,
            healthy_probe and (desired_exact or rollback_exact),
            {
                "probe": "foundation-local-v1",
                "desired_exact_bytes": desired_exact,
                "rollback_exact_bytes": rollback_exact,
            },
            installed_digest,
            installed_digest,
            config_digest,
        )


def _marker_revision(installed: Path) -> str | None:
    marker = installed.with_name(installed.name + ".upkeeper-revision")
    try:
        value = marker.read_text(encoding="utf-8").strip()
    except (OSError, UnicodeError):
        return None
    if len(value) == 40 and all(char in "0123456789abcdef" for char in value):
        return value
    return None


def _sha256(path: Path) -> str | None:
    try:
        digest = hashlib.sha256()
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
        return digest.hexdigest()
    except OSError:
        return None
