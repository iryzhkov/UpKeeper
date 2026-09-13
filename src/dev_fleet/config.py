"""Load the JSON-compatible YAML sources of truth and validate their full shape."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .constants import SCHEMA_VERSION
from .validation import (
    MACHINE_HASH_RE,
    SSH_FINGERPRINT_RE,
    ValidationError,
    validate_address,
    validate_component_name,
    validate_git_commit,
    validate_home_relative_path,
    validate_host_name,
    validate_source_url,
)


@dataclass(frozen=True)
class HostSpec:
    name: str
    address: str
    user: str
    canonical_hostname: str
    architecture: str
    os_id: str
    os_version: str
    machine_id_sha256: str
    ssh_host_ed25519_sha256: str
    stable_address_verified: bool
    ssh_alias: str | None = None
    stable_name: str | None = None
    nebula_address: str | None = None
    setup_checkout: str | None = None


@dataclass(frozen=True)
class FleetConfig:
    root: Path
    hosts: dict[str, HostSpec]
    manifest: dict[str, Any]

    @property
    def components(self) -> dict[str, dict[str, Any]]:
        return self.manifest["components"]


def _load_json_yaml(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValidationError(f"cannot load {path.name}: {exc}") from exc
    if not isinstance(value, dict):
        raise ValidationError(f"{path.name} must contain an object")
    return value


def _required_string(mapping: dict[str, Any], key: str, context: str) -> str:
    value = mapping.get(key)
    if not isinstance(value, str) or not value:
        raise ValidationError(f"{context}.{key} must be a non-empty string")
    return value


def load_inventory(path: Path) -> dict[str, HostSpec]:
    data = _load_json_yaml(path)
    if data.get("all", {}).get("vars", {}).get("fleet_inventory_schema_version") != SCHEMA_VERSION:
        raise ValidationError("unsupported inventory schema_version")
    raw_hosts = data.get("all", {}).get("hosts")
    if not isinstance(raw_hosts, dict) or not raw_hosts:
        raise ValidationError("inventory all.hosts must be a non-empty object")

    hosts: dict[str, HostSpec] = {}
    for name, raw in raw_hosts.items():
        validate_host_name(name)
        if not isinstance(raw, dict):
            raise ValidationError(f"inventory host {name} must be an object")
        address = validate_address(_required_string(raw, "ansible_host", name))
        user = _required_string(raw, "ansible_user", name)
        if not re_full_user(user):
            raise ValidationError(f"{name}.ansible_user is invalid")
        canonical = _required_string(raw, "canonical_hostname", name)
        validate_address(canonical)
        machine_hash = _required_string(raw, "machine_id_sha256", name)
        if not MACHINE_HASH_RE.fullmatch(machine_hash):
            raise ValidationError(f"{name}.machine_id_sha256 is invalid")
        fingerprint = _required_string(raw, "ssh_host_ed25519_sha256", name)
        if not SSH_FINGERPRINT_RE.fullmatch(fingerprint):
            raise ValidationError(f"{name}.ssh_host_ed25519_sha256 is invalid")
        stable = raw.get("stable_address_verified")
        if not isinstance(stable, bool):
            raise ValidationError(f"{name}.stable_address_verified must be boolean")
        for optional_address in ("stable_name", "nebula_address"):
            if raw.get(optional_address) is not None:
                validate_address(raw[optional_address])
        if raw.get("ssh_alias") is not None:
            validate_host_name(raw["ssh_alias"])
        if raw.get("setup_checkout") is not None:
            validate_home_relative_path(raw["setup_checkout"], frozenset({".local", "Work"}))
        hosts[name] = HostSpec(
            name=name,
            address=address,
            user=user,
            canonical_hostname=canonical,
            architecture=_required_string(raw, "architecture", name),
            os_id=_required_string(raw, "os_id", name),
            os_version=_required_string(raw, "os_version", name),
            machine_id_sha256=machine_hash,
            ssh_host_ed25519_sha256=fingerprint,
            stable_address_verified=stable,
            ssh_alias=raw.get("ssh_alias"),
            stable_name=raw.get("stable_name"),
            nebula_address=raw.get("nebula_address"),
            setup_checkout=raw.get("setup_checkout"),
        )
    return hosts


def re_full_user(value: str) -> bool:
    return bool(value) and len(value) <= 32 and value.replace("_", "").replace("-", "").isalnum()


def load_manifest(path: Path) -> dict[str, Any]:
    data = _load_json_yaml(path)
    if data.get("schema_version") != SCHEMA_VERSION:
        raise ValidationError("unsupported desired-state schema_version")
    enabled = data.get("deployment_enabled")
    if not isinstance(enabled, bool):
        raise ValidationError("deployment_enabled must be boolean")
    components = data.get("components")
    if not isinstance(components, dict) or not components:
        raise ValidationError("components must be a non-empty object")

    names = set(components)
    for name, component in components.items():
        validate_component_name(name)
        if not isinstance(component, dict):
            raise ValidationError(f"component {name} must be an object")
        source = component.get("source")
        if not isinstance(source, dict):
            raise ValidationError(f"{name}.source must be an object")
        validate_source_url(_required_string(source, "canonical", name))
        mirrors = source.get("allowed_mirrors")
        if not isinstance(mirrors, list) or not all(isinstance(item, str) for item in mirrors):
            raise ValidationError(f"{name}.source.allowed_mirrors must be a string list")
        for mirror in mirrors:
            validate_source_url(mirror)

        pin = component.get("pin")
        if pin is None:
            if enabled:
                raise ValidationError(f"{name} has no immutable pin while deployment is enabled")
        elif not isinstance(pin, dict) or pin.get("kind") != "git":
            raise ValidationError(f"{name}.pin must be a git pin or null")
        else:
            validate_git_commit(_required_string(pin, "value", name))

        paths = component.get("paths")
        if not isinstance(paths, dict) or not paths:
            raise ValidationError(f"{name}.paths must be a non-empty object")
        for value in paths.values():
            if not isinstance(value, str):
                raise ValidationError(f"{name}.paths values must be strings")
            validate_home_relative_path(value)

        architectures = component.get("architectures")
        if not isinstance(architectures, list) or not architectures:
            raise ValidationError(f"{name}.architectures must be non-empty")
        if any(item not in {"aarch64", "x86_64"} for item in architectures):
            raise ValidationError(f"{name}.architectures contains an unsupported value")
        dependencies = component.get("dependencies")
        if not isinstance(dependencies, list) or any(item not in names for item in dependencies):
            raise ValidationError(f"{name}.dependencies contains an unknown component")
        order = component.get("activation_order")
        if not isinstance(order, int) or order < 0:
            raise ValidationError(f"{name}.activation_order must be a non-negative integer")

    _validate_dependency_graph(components)
    return data


def _validate_dependency_graph(components: dict[str, dict[str, Any]]) -> None:
    visiting: set[str] = set()
    visited: set[str] = set()

    def visit(name: str) -> None:
        if name in visiting:
            raise ValidationError("component dependency graph contains a cycle")
        if name in visited:
            return
        visiting.add(name)
        for dependency in components[name]["dependencies"]:
            visit(dependency)
        visiting.remove(name)
        visited.add(name)

    for component_name in components:
        visit(component_name)


def load_config(root: Path) -> FleetConfig:
    resolved = root.resolve()
    return FleetConfig(
        root=resolved,
        hosts=load_inventory(resolved / "inventory" / "hosts.yml"),
        manifest=load_manifest(resolved / "desired" / "components.yml"),
    )
