from pathlib import Path

import pytest

from dev_fleet.config import load_config
from dev_fleet.validation import (
    ValidationError,
    parse_host_selector,
    validate_component_name,
    validate_git_commit,
    validate_home_relative_path,
    validate_source_url,
)

ROOT = Path(__file__).resolve().parents[1]


def test_repository_inventory_and_manifest_parse() -> None:
    config = load_config(ROOT)
    assert list(config.hosts) == ["laptop", "omarchy-pc", "normandy", "homelab"]
    assert config.hosts["omarchy-pc"].ssh_alias == "gaming-pc"
    assert config.hosts["laptop"].canonical_hostname == "omarchy-macbook-air-igor"
    assert config.manifest["deployment_enabled"] is False
    assert config.components["t3-steward"]["pin"]["value"] == (
        "81f1965d614db49891230e8b6c6e23db6c43f806"
    )


@pytest.mark.parametrize(
    "value",
    ["../etc/passwd", "/tmp/payload", ".local//bin", ".local/../.ssh", "tmp/file", ""],
)
def test_home_relative_paths_reject_escape(value: str) -> None:
    with pytest.raises(ValidationError):
        validate_home_relative_path(value)


@pytest.mark.parametrize("value", ["main", "HEAD", "-f", "a" * 39, "A" * 40, "a" * 41])
def test_git_ref_requires_full_lowercase_commit(value: str) -> None:
    with pytest.raises(ValidationError):
        validate_git_commit(value)


@pytest.mark.parametrize(
    "value",
    [
        "https://evil.example/repo.git",
        "http://github.com/iryzhkov/repo.git",
        "https://user:password@github.com/repo.git",
        "git@github.com:../../repo.git",
        "ssh://github.com/iryzhkov/repo.git",
    ],
)
def test_source_url_allowlist(value: str) -> None:
    with pytest.raises(ValidationError):
        validate_source_url(value)


@pytest.mark.parametrize("value", ["*", "normandy;id", "Normandy", "", "-limit"])
def test_host_and_component_names_are_not_expressions(value: str) -> None:
    with pytest.raises(ValidationError):
        validate_component_name(value)


def test_selector_accepts_only_exact_known_hosts() -> None:
    known = {"laptop", "normandy", "homelab"}
    assert parse_host_selector("normandy,laptop", known) == ["normandy", "laptop"]
    with pytest.raises(ValidationError):
        parse_host_selector("norm*", known)
    with pytest.raises(ValidationError):
        parse_host_selector("normandy,normandy", known)
    with pytest.raises(ValidationError):
        parse_host_selector("unknown", known)
