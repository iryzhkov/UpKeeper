"""Frozen inert owning inputs; these checks do not qualify a production helper.

Helper protocol, physical guards, retained generation, fresh token counts, genuine Go
consumer/store faults and installed accessibility remain FUTURE_REQUIRED.
"""

import hashlib
import json
import re
import stat
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
OWNING = ROOT / "shared/catalog-guidance"
TOOLS = ("fixture-cli", "fixture-mcp", "fixture-third-party", "huyang")
POLICY_DIGEST = "eb4007baaf0592dc7c103c759793c1313b8a6d6650668cf733ed9cbe7633e8aa"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def load(relative):
    path = OWNING / relative
    data = path.read_bytes()
    value = json.loads(data)
    assert data == (json.dumps(value, sort_keys=True, indent=2, ensure_ascii=True) + "\n").encode()
    assert stat.S_IMODE(path.stat().st_mode) == 0o644
    assert path.stat().st_nlink == 1
    assert not path.is_symlink()
    return value, data


def test_manifest_crosspins_and_closed_four_tool_inventory():
    manifest, data = load("guidance/manifest.json")
    assert set(manifest) == {
        "schema", "schema_version", "catalog_sha256", "producer_receipt_sha256",
        "policy_sha256", "required_global_policy", "records",
    }
    assert manifest["schema"] == "tool-guidance-manifest/v1"
    assert type(manifest["schema_version"]) is int
    assert manifest["schema_version"] == 1
    assert manifest["policy_sha256"] == POLICY_DIGEST
    assert tuple(record["tool_id"] for record in manifest["records"]) == TOOLS
    for record in manifest["records"]:
        assert set(record) == {"tool_id", "path", "sha256"}
        assert record["path"] == "guidance/" + record["tool_id"] + ".json"
        _, package = load(record["path"])
        assert digest(package) == record["sha256"]
    policy = manifest["required_global_policy"]
    assert set(policy) == {"path", "sha256"}
    assert policy["path"] == "guidance/required-global-policy.json"
    _, policy_bytes = load(policy["path"])
    assert digest(policy_bytes) == policy["sha256"]
    layout, _ = load("templates/discovery.txt")
    assert layout["bindings"]["guidance_manifest_sha256"] == digest(data)
    assert layout["bindings"]["required_global_policy_sha256"] == digest(policy_bytes)


@pytest.mark.parametrize("tool_id", TOOLS)
def test_owned_rules_help_and_authentic_source_evidence(tool_id):
    package, _ = load("guidance/" + tool_id + ".json")
    assert set(package) == {
        "schema", "schema_version", "tool_id", "owner", "purpose", "rules",
        "help", "capability_ids", "workflow",
    }
    assert package["schema"] == "tool-guidance/v1"
    assert type(package["schema_version"]) is int and package["schema_version"] == 1
    assert package["tool_id"] == tool_id
    owner = package["owner"]
    assert set(owner) == {
        "kind", "owning_path", "evidence_path", "evidence_sha256",
        "source_path", "source_sha256", "status",
    }
    assert owner["owning_path"] == "shared/catalog-guidance/guidance/" + tool_id + ".json"
    assert digest((ROOT / owner["source_path"]).read_bytes()) == owner["source_sha256"]
    assert owner["status"] == "hypothetical-new-owner-input-unapproved"
    ids = []
    for rule in package["rules"]:
        assert set(rule) == {"id", "text", "placement", "evidence_quote_sha256"}
        assert re.fullmatch(re.escape(tool_id) + r"\.[a-z-]+\.v1", rule["id"])
        assert rule["placement"] == "before-tool-choice"
        assert rule["text"] and "\x00" not in rule["text"]
        ids.append(rule["id"])
    assert len(ids) == len(set(ids))
    help_descriptor = package["help"]
    if tool_id == "huyang":
        assert set(help_descriptor) == {"kind", "path", "sha256", "display_path"}
        assert help_descriptor["kind"] == "pinned-reference"
        assert help_descriptor["path"] == ".config/agents/huyang.md"
        assert help_descriptor["display_path"] == "~/.config/agents/huyang.md"
    else:
        assert set(help_descriptor) == {"kind", "path", "sha256", "tool_id"}
        assert help_descriptor["kind"] == "catalog-record"
        assert help_descriptor["path"] == "inputs/tool-catalog.json"
        assert help_descriptor["tool_id"] == tool_id
        manifest, _ = load("guidance/manifest.json")
        assert help_descriptor["sha256"] == manifest["catalog_sha256"]
        descriptor = json.loads((ROOT / owner["source_path"]).read_bytes())
        assert package["capability_ids"] == descriptor["tool"]["capabilities"]


