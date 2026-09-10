from datetime import UTC, datetime, timedelta

import pytest

from dev_fleet.locks import (
    LocalLockStore,
    LockContentionError,
    LockMetadata,
    acquire_all,
    ordered_hosts,
    stale_lock_confirmed,
)


def metadata(run_id: str = "20260910T010203Z-deadbeef") -> LockMetadata:
    return LockMetadata.new(
        run_id=run_id,
        controller="normandy",
        controller_pid=1234,
        desired_manifest_commit="a" * 40,
        components=["t3-steward"],
        now=datetime(2026, 9, 10, tzinfo=UTC),
    )


def test_hosts_lock_in_deterministic_order() -> None:
    assert ordered_hosts(["normandy", "homelab", "laptop"]) == [
        "homelab",
        "laptop",
        "normandy",
    ]


def test_contention_never_overwrites_owner(tmp_path) -> None:
    store = LocalLockStore(tmp_path / "locks")
    first = metadata()
    store.acquire("laptop", first)
    with pytest.raises(LockContentionError):
        store.acquire("laptop", metadata("20260910T010204Z-feedface"))
    assert store.inspect("laptop").metadata == first


def test_partial_acquisition_is_released_on_contention(tmp_path) -> None:
    store = LocalLockStore(tmp_path / "locks")
    store.acquire("normandy", metadata("20260910T010204Z-feedface"))
    with pytest.raises(LockContentionError):
        acquire_all(store, ["laptop", "normandy"], metadata())
    assert store.inspect("laptop").state == "absent"
    assert store.inspect("normandy").state == "held"


def test_release_requires_matching_run(tmp_path) -> None:
    store = LocalLockStore(tmp_path / "locks")
    store.acquire("homelab", metadata())
    with pytest.raises(LockContentionError):
        store.release("homelab", "20260910T010204Z-feedface")
    assert store.inspect("homelab").state == "held"


def test_old_lock_is_not_stale_when_controller_state_is_uncertain() -> None:
    item = metadata()
    now = datetime(2026, 9, 10, tzinfo=UTC) + timedelta(hours=2)
    assert not stale_lock_confirmed(
        item,
        now=now,
        maximum_age_seconds=60,
        controller_is_alive=lambda _host, _pid: None,
    )
    assert stale_lock_confirmed(
        item,
        now=now,
        maximum_age_seconds=60,
        controller_is_alive=lambda _host, _pid: False,
    )
