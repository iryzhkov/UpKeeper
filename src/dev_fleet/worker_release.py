"""U0 worker bootstrap distribution, independent of S4 enrollment."""

from __future__ import annotations

import os
import re
import stat
from pathlib import Path

from .release_assets import atomic_write, canonical, digest

REF = re.compile(r"secretref:f02-protocol/[a-z][a-z0-9-]{0,62}")
NAME = re.compile(r"[a-z][a-z0-9-]{0,62}")
DESTINATION = ".config/t3-steward/worker-bootstrap.json"


def validate_workers(component):
    if set(component) != {"kind", "hosts"} or component["kind"] != "worker-configuration":
        raise ValueError("worker-configuration must contain only kind and hosts")
    if not isinstance(component["hosts"], dict):
        raise ValueError("worker hosts must be an object")
    for host, entry in component["hosts"].items():
        if not NAME.fullmatch(host) or not isinstance(entry, dict):
            raise ValueError("invalid worker host")
        if set(entry) != {
            "schema_version",
            "worker_id",
            "coordinator_id",
            "transport",
            "capabilities",
            "provider_routes",
            "credential_ref",
        }:
            raise ValueError("unknown worker bootstrap fields; catalogs and secrets are forbidden")
        if entry["schema_version"] != 1 or entry["worker_id"] != host:
            raise ValueError("worker identity must match its inventory host")
        if not NAME.fullmatch(entry["coordinator_id"]):
            raise ValueError("invalid coordinator identity")
        if entry["transport"] != "ssh":
            raise ValueError("remote worker bootstrap transport must be ssh")
        if entry["credential_ref"] != f"secretref:f02-protocol/{host}":
            raise ValueError("worker needs its own opaque f02-protocol secret reference")
        for field in ("capabilities", "provider_routes"):
            values = entry[field]
            if not isinstance(values, list) or values != sorted(set(values)):
                raise ValueError("capabilities and provider routes must be sorted and unique")
            if any(
                not isinstance(v, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._/-]*", v)
                for v in values
            ):
                raise ValueError("invalid capability or route identifier")
        if not {"git", "huyang"} <= set(entry["capabilities"]):
            raise ValueError("worker must declare git and huyang")


def resolve_secret_reference(home: Path, reference: str) -> Path:
    """Resolve metadata only. Never put the value or its hash in a run record."""
    if not REF.fullmatch(reference):
        raise ValueError("invalid f02-protocol reference")
    root = home / ".config/upkeeper/secrets"
    path = root / reference.removeprefix("secretref:")
    if path.is_symlink() or not path.resolve().is_relative_to(root.resolve()):
        raise ValueError("secret reference escapes its target-local store")
    info = path.stat()
    if (
        not stat.S_ISREG(info.st_mode)
        or info.st_uid != os.getuid()
        or stat.S_IMODE(info.st_mode) != 0o600
        or info.st_size == 0
    ):
        raise ValueError("secret reference requires a nonempty owner-only 0600 file")
    return path


def worker_projection(component, home, host, run_id, dry_run=False):
    validate_workers(component)
    entry = component["hosts"].get(host)
    if entry is None:
        return {"state": "converged", "configured": False}
    # No secrets are opened. S4 runtime consumes this same resolver/path contract.
    resolve_secret_reference(home, entry["credential_ref"])
    target = home / DESTINATION
    if target.is_symlink() or not target.resolve().is_relative_to(home.resolve()):
        raise ValueError("unsafe worker bootstrap target")
    payload = canonical(entry)
    before = target.read_bytes() if target.exists() else None
    changed = before != payload
    if changed and not dry_run:
        backup = home / ".local/state/dev-fleet/worker-config" / run_id
        backup.mkdir(parents=True, exist_ok=False, mode=0o700)
        atomic_write(
            backup / "prior.json",
            canonical(
                {
                    "existed": before is not None,
                    "content": before.decode() if before is not None else None,
                }
            ),
            0o600,
        )
        atomic_write(target, payload, 0o600)
        if target.read_bytes() != payload:
            raise RuntimeError("worker bootstrap failed verification")
    return {
        "state": "updated" if changed else "converged",
        "configured": True,
        "configuration_sha256": digest(payload),
        "credential_ref": entry["credential_ref"],
        "enrolled": False,
    }
