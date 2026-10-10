"""Guard the failed-canary recovery instructions against premature hold removal."""

from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]


def assert_safe_rollback(guide):
    rollback = guide.split("## Rollback\n", 1)[1].split("## Status as a gate", 1)[0]
    fence = rollback.index(
        "upkeeper hold set --hosts omarchy-pc laptop normandy homelab"
    )
    restore = rollback.index('upkeeper rollback --to "$PREVIOUS"')
    publish_gate = rollback.index(
        "Before clearing any hold, confirm that every remote's `main`"
    )
    ci_gate = rollback.index("corrective release commit.")
    canary = rollback.index("upkeeper pull --hosts omarchy-pc --override-hold")
    clear_gate = rollback.index(
        "Only after the corrective publication, exact-commit CI and canary verification"
    )
    clear = rollback.index(
        "upkeeper hold clear --hosts omarchy-pc laptop normandy homelab"
    )
    assert fence < restore < publish_gate < ci_gate < canary < clear_gate < clear
    fence_command = rollback[fence:].split("```", 1)[0]
    assert "--until" not in fence_command
    assert "leave the entire fleet held\nindefinitely" in rollback
    assert "restoring the safe pin" in rollback
    assert "If publication, CI or canary verification fails, retain every hold." in rollback


def test_failed_canary_retains_fleet_fence_until_verified_corrective_publication():
    assert_safe_rollback((ROOT / "docs/authored-release.md").read_text())


def test_review_reproduction_cannot_authorize_clearing_fleet_holds():
    # The reviewed sequence rolls back locally, holds only the canary, then
    # releases other hosts while main still pins the rejected release.
    unsafe = """## Rollback
upkeeper rollback --to "$PREVIOUS" --hosts omarchy-pc --components t3-steward
upkeeper hold set --hosts omarchy-pc --reason "canary rejected rc.120"
When the rollback is done, clear the holds as in step 6.
upkeeper hold clear --hosts laptop normandy homelab --reason "rollback done"
## Status as a gate
"""
    with pytest.raises((ValueError, AssertionError)):
        assert_safe_rollback(unsafe)
