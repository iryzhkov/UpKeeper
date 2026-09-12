import json
from pathlib import Path

import pytest

from dev_fleet.rollout import RolloutPlan, RolloutStage


def test_frozen_rollout_fixture_has_bounded_deterministic_progression() -> None:
    raw = json.loads(
        (Path(__file__).parent / "fixtures" / "h02-rollout-plan-v1.json").read_text()
    )
    plan = RolloutPlan(
        raw["schema_version"],
        tuple(raw["hosts"]),
        tuple(raw["canary_hosts"]),
        raw["batch_size"],
        raw["soak_seconds"],
    )
    steps = plan.steps()

    assert [step.stage for step in steps] == [
        RolloutStage.INSTALL_ONLY,
        RolloutStage.CANARY,
        RolloutStage.SOAK,
        RolloutStage.BATCH,
        RolloutStage.BATCH,
        RolloutStage.FLEET,
    ]
    assert steps[0].hosts == plan.hosts
    assert steps[1].hosts == steps[2].hosts == ("normandy",)
    assert [step.hosts for step in steps[3:-1]] == [
        ("homelab", "laptop"),
        ("omarchy-pc",),
    ]
    assert all(len(step.hosts) <= plan.batch_size for step in steps[3:-1])
    assert len(plan.plan_hash) == 64


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("canary_hosts", ("missing",)),
        ("batch_size", 0),
        ("soak_seconds", 0),
    ],
)
def test_rollout_plan_rejects_unbounded_or_unknown_progression(field, value) -> None:
    arguments = {
        "schema_version": 1,
        "hosts": ("homelab", "laptop", "normandy"),
        "canary_hosts": ("normandy",),
        "batch_size": 2,
        "soak_seconds": 3600,
    }
    arguments[field] = value
    with pytest.raises(ValueError):
        RolloutPlan(**arguments)
