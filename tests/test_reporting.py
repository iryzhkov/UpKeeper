from pathlib import Path

from dev_fleet.config import load_config
from dev_fleet.constants import EXIT_DRIFT, EXIT_PREFLIGHT
from dev_fleet.reporting import build_doctor_report, build_status_report

ROOT = Path(__file__).resolve().parents[1]


def observation(name: str) -> dict:
    config = load_config(ROOT)
    spec = config.hosts[name]
    return {
        "reachable": True,
        "transport": "local",
        "hostname": spec.canonical_hostname,
        "user": spec.user,
        "architecture": spec.architecture,
        "machine_id_sha256": spec.machine_id_sha256,
        "os": {"id": spec.os_id, "version": spec.os_version},
        "python": {"version": "3.11.2", "executable": "/usr/bin/python3"},
        "ansible": {"available": False, "version": None},
        "systemd_user": {"state": "running", "linger": "yes"},
        "forgejo_access": True,
        "source_access": {
            component: {"reachable": True, "via": "ssh"}
            for component in ("t3-steward", "agent99", "nvim-configuration", "homelab-cli")
        },
        "locks": [],
        "components": {
            "t3-steward": {
                "version": ("t3-steward 0.10.1 (commit 81f1965d614db49891230e8b6c6e23db6c43f806)"),
                "service": {"ActiveState": "active", "UnitFileState": "enabled"},
            },
            "agent99": {"checkout": {"present": True, "head": "f" * 40}, "active_bridges": []},
            "nvim-configuration": {"checkout": {"present": True, "head": "e" * 40}},
            "homelab-cli": {"checkout": {"present": False}},
            "agent-config": {"config": {"exists": True}},
        },
    }


def test_doctor_reports_prerequisite_failure_without_claiming_success() -> None:
    config = load_config(ROOT)
    report, code = build_doctor_report(config, {"normandy": observation("normandy")})
    assert code == EXIT_PREFLIGHT
    assert report["result"] == "failed"
    assert "ansible-controller" in report["hosts"][0]["failed_checks"]


def test_status_has_stable_desired_installed_activated_running_fields() -> None:
    config = load_config(ROOT)
    report, code = build_status_report(config, {"normandy": observation("normandy")})
    assert code == EXIT_DRIFT
    component = next(
        item for item in report["hosts"][0]["components"] if item["name"] == "t3-steward"
    )
    assert component["state"] == "converged"
    assert set(component) >= {"desired", "installed", "activated", "running"}
    assert report["schema_version"] == 1


def test_offline_host_is_explicit() -> None:
    config = load_config(ROOT)
    report, _code = build_status_report(
        config,
        {"laptop": {"reachable": False, "transport": "ssh", "error": "TimeoutExpired"}},
    )
    assert report["result"] == "partial"
    assert report["hosts"][0]["result"] == "offline"
