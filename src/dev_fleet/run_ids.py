"""Opaque, sortable run identifiers."""

from __future__ import annotations

import secrets
from datetime import UTC, datetime

from .validation import validate_run_id


def new_run_id(now: datetime | None = None, entropy: str | None = None) -> str:
    timestamp = (now or datetime.now(UTC)).astimezone(UTC).strftime("%Y%m%dT%H%M%SZ")
    suffix = entropy or secrets.token_hex(4)
    value = f"{timestamp}-{suffix}"
    return validate_run_id(value)
