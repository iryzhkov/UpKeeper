"""Central redaction used before any user-visible or persisted serialization."""

from __future__ import annotations

import re
from collections.abc import Mapping, Sequence
from typing import Any

REDACTED = "<redacted>"

SECRET_KEY_RE = re.compile(
    r"(?i)(?:^|[_-])(?:authorization|cookie|credential|password|passwd|private[_-]?key|"
    r"secret|token|api[_-]?key)(?:$|[_-])"
)
PRIVATE_KEY_RE = re.compile(
    r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----",
    re.DOTALL,
)
AUTH_RE = re.compile(r"(?i)(authorization\s*:\s*(?:bearer|token|basic)\s+)[^\s,;]+")
ASSIGNMENT_RE = re.compile(
    r"(?i)\b(password|passwd|secret|token|api[_-]?key|private[_-]?key)\s*([=:])\s*"
    r"([^\s,;]+)"
)
QUERY_RE = re.compile(r"(?i)([?&](?:access_token|api_key|key|password|secret|token)=)[^&\s]+")
TOKEN_RE = re.compile(
    r"(?<![A-Za-z0-9])(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|"
    r"gitea_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{20,}|AGE-SECRET-KEY-[A-Z0-9-]{20,})"
)
JWT_RE = re.compile(r"(?<![A-Za-z0-9_-])[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}")
URL_USERINFO_RE = re.compile(r"(?P<scheme>https?|ssh)://[^\s/@:]+:[^\s/@]+@")


def redact_text(value: str) -> str:
    value = PRIVATE_KEY_RE.sub(REDACTED, value)
    value = URL_USERINFO_RE.sub(lambda match: f"{match.group('scheme')}://{REDACTED}@", value)
    value = AUTH_RE.sub(lambda match: f"{match.group(1)}{REDACTED}", value)
    value = ASSIGNMENT_RE.sub(lambda match: f"{match.group(1)}{match.group(2)}{REDACTED}", value)
    value = QUERY_RE.sub(lambda match: f"{match.group(1)}{REDACTED}", value)
    value = TOKEN_RE.sub(REDACTED, value)
    return JWT_RE.sub(REDACTED, value)


def redact(value: Any, key: str | None = None) -> Any:
    if key is not None and SECRET_KEY_RE.search(key):
        return REDACTED
    if isinstance(value, str):
        return redact_text(value)
    if isinstance(value, Mapping):
        return {
            str(item_key): redact(item_value, str(item_key))
            for item_key, item_value in value.items()
        }
    if isinstance(value, Sequence) and not isinstance(value, (str, bytes, bytearray)):
        return [redact(item) for item in value]
    return value
