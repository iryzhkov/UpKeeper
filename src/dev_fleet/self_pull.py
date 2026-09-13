"""Local checkout and inventory fences for pull --self."""
from __future__ import annotations

import subprocess
from pathlib import Path

from .config import FleetConfig
from .probe import local_machine_id_sha256
from .validation import ValidationError


def checkout_revision(root: Path) -> str:
    def git(*args: str) -> str:
        try:
            result = subprocess.run(
                ["git", "-C", str(root), *args], capture_output=True,
                text=True, check=False, timeout=30,
            )
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise ValidationError("cannot inspect self checkout") from exc
        if result.returncode:
            raise ValidationError("--self requires a readable Git checkout")
        return result.stdout.strip()

    if Path(git("rev-parse", "--show-toplevel")).resolve() != root.resolve():
        raise ValidationError("--self must use the dev-fleet checkout root")
    if git("status", "--porcelain", "--untracked-files=all"):
        raise ValidationError("--self refuses a dirty checkout; commit or preserve work first")
    return git("rev-parse", "--verify", "HEAD")


def local_host(config: FleetConfig) -> str:
    identity = local_machine_id_sha256()
    matches = [name for name, host in config.hosts.items()
               if identity and host.machine_id_sha256 == identity]
    if len(matches) != 1:
        raise ValidationError("--self requires exactly one inventory match for this machine-id")
    return matches[0]
