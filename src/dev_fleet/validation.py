"""Strict validation for every value that could reach a command or filesystem path."""

from __future__ import annotations

import ipaddress
import re
from pathlib import PurePosixPath
from urllib.parse import urlsplit

COMPONENT_RE = re.compile(r"^[a-z][a-z0-9-]{0,63}$")
HOST_RE = re.compile(r"^[a-z][a-z0-9-]{0,63}$")
HOSTNAME_RE = re.compile(
    r"^(?=.{1,253}$)(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*"
    r"[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$"
)
GIT_COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
MACHINE_HASH_RE = re.compile(r"^[0-9a-f]{64}$")
SSH_FINGERPRINT_RE = re.compile(r"^SHA256:[A-Za-z0-9+/]{42,44}=?$")
RUN_ID_RE = re.compile(r"^\d{8}T\d{6}Z-[0-9a-f]{8}$")
SCP_GIT_RE = re.compile(r"^(?P<user>git)@(?P<host>[A-Za-z0-9.-]+):(?P<path>[A-Za-z0-9._/-]+\.git)$")
ALLOWED_SOURCE_HOSTS = {"github.com", "git.ryzhkov.dev"}
ALLOWED_HOME_ROOTS = {".config", ".local"}


class ValidationError(ValueError):
    """An input failed a security boundary."""


def validate_component_name(name: str, known: set[str] | None = None) -> str:
    if not COMPONENT_RE.fullmatch(name):
        raise ValidationError(f"invalid component name: {name!r}")
    if known is not None and name not in known:
        raise ValidationError(f"unknown component: {name}")
    return name


def validate_host_name(name: str, known: set[str] | None = None) -> str:
    if not HOST_RE.fullmatch(name):
        raise ValidationError(f"invalid host selector: {name!r}")
    if known is not None and name not in known:
        raise ValidationError(f"unknown host: {name}")
    return name


def parse_host_selector(raw: str | None, known: set[str]) -> list[str]:
    if raw is None:
        return sorted(known)
    if not raw or raw.strip() != raw:
        raise ValidationError("host selector may not be empty or padded")
    parts = raw.split(",")
    if any(not part for part in parts):
        raise ValidationError("host selector contains an empty entry")
    selected = [validate_host_name(part, known) for part in parts]
    if len(selected) != len(set(selected)):
        raise ValidationError("host selector contains duplicates")
    return selected


def validate_git_commit(value: str) -> str:
    if not GIT_COMMIT_RE.fullmatch(value):
        raise ValidationError("git refs must be full lowercase 40-character commits")
    return value


def validate_home_relative_path(value: str) -> str:
    if not value or any(ord(char) < 32 for char in value):
        raise ValidationError("path is empty or contains control characters")
    path = PurePosixPath(value)
    if path.is_absolute() or value.endswith("/") or "//" in value:
        raise ValidationError(f"path must be a normalized home-relative path: {value!r}")
    if any(part in {"", ".", ".."} for part in path.parts):
        raise ValidationError(f"path traversal is forbidden: {value!r}")
    if path.parts[0] not in ALLOWED_HOME_ROOTS:
        raise ValidationError(f"path must stay below .config or .local: {value!r}")
    if str(path) != value:
        raise ValidationError(f"path is not normalized: {value!r}")
    return value


def validate_source_url(value: str) -> str:
    match = SCP_GIT_RE.fullmatch(value)
    if match:
        if match.group("host") not in ALLOWED_SOURCE_HOSTS or ".." in match.group("path"):
            raise ValidationError(f"source URL is not allowed: {value!r}")
        return value

    parsed = urlsplit(value)
    if (
        parsed.scheme != "https"
        or parsed.hostname not in ALLOWED_SOURCE_HOSTS
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
        or not parsed.path.endswith(".git")
        or ".." in PurePosixPath(parsed.path).parts
    ):
        raise ValidationError(f"source URL is not allowed: {value!r}")
    return value


def validate_address(value: str) -> str:
    try:
        ipaddress.ip_address(value)
    except ValueError:
        if not HOSTNAME_RE.fullmatch(value):
            raise ValidationError(f"invalid host address: {value!r}") from None
    return value


def validate_run_id(value: str) -> str:
    if not RUN_ID_RE.fullmatch(value):
        raise ValidationError(f"invalid run ID: {value!r}")
    return value
