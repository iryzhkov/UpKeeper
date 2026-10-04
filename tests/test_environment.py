from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest

from dev_fleet.environment import (
    AgentEnvironmentSpec,
    DriftState,
    ProbeResult,
    RecoveryObservation,
    behavior_is_equivalent_or_stricter,
    classify_asset_drift,
    environment_from_dict,
    environment_to_dict,
    project_recovery_inventory,
)

FIXTURE = Path(__file__).parent / "fixtures" / "agent-environment-v1.json"
EVIDENCE = "a" * 64


def load_raw() -> dict:
    return json.loads(FIXTURE.read_text(encoding="utf-8"))


def test_fixture_round_trips_with_stable_revision() -> None:
    spec = environment_from_dict(load_raw())
    restored = environment_from_dict(environment_to_dict(spec))
    assert restored == spec
    assert spec.revision == restored.revision
    assert len(spec.revision) == 64
    assert spec.adapter == "local-single-worker-v1"


@pytest.mark.parametrize(
    ("mutation", "message"),
    [
        (lambda raw: raw.update({"unexpected": True}), "fields mismatch"),
        (lambda raw: raw.update({"worker_ids": ["one", "two"]}), "exactly one worker"),
        (
            lambda raw: raw.update({"phases": ["activate", "install", "probe", "rollback"]}),
            "phases",
        ),
        (lambda raw: raw["secret_refs"].append("plaintext"), "secretref"),
        (
            lambda raw: raw["assets"][0].update({"staged_path": raw["assets"][0]["active_path"]}),
            "staging",
        ),
        (lambda raw: raw["probes"][0].update({"argv": ["sh", "-c", "true"]}), "absolute"),
    ],
)
def test_invalid_environment_fails_closed(mutation, message: str) -> None:
    raw = copy.deepcopy(load_raw())
    mutation(raw)
    with pytest.raises(ValueError, match=message):
        environment_from_dict(raw)


def test_drift_preserves_distinct_failure_states() -> None:
    spec = environment_from_dict(load_raw())
    observed = {
        spec.assets[0].asset_id: (spec.assets[0].sha256, "evidence:one"),
        spec.assets[1].asset_id: ("f" * 64, "evidence:two"),
        spec.assets[2].asset_id: (None, "evidence:three"),
    }
    states = {
        item.subject: item.state
        for item in classify_asset_drift(spec, observed, ["/home/igor/.config/local-change"])
    }
    assert states[spec.assets[0].asset_id] is DriftState.MATCH
    assert states[spec.assets[1].asset_id] is DriftState.MISMATCH
    assert states[spec.assets[2].asset_id] is DriftState.MISSING
    assert states[spec.assets[3].asset_id] is DriftState.UNAVAILABLE
    assert states["/home/igor/.config/local-change"] is DriftState.UNEXPLAINED


def test_behavior_requires_complete_non_regressing_probe_evidence() -> None:
    spec = environment_from_dict(load_raw())
    baseline = [
        ProbeResult(item.probe_id, True, item.safety_claims, EVIDENCE) for item in spec.probes
    ]
    candidate = [
        ProbeResult(item.probe_id, True, item.safety_claims + ("stricter",), EVIDENCE)
        for item in spec.probes
    ]
    assert behavior_is_equivalent_or_stricter(spec.probes, baseline, candidate)
    assert not behavior_is_equivalent_or_stricter(spec.probes, baseline, candidate[:-1])
    failed = list(candidate)
    failed[0] = ProbeResult(failed[0].probe_id, False, failed[0].safety_claims, EVIDENCE)
    assert not behavior_is_equivalent_or_stricter(spec.probes, baseline, failed)


def test_recovery_inventory_retains_owner_and_unavailable_sources() -> None:
    spec = environment_from_dict(load_raw())
    observation = RecoveryObservation("lease", "t3-steward", "ready", ("lease-1",), EVIDENCE)
    projected = project_recovery_inventory(spec, {"lease": observation})
    assert {item.kind: item for item in projected}["lease"] == observation
    assert {item.kind for item in projected if item.status == "unavailable"} == {
        "huyang-journal",
        "t3-thread",
        "worktree",
    }
    with pytest.raises(ValueError, match="owner"):
        project_recovery_inventory(
            spec,
            {"lease": RecoveryObservation("lease", "UpKeeper", "ready", (), EVIDENCE)},
        )


def test_spec_contains_references_not_secret_values() -> None:
    spec = environment_from_dict(load_raw())
    payload = environment_to_dict(spec)
    assert payload["secret_refs"] == ["secretref:codex-auth"]
    assert "token" not in json.dumps(payload).lower()
    assert isinstance(spec, AgentEnvironmentSpec)


def test_fixture_covers_every_frozen_worker_profile() -> None:
    fixture = json.loads(
        (FIXTURE.parent / "h03-environment-faults-v1.json").read_text(encoding="utf-8")
    )
    environment = environment_from_dict(load_raw())
    assert fixture["schema_version"] == 1
    assert fixture["profiles"] == [
        {
            "host_profile": environment.host_profile,
            "worker_id": environment.worker_ids[0],
            "host": "normandy",
        }
    ]
    assert {item["fault_id"] for item in fixture["faults"]} == {
        "partial-install",
        "weakened-safety",
        "bad-environment-revision",
        "managed-drift",
        "unexplained-drift",
    }
