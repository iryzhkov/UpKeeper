from pathlib import Path

import pytest

from dev_fleet import cli
from dev_fleet.validation import ValidationError


def test_installed_upkeeper_discovers_conventional_config(monkeypatch, tmp_path: Path) -> None:
    root = tmp_path / ".config" / "dev-fleet"
    (root / "inventory").mkdir(parents=True)
    (root / "desired").mkdir()
    (root / "inventory" / "hosts.yml").write_text("{}\n")
    (root / "desired" / "components.yml").write_text("{}\n")
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.delenv("DEV_FLEET_CONFIG_ROOT", raising=False)
    monkeypatch.setattr(cli, "__file__", str(tmp_path / ".local/libexec/upkeeper"))

    assert cli._root() == root.resolve()


def test_config_override_is_absolute_and_complete(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setenv("DEV_FLEET_CONFIG_ROOT", "relative")
    with pytest.raises(ValidationError, match="absolute"):
        cli._root()

    monkeypatch.setenv("DEV_FLEET_CONFIG_ROOT", str(tmp_path / "missing"))
    monkeypatch.setattr(cli, "__file__", str(tmp_path / ".local/libexec/upkeeper"))
    monkeypatch.setenv("HOME", str(tmp_path / "empty-home"))
    with pytest.raises(ValidationError, match="configuration unavailable"):
        cli._root()
