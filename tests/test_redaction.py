import json
from dataclasses import replace
from datetime import UTC, datetime

from dev_fleet.records import RunRecord, RunRecordStore
from dev_fleet.redaction import REDACTED, redact, redact_text
from dev_fleet.run_ids import new_run_id


def test_redacts_structured_secret_keys_recursively() -> None:
    value = {
        "token": "visible-no-more",
        "nested": {"api_key": "also-hidden", "safe": "kept"},
        "items": [{"password": "hidden"}],
    }
    result = redact(value)
    assert result["token"] == REDACTED
    assert result["nested"]["api_key"] == REDACTED
    assert result["nested"]["safe"] == "kept"
    assert result["items"][0]["password"] == REDACTED


def test_opaque_secret_refs_remain_serializable() -> None:
    assert redact({"secret_refs": ["secretref:codex-auth"]}) == {
        "secret_refs": ["secretref:codex-auth"]
    }


def test_redacts_representative_secret_shapes() -> None:
    samples = [
        "Authorization: Bearer abcdefghijklmnopqrstuvwxyz",
        "token=abcdefghijklmnop",
        "https://user:password@example.test/path",
        "https://example.test/?access_token=abcdefghijklmnop&safe=yes",
        "ghp_abcdefghijklmnopqrstuvwxyz123456",
        "github_pat_abcdefghijklmnopqrstuvwxyz_123456",
        "sk-abcdefghijklmnopqrstuvwxyz123456",
        "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnopqrstuvwxyz",
        "-----BEGIN PRIVATE KEY-----\nsecret material\n-----END PRIVATE KEY-----",
    ]
    for sample in samples:
        redacted = redact_text(sample)
        assert "abcdefghijklmnopqrstuvwxyz" not in redacted
        assert REDACTED in redacted


def test_run_record_is_redacted_before_disk(tmp_path) -> None:
    run_id = new_run_id(datetime(2026, 9, 10, tzinfo=UTC), "deadbeef")
    record = RunRecord.new(
        run_id=run_id,
        command="deploy",
        controller="normandy",
        desired_manifest_commit="a" * 40,
        components=["t3-steward"],
        hosts=["laptop"],
    )
    record = replace(
        record,
        details={
            "Authorization": "Bearer should-not-survive",
            "message": "token=also-secret",
            "safe": "preserved",
        },
    )
    target = RunRecordStore(tmp_path / "runs").write(record)
    text = target.read_text(encoding="utf-8")
    parsed = json.loads(text)
    assert "should-not-survive" not in text
    assert "also-secret" not in text
    assert parsed["details"]["safe"] == "preserved"
    assert target.stat().st_mode & 0o777 == 0o600
