import json

import pytest

from dev_fleet.worker_lifecycle import (
    WORKER_UNIT,
    WorkerRestartFailed,
    restart_persistent_worker,
)


class FakeHost:
    """Records the commands a restart would run against one target host."""

    def __init__(self, output, ok=True, loaded=True, active=True):
        self.output = output
        self.ok = ok
        self.loaded = loaded
        self.active = active
        self.commands: list[list[str]] = []

    def run(self, argv, **kwargs):
        return {"ok": self.ok, "rc": 0 if self.ok else 1, "output": self.output}

    def must(self, argv, **kwargs):
        self.commands.append(list(argv))
        return ""

    def unit_loaded(self, unit):
        return self.loaded and unit == WORKER_UNIT

    def unit_active(self, unit):
        return self.active

    def restart(self, actions, dry_run=False):
        restart_persistent_worker(
            "/home/igor/.local/bin/t3-steward", actions, run=self.run, must=self.must,
            unit_loaded=self.unit_loaded, unit_active=self.unit_active, dry_run=dry_run,
        )


def summary(dispatched):
    return json.dumps({
        "workerId": "homelab", "attempts": dispatched, "dispatched": dispatched, "phases": {},
    })


def test_idle_worker_is_restarted_onto_the_installed_binary() -> None:
    host = FakeHost(summary(0))
    actions: list[str] = []
    host.restart(actions)
    assert host.commands == [["systemctl", "--user", "restart", WORKER_UNIT]]
    assert actions == [f"restarted {WORKER_UNIT} onto the pinned steward"]


def test_dispatched_execution_defers_the_worker_restart() -> None:
    host = FakeHost(summary(2))
    actions: list[str] = []
    host.restart(actions)
    assert host.commands == []
    assert actions == ["worker restart deferred: 2 dispatched attempt(s) in the journal"]


def test_host_without_the_worker_unit_is_untouched() -> None:
    host = FakeHost(summary(0), loaded=False)
    actions: list[str] = []
    host.restart(actions)
    assert host.commands == [] and actions == []


def test_unreadable_journal_fails_instead_of_restarting() -> None:
    failing = FakeHost("boom", ok=False)
    with pytest.raises(WorkerRestartFailed):
        failing.restart([])
    unparsable = FakeHost("not json")
    with pytest.raises(WorkerRestartFailed):
        unparsable.restart([])
    assert failing.commands == [] and unparsable.commands == []


def test_worker_that_stays_down_is_reported() -> None:
    host = FakeHost(summary(0), active=False)
    with pytest.raises(WorkerRestartFailed):
        host.restart([])
    assert host.commands == [["systemctl", "--user", "restart", WORKER_UNIT]]


def test_dry_run_reports_the_restart_without_performing_it() -> None:
    host = FakeHost(summary(0))
    actions: list[str] = []
    host.restart(actions, dry_run=True)
    assert host.commands == []
    assert actions == [f"would restart {WORKER_UNIT} onto the pinned steward"]
