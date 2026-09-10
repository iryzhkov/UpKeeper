"""Sanitized local run records for future mutating commands.

Doctor and status deliberately never instantiate this writer.
"""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import asdict, dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from .redaction import redact
from .validation import validate_run_id


@dataclass(frozen=True)
class RunRecord:
    schema_version: int
    run_id: str
    command: str
    controller: str
    desired_manifest_commit: str | None
    components: list[str]
    hosts: list[str]
    phase: str
    result: str
    created_at: str
    details: dict[str, Any]

    @classmethod
    def new(
        cls,
        *,
        run_id: str,
        command: str,
        controller: str,
        desired_manifest_commit: str | None,
        components: list[str],
        hosts: list[str],
    ) -> RunRecord:
        return cls(
            schema_version=1,
            run_id=validate_run_id(run_id),
            command=command,
            controller=controller,
            desired_manifest_commit=desired_manifest_commit,
            components=components,
            hosts=hosts,
            phase="created",
            result="running",
            created_at=datetime.now(UTC).isoformat().replace("+00:00", "Z"),
            details={},
        )


class RunRecordStore:
    """Atomic writer with a caller-supplied root; writing is always explicit."""

    def __init__(self, root: Path) -> None:
        self.root = root.expanduser().resolve()

    @classmethod
    def default(cls) -> RunRecordStore:
        return cls(Path.home() / ".local" / "state" / "dev-fleet" / "runs")

    def write(self, record: RunRecord) -> Path:
        validate_run_id(record.run_id)
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        os.chmod(self.root, 0o700)
        target = self.root / f"{record.run_id}.json"
        payload = json.dumps(redact(asdict(record)), indent=2, sort_keys=True) + "\n"
        descriptor, temporary = tempfile.mkstemp(prefix=".run-", dir=self.root)
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, target)
        except BaseException:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass
            raise
        return target

    def read(self, run_id: str) -> dict[str, Any]:
        path = self.root / f"{validate_run_id(run_id)}.json"
        return json.loads(path.read_text(encoding="utf-8"))

    def list_ids(self) -> list[str]:
        if not self.root.is_dir():
            return []
        values = []
        for path in self.root.glob("*.json"):
            try:
                values.append(validate_run_id(path.stem))
            except ValueError:
                continue
        return sorted(values)
