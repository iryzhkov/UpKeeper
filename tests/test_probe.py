import subprocess
from pathlib import Path
from unittest.mock import patch

from dev_fleet.config import load_config
from dev_fleet.probe import probe_host

ROOT = Path(__file__).resolve().parents[1]


def test_remote_probe_uses_strict_host_keys_and_stdin(monkeypatch) -> None:
    config = load_config(ROOT)
    spec = config.hosts["laptop"]
    monkeypatch.setattr("dev_fleet.probe.local_machine_id_sha256", lambda: "0" * 64)
    captured = {}

    def fake_run(command, **kwargs):
        captured["command"] = command
        captured["input"] = kwargs["input"]
        return subprocess.CompletedProcess(
            command,
            0,
            stdout='{"hostname":"omarchy-macbook-air-igor"}',
            stderr="",
        )

    with patch("dev_fleet.probe.subprocess.run", fake_run):
        result = probe_host(spec, timeout=20)

    assert result["reachable"] is True
    assert "StrictHostKeyChecking=yes" in captured["command"]
    assert captured["command"][-2:] == ["python3", "-"]
    assert "Path.home()" in captured["input"]
    assert not any(word in captured["input"] for word in ("mkdir(", "unlink(", "replace("))