def test_layout_is_only_the_reviewed_finite_assembly():
    layout, _ = load("templates/discovery.txt")
    assert set(layout) == {"schema_version", "bindings", "texts"}
    assert type(layout["schema_version"]) is int and layout["schema_version"] == 4
    assert layout["texts"] == {
        "shared": "{{GLOBAL_SHARED}}{{FRAGMENTS}}{{MODEL_CODEX}}\n\n{{DISCOVERY}}",
        "claude": "{{GLOBAL_CLAUDE}}{{FRAGMENTS}}{{MODEL_CLAUDE}}\n\n{{DISCOVERY}}",
        "codex": "<!-- fleet:start -->\n{{SHARED_MAIN}}<!-- fleet:end -->\n",
        "discovery": "## Candidate tool discovery\n\n{{MENU_ROWS}}\n"
        "Catalog SHA256: {{CATALOG_SHA256}}\n"
        "Producer receipt SHA256: {{PRODUCER_RECEIPT_SHA256}}\n",
    }
    bindings = layout["bindings"]
    assert bindings["policy_sha256"] == POLICY_DIGEST
    assert digest((ROOT / "shared/agent-instructions/owner.json").read_bytes()) == (
        bindings["imported_owner_sha256"]
    )
    for path, pin in bindings["imported_source_sha256"].items():
        assert digest((ROOT / "shared/agent-instructions" / path).read_bytes()) == pin


def test_policy_retains_required_owner_blocker_and_reference_custody():
    policy, _ = load("guidance/required-global-policy.json")
    assert set(policy) == {
        "schema", "schema_version", "provenance", "texts",
        "operational_reference", "deferred_owner_seam",
    }
    assert policy["schema"] == "required-global-policy/v1"
    assert type(policy["schema_version"]) is int and policy["schema_version"] == 1
    seam = policy["deferred_owner_seam"]
    assert seam["schema"] == "required-owner-handoff/v1"
    assert seam["tool_id"] == "t3-steward"
    assert seam["catalog_membership"] == "absent"
    assert seam["status"] == "blocked-owner-handoff"
    for text in policy["texts"].values():
        for quote in seam["resident_quotes"]:
            assert quote in text
        assert "Quarantine release is authenticated marker-only cleanup" in text
        assert "cannot retry intake" in text
        assert "immutable rollback bundles" in text
    layout, _ = load("templates/discovery.txt")
    reference_pin = layout["bindings"]["operational_reference_sha256"]
    assert digest(policy["operational_reference"].encode()) == reference_pin
    assert policy["provenance"]["reference_sha256"] == reference_pin
    for text in layout["texts"].values():
        assert "operational-guidance.md" not in text
        assert policy["operational_reference"] not in text


def test_owner_is_separately_versioned_without_mutating_imported_owner():
    owner, _ = load("owner.json")
    assert len(owner) == 16 and owner["schema_version"] == 5
    assert owner["state"] == "candidate-inert"
    assert owner["import_commit"] == "b32c0b6f48e12b7d74a60f8a010f84bab73d299f"
    assert owner["import_tree"] == "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52"
    assert owner["overlay_source_files"] == [
        "config/bin/agents-tool-project", "templates/discovery.txt", "guidance/manifest.json",
        "guidance/required-global-policy.json", "guidance/fixture-cli.json",
        "guidance/fixture-mcp.json", "guidance/fixture-third-party.json", "guidance/huyang.json",
    ]
    assert owner["evidence_source_files"] == [
        "internal/toolproducer/testdata/descriptors/" + tool + ".json" for tool in TOOLS[:3]
    ]
    original = json.loads((ROOT / "shared/agent-instructions/owner.json").read_bytes())
    assert original["schema_version"] != owner["schema_version"]
