package author

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// Fixtures retain the frozen archive owner and dual projection bytes; request,
// summary and receipt envelopes use the admitted protocol inventories and pins.
// They are inert examples, not real source acquisition or preparation receipts.
// inputs.sha256 verified drafts/guidance/source-plan/wire-examples at execution.
// No fixture is generated from the production schemas or production validator.
var catalogTestFixtures = map[CatalogDocumentKind]string{
	"owner5": `{
  "catalog_input_contract": "offline-tool-producer-receipt-v1",
  "evidence_source_files": [
    "internal/toolproducer/testdata/descriptors/fixture-cli.json",
    "internal/toolproducer/testdata/descriptors/fixture-mcp.json",
    "internal/toolproducer/testdata/descriptors/fixture-third-party.json"
  ],
  "future_merges": [
    ".claude/CLAUDE.md:omarchy-setup import fence",
    ".config/opencode/opencode.json:instructions"
  ],
  "import_commit": "b32c0b6f48e12b7d74a60f8a010f84bab73d299f",
  "import_repository": "github.com/iryzhkov/omarchy-setup",
  "import_source_files": [
    "config/bin/agents-instructions-gen",
    "config/bin/agents-role-project",
    "config/claude/CLAUDE.md",
    "config/claude/skills/huyang/SKILL.md",
    "config/claude/skills/implementation-session/SKILL.md",
    "config/claude/skills/implementation-session/references/prompt-template.md",
    "config/claude/skills/ov-memory-curation/SKILL.md",
    "config/claude/skills/ov-memory/SKILL.md",
    "config/claude/skills/publish-agent-tooling/SKILL.md",
    "config/claude/skills/t3-campaign/SKILL.md",
    "config/claude/skills/t3-campaign/references/executor-briefs.md",
    "config/claude/skills/t3-task/SKILL.md",
    "config/claude/skills/t3-wait/SKILL.md"
  ],
  "import_tree": "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52",
  "instruction_files": [
    ".claude/agents/critical-reviewer.md",
    ".claude/agents/executor.md",
    ".claude/agents/planner.md",
    ".claude/agents/reader.md",
    ".claude/agents/reviewer.md",
    ".claude/omarchy-setup/CLAUDE.md",
    ".codex/AGENTS.md",
    ".codex/critical-reviewer.config.toml",
    ".codex/executor.config.toml",
    ".codex/planner.config.toml",
    ".codex/reader.config.toml",
    ".codex/reviewer.config.toml",
    ".config/agents/AGENTS.md",
    ".config/agents/huyang.md",
    ".config/agents/jocasta.md",
    ".config/agents/operational-guidance.md",
    ".config/agents/ov-memory.md",
    ".config/agents/t3-campaign-executor-briefs.md",
    ".config/agents/t3-steward.md"
  ],
  "overlay_source_files": [
    "config/bin/agents-tool-project",
    "templates/discovery.txt",
    "guidance/manifest.json",
    "guidance/required-global-policy.json",
    "guidance/fixture-cli.json",
    "guidance/fixture-mcp.json",
    "guidance/fixture-third-party.json",
    "guidance/huyang.json"
  ],
  "owner": "upkeeper",
  "policy_input_contract": "canonical-release-manifest-v1",
  "retired_reference": ".config/agents/agent99.md",
  "schema_version": 5,
  "skill_roots": [
    ".agents/skills",
    ".claude/skills"
  ],
  "source_repository": "github.com/iryzhkov/UpKeeper",
  "state": "candidate-inert"
}
`,
	"request4": `{
  "catalog_path": "/scratch/d1/input/catalog.json",
  "catalog_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "guidance_manifest_path": "/scratch/d1/input/guidance_manifest.json",
  "guidance_manifest_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "imported_files": [
    {
      "mode": 420,
      "path": ".claude/agents/critical-reviewer.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".claude/agents/executor.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".claude/agents/planner.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".claude/agents/reader.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".claude/agents/reviewer.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".claude/omarchy-setup/CLAUDE.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".codex/AGENTS.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".codex/critical-reviewer.config.toml",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".codex/executor.config.toml",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".codex/planner.config.toml",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".codex/reader.config.toml",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".codex/reviewer.config.toml",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".config/agents/AGENTS.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".config/agents/huyang.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".config/agents/jocasta.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".config/agents/ov-memory.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".config/agents/t3-campaign-executor-briefs.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    },
    {
      "mode": 420,
      "path": ".config/agents/t3-steward.md",
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    }
  ],
  "imported_output_root": "/scratch/d1/imported",
  "observation_path": "/scratch/d1/input/observation.json",
  "observation_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "platform": "linux-amd64",
  "policy_input_path": "/scratch/d1/input/policy_input.json",
  "policy_input_sha256": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
  "producer_input_path": "/scratch/d1/input/producer_input.json",
  "producer_input_sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
  "producer_receipt_path": "/scratch/d1/input/producer_receipt.json",
  "producer_receipt_sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "profile_path": "/scratch/d1/input/profile.json",
  "profile_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "schema_version": 4,
  "state": "candidate-inert",
  "template_path": "/scratch/d1/input/template.json",
  "template_sha256": "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
  "tokenizer_manifest_path": "/scratch/d1/input/tokenizer_manifest.json",
  "tokenizer_manifest_sha256": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
  "tokenizer_root": "/scratch/d1/tokenizer"
}
`,
	"summary4": `{
  "catalog_sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a",
  "discovery_tokens": 228,
  "files": [
    {
      "mode": 420,
      "path": ".claude/agents/critical-reviewer.md",
      "sha256": "6dbef9e57cd905eb11704515891c5cb9e15bb4e153aabef812c4975805020758"
    },
    {
      "mode": 420,
      "path": ".claude/agents/executor.md",
      "sha256": "4afdf4c1cc0cd2b4be386ed0532cb3eaa43a8ac3a76e8e48589d40e301e7d366"
    },
    {
      "mode": 420,
      "path": ".claude/agents/planner.md",
      "sha256": "58e47dae31e82c260a8ea3b4f10c1c522c449bdad81538dadd4c3ab3f71187e3"
    },
    {
      "mode": 420,
      "path": ".claude/agents/reader.md",
      "sha256": "13354f6d60ad4b307cfaa885923b16a29b8944b2a4510d641f8bf3d9901c3992"
    },
    {
      "mode": 420,
      "path": ".claude/agents/reviewer.md",
      "sha256": "9613123a697cf4521f27498dcfc51d31cbe3cf5db26caada0452357e73ac4ea3"
    },
    {
      "mode": 420,
      "path": ".claude/omarchy-setup/CLAUDE.md",
      "sha256": "ab34ddd1a24c706959593fecc2319101c6512a31fcfdff1af0ae768f629bd7d0"
    },
    {
      "mode": 420,
      "path": ".codex/AGENTS.md",
      "sha256": "d9db90a3f19bb15fa915daeda850a0d381a8a2ec5b5b0f109e9fefad1ba208f1"
    },
    {
      "mode": 420,
      "path": ".codex/critical-reviewer.config.toml",
      "sha256": "44f75e02abc4e168453bc913c56b5d4b7d5f8a7b097510e7fc5dae2a0b0d25ae"
    },
    {
      "mode": 420,
      "path": ".codex/executor.config.toml",
      "sha256": "e9559ddfad5dfd37c56ef116778348ee3a7985c6e349583df2aaef8aaec25665"
    },
    {
      "mode": 420,
      "path": ".codex/planner.config.toml",
      "sha256": "072818376f0d476ae38a24199a7863bb549a3db82fd6a2e28e59d0bfd720a343"
    },
    {
      "mode": 420,
      "path": ".codex/reader.config.toml",
      "sha256": "d6b4a41a5de7ca9708321ceb7596420b6b858b5fa20694a2bd3643c2be06e0b3"
    },
    {
      "mode": 420,
      "path": ".codex/reviewer.config.toml",
      "sha256": "7725eeb075952ddf9df017e7af68c267fe2688c91af562d81061bffaa977e076"
    },
    {
      "mode": 420,
      "path": ".config/agents/AGENTS.md",
      "sha256": "1cd46822fb19c6b2922444f981bc565695de015b7e9d4a43e989a2cc56fefd04"
    },
    {
      "mode": 420,
      "path": ".config/agents/huyang.md",
      "sha256": "d325c443031af38500a36f89d964e0f6f240e5acc4f2dabe6e0b011f5dfce8e4"
    },
    {
      "mode": 420,
      "path": ".config/agents/jocasta.md",
      "sha256": "423e462fb26bc75972d13717f53bd21e29425f5cb3a9f6d37f6080e95216f6e6"
    },
    {
      "mode": 420,
      "path": ".config/agents/operational-guidance.md",
      "sha256": "6c0d6ae65c7aebda0d4785115841e7cf2e65e3ddd2a71485b71cce0e488fa2d4"
    },
    {
      "mode": 420,
      "path": ".config/agents/ov-memory.md",
      "sha256": "23bba5308a40c68f6107a72be1b0bb34113570439c9d09b134bdffc2356ea6c1"
    },
    {
      "mode": 420,
      "path": ".config/agents/t3-campaign-executor-briefs.md",
      "sha256": "fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8"
    },
    {
      "mode": 420,
      "path": ".config/agents/t3-steward.md",
      "sha256": "31f405e47c776324044cc2d7dbfa5efb044ae293d81a48967312dcc06d925ff5"
    },
    {
      "mode": 420,
      "path": "metadata/guidance/fragments/fixture-cli.md",
      "sha256": "c793c721c60c90240770a69d33be77a7c06dd6868cea1cff55c77aeed9307413"
    },
    {
      "mode": 420,
      "path": "metadata/guidance/fragments/fixture-mcp.md",
      "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    },
    {
      "mode": 420,
      "path": "metadata/guidance/fragments/fixture-third-party.md",
      "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    },
    {
      "mode": 420,
      "path": "metadata/guidance/fragments/huyang.md",
      "sha256": "cb4dcd48812d76b0298e864a3bb0b9a50e0add6afecbdf132e113900581e2264"
    },
    {
      "mode": 420,
      "path": "metadata/guidance/menu.json",
      "sha256": "b77bbc5eb86ee6bb9422f7fa1251af1ab9f4555a75f6e327ff910b49d3804691"
    },
    {
      "mode": 420,
      "path": "metadata/guidance/menu.txt",
      "sha256": "4a6ebecb761b0113f90cc55822d176fdeee1c030e96eb2f6f2dc8513fcdbe795"
    }
  ],
  "guidance": {
    "catalog_sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a",
    "decisions": [
      {
        "decision": "required-interface-blocked",
        "fragment_sha256": "c793c721c60c90240770a69d33be77a7c06dd6868cea1cff55c77aeed9307413",
        "guidance_sha256": "42c026616fbaa9111ca91fdcb229ef11aacd1d85294eb0de0f75e822936db202",
        "help_sha256": "c096b0fe350f94e9ba0164a0eb2fc2fe38c983f091c8a459104ac174e2f6cfaa",
        "rule_ids": [],
        "source_owner": {
          "evidence_path": "inputs/tool-catalog.json",
          "evidence_sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a",
          "kind": "catalog-fixture",
          "owning_path": "shared/catalog-guidance/guidance/fixture-cli.json",
          "source_path": "internal/toolproducer/testdata/descriptors/fixture-cli.json",
          "source_sha256": "a0e6c6a8f4f1643c8a039ff97c0ff7a854edaba182bf9cdbfd7c52f3d279d74e",
          "status": "hypothetical-new-owner-input-unapproved"
        },
        "tool_id": "fixture-cli"
      },
      {
        "decision": "declaration-only",
        "fragment_sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
        "guidance_sha256": "c77844249d667114540692312878cb9704e17767884953aaffd0d31461958674",
        "help_sha256": "c522fb9bf2367c685d7ae0e284242a9ba6ba29787296c0750beeb224ced12655",
        "rule_ids": [],
        "source_owner": {
          "evidence_path": "inputs/tool-catalog.json",
          "evidence_sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a",
          "kind": "catalog-fixture",
          "owning_path": "shared/catalog-guidance/guidance/fixture-mcp.json",
          "source_path": "internal/toolproducer/testdata/descriptors/fixture-mcp.json",
          "source_sha256": "5d061d829dab6e36d6c1899e2d6f2780fb129ae25efc28fe688d59cc41a6ef5b",
          "status": "hypothetical-new-owner-input-unapproved"
        },
        "tool_id": "fixture-mcp"
      },
      {
        "decision": "declaration-only",
        "fragment_sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
        "guidance_sha256": "b159186d27cfd929e772b6ecb464d8dd80ce4f6a01da95477606a4c7e65c1f01",
        "help_sha256": "a434c306f04b60bb7af422a77136095b8a1455c7a15d789b7562bef8959dd160",
        "rule_ids": [],
        "source_owner": {
          "evidence_path": "inputs/tool-catalog.json",
          "evidence_sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a",
          "kind": "catalog-fixture",
          "owning_path": "shared/catalog-guidance/guidance/fixture-third-party.json",
          "source_path": "internal/toolproducer/testdata/descriptors/fixture-third-party.json",
          "source_sha256": "e0923229f54efe7396e9eb396c8f51179c56b94a9756b0316fb767411ae7950e",
          "status": "hypothetical-new-owner-input-unapproved"
        },
        "tool_id": "fixture-third-party"
      },
      {
        "decision": "visible-observed",
        "fragment_sha256": "cb4dcd48812d76b0298e864a3bb0b9a50e0add6afecbdf132e113900581e2264",
        "guidance_sha256": "b41eeba148797d10d45b7bc727513fbc9c062eae69db1487df5aa53c408c80be",
        "help_sha256": "d325c443031af38500a36f89d964e0f6f240e5acc4f2dabe6e0b011f5dfce8e4",
        "rule_ids": [
          "huyang.repository-boundary.v1"
        ],
        "source_owner": {
          "evidence_path": "baseline/.config/agents/AGENTS.md",
          "evidence_sha256": "4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9",
          "kind": "imported-guidance-proposal",
          "owning_path": "shared/catalog-guidance/guidance/huyang.json",
          "source_path": "shared/agent-instructions/config/claude/skills/huyang/SKILL.md",
          "source_sha256": "3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f",
          "status": "hypothetical-new-owner-input-unapproved"
        },
        "tool_id": "huyang"
      }
    ],
    "files": [
      {
        "bytes": 916,
        "mode": 420,
        "path": "discovery.txt",
        "sha256": "ca656e319d39dc90d0f20b9da5ed6262c1813a0ef6ac556af46d2c58291f1081"
      },
      {
        "bytes": 8865,
        "mode": 420,
        "path": "final-instructions/.claude/omarchy-setup/CLAUDE.md",
        "sha256": "ab34ddd1a24c706959593fecc2319101c6512a31fcfdff1af0ae768f629bd7d0"
      },
      {
        "bytes": 7876,
        "mode": 420,
        "path": "final-instructions/.codex/AGENTS.md",
        "sha256": "d9db90a3f19bb15fa915daeda850a0d381a8a2ec5b5b0f109e9fefad1ba208f1"
      },
      {
        "bytes": 7836,
        "mode": 420,
        "path": "final-instructions/.config/agents/AGENTS.md",
        "sha256": "1cd46822fb19c6b2922444f981bc565695de015b7e9d4a43e989a2cc56fefd04"
      },
      {
        "bytes": 25969,
        "mode": 420,
        "path": "final-instructions/.config/agents/operational-guidance.md",
        "sha256": "6c0d6ae65c7aebda0d4785115841e7cf2e65e3ddd2a71485b71cce0e488fa2d4"
      },
      {
        "bytes": 102,
        "mode": 420,
        "path": "fragments/fixture-cli.md",
        "sha256": "c793c721c60c90240770a69d33be77a7c06dd6868cea1cff55c77aeed9307413"
      },
      {
        "bytes": 0,
        "mode": 420,
        "path": "fragments/fixture-mcp.md",
        "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
      },
      {
        "bytes": 0,
        "mode": 420,
        "path": "fragments/fixture-third-party.md",
        "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
      },
      {
        "bytes": 598,
        "mode": 420,
        "path": "fragments/huyang.md",
        "sha256": "cb4dcd48812d76b0298e864a3bb0b9a50e0add6afecbdf132e113900581e2264"
      },
      {
        "bytes": 21470,
        "mode": 420,
        "path": "menu.json",
        "sha256": "b77bbc5eb86ee6bb9422f7fa1251af1ab9f4555a75f6e327ff910b49d3804691"
      },
      {
        "bytes": 14128,
        "mode": 420,
        "path": "menu.txt",
        "sha256": "4a6ebecb761b0113f90cc55822d176fdeee1c030e96eb2f6f2dc8513fcdbe795"
      }
    ],
    "guidance_manifest_sha256": "ae511a13e2cb9e8b9967824a54f2c88bcb1799a6aa83bc760821bf7f91186e9a",
    "layout_sha256": "3fc46a2002e917bc1c8a1850769fc093a8b7703296955a61e67ca98ba7594334",
    "owner_handoff": "blocked-owner-handoff",
    "policy_sha256": "eb4007baaf0592dc7c103c759793c1313b8a6d6650668cf733ed9cbe7633e8aa",
    "producer_receipt_sha256": "74c8dba65aa9c1de49dba4ded25569a15b060c6a897f8f091df3dbab02bfc51f",
    "profile_sha256": "a74a5cbce1d4eb4fc89f2caf53297d5e3e088f055a0b2b9e30230e681a61061e",
    "required_global_policy_sha256": "fd02a30aacfbd1f6dda95f429d6e970385e6bd0ca7dce722e3d3da87acefc3f5",
    "schema": "dual-guidance-summary/v1",
    "schema_version": 1,
    "status": "required-interface-blocked"
  },
  "modified_paths": [
    ".claude/omarchy-setup/CLAUDE.md",
    ".codex/AGENTS.md",
    ".config/agents/AGENTS.md"
  ],
  "policy_sha256": "eb4007baaf0592dc7c103c759793c1313b8a6d6650668cf733ed9cbe7633e8aa",
  "producer_receipt_sha256": "74c8dba65aa9c1de49dba4ded25569a15b060c6a897f8f091df3dbab02bfc51f",
  "request_sha256": "a39ee746cca450399a3e3eafb14c752eb5d19ffb1b16cb03de87c10dfb671567",
  "resident_tokens": 2815,
  "schema_version": 4,
  "state": "candidate-inert",
  "template_sha256": "3fc46a2002e917bc1c8a1850769fc093a8b7703296955a61e67ca98ba7594334",
  "tokenizer": "tiktoken-0.12.0-cl100k_base",
  "tokenizer_manifest_sha256": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
}
`,
	"receipt5": `{
  "catalog": {
    "artifact_manifest_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "authorized": "unknown",
    "catalog_sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a",
    "configured": "unknown",
    "derivation": "upkeeper-tool-overlay-v4",
    "discovery_tokens": 1200,
    "enforcement": "unqualified",
    "input_sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    "installed": "unknown",
    "platform": "linux-amd64",
    "producer_receipt_sha256": "74c8dba65aa9c1de49dba4ded25569a15b060c6a897f8f091df3dbab02bfc51f",
    "readiness": "unassessed",
    "resident_tokens": 3000,
    "tokenizer": "tiktoken-0.12.0-cl100k_base",
    "tokenizer_manifest_sha256": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
  },
  "files": [
    {
      "kind": "instruction",
      "logical_destination": ".claude/agents/critical-reviewer.md",
      "mode": 420,
      "path": ".claude/agents/critical-reviewer.md",
      "sha256": "6dbef9e57cd905eb11704515891c5cb9e15bb4e153aabef812c4975805020758"
    },
    {
      "kind": "instruction",
      "logical_destination": ".claude/agents/executor.md",
      "mode": 420,
      "path": ".claude/agents/executor.md",
      "sha256": "4afdf4c1cc0cd2b4be386ed0532cb3eaa43a8ac3a76e8e48589d40e301e7d366"
    },
    {
      "kind": "instruction",
      "logical_destination": ".claude/agents/planner.md",
      "mode": 420,
      "path": ".claude/agents/planner.md",
      "sha256": "58e47dae31e82c260a8ea3b4f10c1c522c449bdad81538dadd4c3ab3f71187e3"
    },
    {
      "kind": "instruction",
      "logical_destination": ".claude/agents/reader.md",
      "mode": 420,
      "path": ".claude/agents/reader.md",
      "sha256": "13354f6d60ad4b307cfaa885923b16a29b8944b2a4510d641f8bf3d9901c3992"
    },
    {
      "kind": "instruction",
      "logical_destination": ".claude/agents/reviewer.md",
      "mode": 420,
      "path": ".claude/agents/reviewer.md",
      "sha256": "9613123a697cf4521f27498dcfc51d31cbe3cf5db26caada0452357e73ac4ea3"
    },
    {
      "kind": "instruction",
      "logical_destination": ".claude/omarchy-setup/CLAUDE.md",
      "mode": 420,
      "path": ".claude/omarchy-setup/CLAUDE.md",
      "sha256": "ab34ddd1a24c706959593fecc2319101c6512a31fcfdff1af0ae768f629bd7d0"
    },
    {
      "kind": "instruction",
      "logical_destination": ".codex/AGENTS.md",
      "mode": 420,
      "path": ".codex/AGENTS.md",
      "sha256": "d9db90a3f19bb15fa915daeda850a0d381a8a2ec5b5b0f109e9fefad1ba208f1"
    },
    {
      "kind": "instruction",
      "logical_destination": ".codex/critical-reviewer.config.toml",
      "mode": 420,
      "path": ".codex/critical-reviewer.config.toml",
      "sha256": "44f75e02abc4e168453bc913c56b5d4b7d5f8a7b097510e7fc5dae2a0b0d25ae"
    },
    {
      "kind": "instruction",
      "logical_destination": ".codex/executor.config.toml",
      "mode": 420,
      "path": ".codex/executor.config.toml",
      "sha256": "e9559ddfad5dfd37c56ef116778348ee3a7985c6e349583df2aaef8aaec25665"
    },
    {
      "kind": "instruction",
      "logical_destination": ".codex/planner.config.toml",
      "mode": 420,
      "path": ".codex/planner.config.toml",
      "sha256": "072818376f0d476ae38a24199a7863bb549a3db82fd6a2e28e59d0bfd720a343"
    },
    {
      "kind": "instruction",
      "logical_destination": ".codex/reader.config.toml",
      "mode": 420,
      "path": ".codex/reader.config.toml",
      "sha256": "d6b4a41a5de7ca9708321ceb7596420b6b858b5fa20694a2bd3643c2be06e0b3"
    },
    {
      "kind": "instruction",
      "logical_destination": ".codex/reviewer.config.toml",
      "mode": 420,
      "path": ".codex/reviewer.config.toml",
      "sha256": "7725eeb075952ddf9df017e7af68c267fe2688c91af562d81061bffaa977e076"
    },
    {
      "kind": "instruction",
      "logical_destination": ".config/agents/AGENTS.md",
      "mode": 420,
      "path": ".config/agents/AGENTS.md",
      "sha256": "1cd46822fb19c6b2922444f981bc565695de015b7e9d4a43e989a2cc56fefd04"
    },
    {
      "kind": "instruction",
      "logical_destination": ".config/agents/huyang.md",
      "mode": 420,
      "path": ".config/agents/huyang.md",
      "sha256": "d325c443031af38500a36f89d964e0f6f240e5acc4f2dabe6e0b011f5dfce8e4"
    },
    {
      "kind": "instruction",
      "logical_destination": ".config/agents/jocasta.md",
      "mode": 420,
      "path": ".config/agents/jocasta.md",
      "sha256": "423e462fb26bc75972d13717f53bd21e29425f5cb3a9f6d37f6080e95216f6e6"
    },
    {
      "kind": "instruction",
      "logical_destination": ".config/agents/operational-guidance.md",
      "mode": 420,
      "path": ".config/agents/operational-guidance.md",
      "sha256": "6c0d6ae65c7aebda0d4785115841e7cf2e65e3ddd2a71485b71cce0e488fa2d4"
    },
    {
      "kind": "instruction",
      "logical_destination": ".config/agents/ov-memory.md",
      "mode": 420,
      "path": ".config/agents/ov-memory.md",
      "sha256": "23bba5308a40c68f6107a72be1b0bb34113570439c9d09b134bdffc2356ea6c1"
    },
    {
      "kind": "instruction",
      "logical_destination": ".config/agents/t3-campaign-executor-briefs.md",
      "mode": 420,
      "path": ".config/agents/t3-campaign-executor-briefs.md",
      "sha256": "fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8"
    },
    {
      "kind": "instruction",
      "logical_destination": ".config/agents/t3-steward.md",
      "mode": 420,
      "path": ".config/agents/t3-steward.md",
      "sha256": "31f405e47c776324044cc2d7dbfa5efb044ae293d81a48967312dcc06d925ff5"
    },
    {
      "kind": "guidance-input",
      "logical_destination": "inputs/guidance-layout.json",
      "mode": 384,
      "path": "inputs/guidance-layout.json",
      "sha256": "3fc46a2002e917bc1c8a1850769fc093a8b7703296955a61e67ca98ba7594334"
    },
    {
      "kind": "guidance-input",
      "logical_destination": "inputs/guidance-observation.json",
      "mode": 384,
      "path": "inputs/guidance-observation.json",
      "sha256": "c7bedf1238d04cbfdb278f92ac3cb5e101c7fb928693e50ba7c53711243a5801"
    },
    {
      "kind": "guidance-input",
      "logical_destination": "inputs/guidance-profile.json",
      "mode": 384,
      "path": "inputs/guidance-profile.json",
      "sha256": "a74a5cbce1d4eb4fc89f2caf53297d5e3e088f055a0b2b9e30230e681a61061e"
    },
    {
      "kind": "guidance-input",
      "logical_destination": "inputs/required-global-policy.json",
      "mode": 384,
      "path": "inputs/required-global-policy.json",
      "sha256": "fd02a30aacfbd1f6dda95f429d6e970385e6bd0ca7dce722e3d3da87acefc3f5"
    },
    {
      "kind": "policy-input",
      "logical_destination": "inputs/route-policy.json",
      "mode": 384,
      "path": "inputs/route-policy.json",
      "sha256": "eb4007baaf0592dc7c103c759793c1313b8a6d6650668cf733ed9cbe7633e8aa"
    },
    {
      "kind": "catalog-input",
      "logical_destination": "inputs/tool-catalog.json",
      "mode": 384,
      "path": "inputs/tool-catalog.json",
      "sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a"
    },
    {
      "kind": "guidance-input",
      "logical_destination": "inputs/tool-guidance-manifest.json",
      "mode": 384,
      "path": "inputs/tool-guidance-manifest.json",
      "sha256": "ae511a13e2cb9e8b9967824a54f2c88bcb1799a6aa83bc760821bf7f91186e9a"
    },
    {
      "kind": "guidance-input",
      "logical_destination": "inputs/tool-guidance/fixture-cli.json",
      "mode": 384,
      "path": "inputs/tool-guidance/fixture-cli.json",
      "sha256": "42c026616fbaa9111ca91fdcb229ef11aacd1d85294eb0de0f75e822936db202"
    },
    {
      "kind": "guidance-input",
      "logical_destination": "inputs/tool-guidance/fixture-mcp.json",
      "mode": 384,
      "path": "inputs/tool-guidance/fixture-mcp.json",
      "sha256": "c77844249d667114540692312878cb9704e17767884953aaffd0d31461958674"
    },
    {
      "kind": "guidance-input",
      "logical_destination": "inputs/tool-guidance/fixture-third-party.json",
      "mode": 384,
      "path": "inputs/tool-guidance/fixture-third-party.json",
      "sha256": "b159186d27cfd929e772b6ecb464d8dd80ce4f6a01da95477606a4c7e65c1f01"
    },
    {
      "kind": "guidance-input",
      "logical_destination": "inputs/tool-guidance/huyang.json",
      "mode": 384,
      "path": "inputs/tool-guidance/huyang.json",
      "sha256": "b41eeba148797d10d45b7bc727513fbc9c062eae69db1487df5aa53c408c80be"
    },
    {
      "kind": "catalog-input",
      "logical_destination": "inputs/tool-producer-input.json",
      "mode": 384,
      "path": "inputs/tool-producer-input.json",
      "sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    },
    {
      "kind": "producer-receipt",
      "logical_destination": "inputs/tool-producer-receipt.json",
      "mode": 384,
      "path": "inputs/tool-producer-receipt.json",
      "sha256": "74c8dba65aa9c1de49dba4ded25569a15b060c6a897f8f091df3dbab02bfc51f"
    },
    {
      "kind": "guidance-metadata",
      "logical_destination": "metadata/guidance/fragments/fixture-cli.md",
      "mode": 420,
      "path": "metadata/guidance/fragments/fixture-cli.md",
      "sha256": "c793c721c60c90240770a69d33be77a7c06dd6868cea1cff55c77aeed9307413"
    },
    {
      "kind": "guidance-metadata",
      "logical_destination": "metadata/guidance/fragments/fixture-mcp.md",
      "mode": 420,
      "path": "metadata/guidance/fragments/fixture-mcp.md",
      "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    },
    {
      "kind": "guidance-metadata",
      "logical_destination": "metadata/guidance/fragments/fixture-third-party.md",
      "mode": 420,
      "path": "metadata/guidance/fragments/fixture-third-party.md",
      "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    },
    {
      "kind": "guidance-metadata",
      "logical_destination": "metadata/guidance/fragments/huyang.md",
      "mode": 420,
      "path": "metadata/guidance/fragments/huyang.md",
      "sha256": "cb4dcd48812d76b0298e864a3bb0b9a50e0add6afecbdf132e113900581e2264"
    },
    {
      "kind": "guidance-metadata",
      "logical_destination": "metadata/guidance/menu.json",
      "mode": 420,
      "path": "metadata/guidance/menu.json",
      "sha256": "b77bbc5eb86ee6bb9422f7fa1251af1ab9f4555a75f6e327ff910b49d3804691"
    },
    {
      "kind": "guidance-metadata",
      "logical_destination": "metadata/guidance/menu.txt",
      "mode": 420,
      "path": "metadata/guidance/menu.txt",
      "sha256": "4a6ebecb761b0113f90cc55822d176fdeee1c030e96eb2f6f2dc8513fcdbe795"
    },
    {
      "kind": "skill",
      "logical_destination": "skills/huyang/SKILL.md",
      "mode": 420,
      "path": "skills/huyang/SKILL.md",
      "sha256": "3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f"
    },
    {
      "kind": "skill",
      "logical_destination": "skills/implementation-session/SKILL.md",
      "mode": 420,
      "path": "skills/implementation-session/SKILL.md",
      "sha256": "37e202ab9bd16de8fd7f776966079c71b6f554bcaa3eb152099507fad0f4a567"
    },
    {
      "kind": "skill",
      "logical_destination": "skills/implementation-session/references/prompt-template.md",
      "mode": 420,
      "path": "skills/implementation-session/references/prompt-template.md",
      "sha256": "4c46d852f6737f9c62b85b34275109c8f586b64fd60ae342a58047d35245f595"
    },
    {
      "kind": "skill",
      "logical_destination": "skills/ov-memory-curation/SKILL.md",
      "mode": 420,
      "path": "skills/ov-memory-curation/SKILL.md",
      "sha256": "c9ae9a8555c7533d2cf24a9aaa235a165da35cf3215b194902363de31e53e82f"
    },
    {
      "kind": "skill",
      "logical_destination": "skills/ov-memory/SKILL.md",
      "mode": 420,
      "path": "skills/ov-memory/SKILL.md",
      "sha256": "e0f282759a22b73c35e45fac382d9121ea60782533411a58552ae4b0aec47453"
    },
    {
      "kind": "skill",
      "logical_destination": "skills/publish-agent-tooling/SKILL.md",
      "mode": 420,
      "path": "skills/publish-agent-tooling/SKILL.md",
      "sha256": "65a8b78f7ce01bf9b61ea0b89db395659ee0fdadde3111d2eb528d7b1c6292cf"
    },
    {
      "kind": "skill",
      "logical_destination": "skills/t3-campaign/SKILL.md",
      "mode": 420,
      "path": "skills/t3-campaign/SKILL.md",
      "sha256": "420fdd7e51cc689a1eea80db6444c2e8233f1fc657a7bc2d99ec2fa8ea25181d"
    },
    {
      "kind": "skill",
      "logical_destination": "skills/t3-campaign/references/executor-briefs.md",
      "mode": 420,
      "path": "skills/t3-campaign/references/executor-briefs.md",
      "sha256": "fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8"
    },
    {
      "kind": "skill",
      "logical_destination": "skills/t3-task/SKILL.md",
      "mode": 420,
      "path": "skills/t3-task/SKILL.md",
      "sha256": "b3af2689fb0815f4bcdf052991a9db2d75b2a20f1c2736fc0cfc71254c59539b"
    },
    {
      "kind": "skill",
      "logical_destination": "skills/t3-wait/SKILL.md",
      "mode": 420,
      "path": "skills/t3-wait/SKILL.md",
      "sha256": "4695ba110bc471feff890f0b4c0429be09ded9824c0c1d04b6fefe075765d955"
    }
  ],
  "guidance": {
    "global_policy_sha256": "fd02a30aacfbd1f6dda95f429d6e970385e6bd0ca7dce722e3d3da87acefc3f5",
    "layout_sha256": "3fc46a2002e917bc1c8a1850769fc093a8b7703296955a61e67ca98ba7594334",
    "manifest_sha256": "ae511a13e2cb9e8b9967824a54f2c88bcb1799a6aa83bc760821bf7f91186e9a",
    "observation_sha256": "c7bedf1238d04cbfdb278f92ac3cb5e101c7fb928693e50ba7c53711243a5801",
    "packages": [
      {
        "path": "guidance/fixture-cli.json",
        "sha256": "42c026616fbaa9111ca91fdcb229ef11aacd1d85294eb0de0f75e822936db202",
        "tool_id": "fixture-cli"
      },
      {
        "path": "guidance/fixture-mcp.json",
        "sha256": "c77844249d667114540692312878cb9704e17767884953aaffd0d31461958674",
        "tool_id": "fixture-mcp"
      },
      {
        "path": "guidance/fixture-third-party.json",
        "sha256": "b159186d27cfd929e772b6ecb464d8dd80ce4f6a01da95477606a4c7e65c1f01",
        "tool_id": "fixture-third-party"
      },
      {
        "path": "guidance/huyang.json",
        "sha256": "b41eeba148797d10d45b7bc727513fbc9c062eae69db1487df5aa53c408c80be",
        "tool_id": "huyang"
      }
    ],
    "profile_sha256": "a74a5cbce1d4eb4fc89f2caf53297d5e3e088f055a0b2b9e30230e681a61061e",
    "projection": {
      "catalog_sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a",
      "decisions": [
        {
          "decision": "required-interface-blocked",
          "fragment_sha256": "c793c721c60c90240770a69d33be77a7c06dd6868cea1cff55c77aeed9307413",
          "guidance_sha256": "42c026616fbaa9111ca91fdcb229ef11aacd1d85294eb0de0f75e822936db202",
          "help_sha256": "c096b0fe350f94e9ba0164a0eb2fc2fe38c983f091c8a459104ac174e2f6cfaa",
          "rule_ids": [],
          "source_owner": {
            "evidence_path": "inputs/tool-catalog.json",
            "evidence_sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a",
            "kind": "catalog-fixture",
            "owning_path": "shared/catalog-guidance/guidance/fixture-cli.json",
            "source_path": "internal/toolproducer/testdata/descriptors/fixture-cli.json",
            "source_sha256": "a0e6c6a8f4f1643c8a039ff97c0ff7a854edaba182bf9cdbfd7c52f3d279d74e",
            "status": "hypothetical-new-owner-input-unapproved"
          },
          "tool_id": "fixture-cli"
        },
        {
          "decision": "declaration-only",
          "fragment_sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
          "guidance_sha256": "c77844249d667114540692312878cb9704e17767884953aaffd0d31461958674",
          "help_sha256": "c522fb9bf2367c685d7ae0e284242a9ba6ba29787296c0750beeb224ced12655",
          "rule_ids": [],
          "source_owner": {
            "evidence_path": "inputs/tool-catalog.json",
            "evidence_sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a",
            "kind": "catalog-fixture",
            "owning_path": "shared/catalog-guidance/guidance/fixture-mcp.json",
            "source_path": "internal/toolproducer/testdata/descriptors/fixture-mcp.json",
            "source_sha256": "5d061d829dab6e36d6c1899e2d6f2780fb129ae25efc28fe688d59cc41a6ef5b",
            "status": "hypothetical-new-owner-input-unapproved"
          },
          "tool_id": "fixture-mcp"
        },
        {
          "decision": "declaration-only",
          "fragment_sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
          "guidance_sha256": "b159186d27cfd929e772b6ecb464d8dd80ce4f6a01da95477606a4c7e65c1f01",
          "help_sha256": "a434c306f04b60bb7af422a77136095b8a1455c7a15d789b7562bef8959dd160",
          "rule_ids": [],
          "source_owner": {
            "evidence_path": "inputs/tool-catalog.json",
            "evidence_sha256": "e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a",
            "kind": "catalog-fixture",
            "owning_path": "shared/catalog-guidance/guidance/fixture-third-party.json",
            "source_path": "internal/toolproducer/testdata/descriptors/fixture-third-party.json",
            "source_sha256": "e0923229f54efe7396e9eb396c8f51179c56b94a9756b0316fb767411ae7950e",
            "status": "hypothetical-new-owner-input-unapproved"
          },
          "tool_id": "fixture-third-party"
        },
        {
          "decision": "visible-observed",
          "fragment_sha256": "cb4dcd48812d76b0298e864a3bb0b9a50e0add6afecbdf132e113900581e2264",
          "guidance_sha256": "b41eeba148797d10d45b7bc727513fbc9c062eae69db1487df5aa53c408c80be",
          "help_sha256": "d325c443031af38500a36f89d964e0f6f240e5acc4f2dabe6e0b011f5dfce8e4",
          "rule_ids": [
            "huyang.repository-boundary.v1"
          ],
          "source_owner": {
            "evidence_path": "baseline/.config/agents/AGENTS.md",
            "evidence_sha256": "4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9",
            "kind": "imported-guidance-proposal",
            "owning_path": "shared/catalog-guidance/guidance/huyang.json",
            "source_path": "shared/agent-instructions/config/claude/skills/huyang/SKILL.md",
            "source_sha256": "3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f",
            "status": "hypothetical-new-owner-input-unapproved"
          },
          "tool_id": "huyang"
        }
      ],
      "files": [
        {
          "bytes": 916,
          "mode": 420,
          "path": "discovery.txt",
          "sha256": "ca656e319d39dc90d0f20b9da5ed6262c1813a0ef6ac556af46d2c58291f1081"
        },
        {
          "bytes": 8865,
          "mode": 420,
          "path": "final-instructions/.claude/omarchy-setup/CLAUDE.md",
          "sha256": "ab34ddd1a24c706959593fecc2319101c6512a31fcfdff1af0ae768f629bd7d0"
        },
        {
          "bytes": 7876,
          "mode": 420,
          "path": "final-instructions/.codex/AGENTS.md",
          "sha256": "d9db90a3f19bb15fa915daeda850a0d381a8a2ec5b5b0f109e9fefad1ba208f1"
        },
        {
          "bytes": 7836,
          "mode": 420,
          "path": "final-instructions/.config/agents/AGENTS.md",
          "sha256": "1cd46822fb19c6b2922444f981bc565695de015b7e9d4a43e989a2cc56fefd04"
        },
        {
          "bytes": 25969,
          "mode": 420,
          "path": "final-instructions/.config/agents/operational-guidance.md",
          "sha256": "6c0d6ae65c7aebda0d4785115841e7cf2e65e3ddd2a71485b71cce0e488fa2d4"
        },
        {
          "bytes": 102,
          "mode": 420,
          "path": "fragments/fixture-cli.md",
          "sha256": "c793c721c60c90240770a69d33be77a7c06dd6868cea1cff55c77aeed9307413"
        },
        {
          "bytes": 0,
          "mode": 420,
          "path": "fragments/fixture-mcp.md",
          "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        },
        {
          "bytes": 0,
          "mode": 420,
          "path": "fragments/fixture-third-party.md",
          "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        },
        {
          "bytes": 598,
          "mode": 420,
          "path": "fragments/huyang.md",
          "sha256": "cb4dcd48812d76b0298e864a3bb0b9a50e0add6afecbdf132e113900581e2264"
        },
        {
          "bytes": 21470,
          "mode": 420,
          "path": "menu.json",
          "sha256": "b77bbc5eb86ee6bb9422f7fa1251af1ab9f4555a75f6e327ff910b49d3804691"
        },
        {
          "bytes": 14128,
          "mode": 420,
          "path": "menu.txt",
          "sha256": "4a6ebecb761b0113f90cc55822d176fdeee1c030e96eb2f6f2dc8513fcdbe795"
        }
      ],
      "guidance_manifest_sha256": "ae511a13e2cb9e8b9967824a54f2c88bcb1799a6aa83bc760821bf7f91186e9a",
      "layout_sha256": "3fc46a2002e917bc1c8a1850769fc093a8b7703296955a61e67ca98ba7594334",
      "owner_handoff": "blocked-owner-handoff",
      "policy_sha256": "eb4007baaf0592dc7c103c759793c1313b8a6d6650668cf733ed9cbe7633e8aa",
      "producer_receipt_sha256": "74c8dba65aa9c1de49dba4ded25569a15b060c6a897f8f091df3dbab02bfc51f",
      "profile_sha256": "a74a5cbce1d4eb4fc89f2caf53297d5e3e088f055a0b2b9e30230e681a61061e",
      "required_global_policy_sha256": "fd02a30aacfbd1f6dda95f429d6e970385e6bd0ca7dce722e3d3da87acefc3f5",
      "schema": "dual-guidance-summary/v1",
      "schema_version": 1,
      "status": "required-interface-blocked"
    },
    "projection_sha256": "78a03b52715dd86f8dd302af7937c4fbbf12ee1a8de8e7c9ca939c0178015b77",
    "schema_version": 1
  },
  "owner": "upkeeper",
  "owner_descriptor_sha256": "55f76514d0efba3c80e62ecc9ba4d2d17518669ae5bc70d53e720d8178886aa2",
  "policy": {
    "canonical_policy_sha256": "eb4007baaf0592dc7c103c759793c1313b8a6d6650668cf733ed9cbe7633e8aa",
    "configured_catalog_validation": "completed-at-preparation",
    "current_authorization": "unknown",
    "derivation": "author-derive-policy-v1",
    "input_contract": "canonical-release-manifest-v1",
    "input_manifest_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "interpretation": "steward-parser-and-pinned-generator",
    "policy_format": "route-policy/v1-canonical-json",
    "readiness": "unassessed",
    "upstream_commit": "b32c0b6f48e12b7d74a60f8a010f84bab73d299f",
    "upstream_repository": "github.com/iryzhkov/omarchy-setup",
    "upstream_tree": "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52"
  },
  "schema_version": 5,
  "source_commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "source_repository": "github.com/iryzhkov/UpKeeper",
  "source_sha256": {
    "internal/toolproducer/testdata/descriptors/fixture-cli.json": "a0e6c6a8f4f1643c8a039ff97c0ff7a854edaba182bf9cdbfd7c52f3d279d74e",
    "internal/toolproducer/testdata/descriptors/fixture-mcp.json": "5d061d829dab6e36d6c1899e2d6f2780fb129ae25efc28fe688d59cc41a6ef5b",
    "internal/toolproducer/testdata/descriptors/fixture-third-party.json": "e0923229f54efe7396e9eb396c8f51179c56b94a9756b0316fb767411ae7950e",
    "shared/agent-instructions/config/bin/agents-instructions-gen": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "shared/agent-instructions/config/bin/agents-role-project": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "shared/agent-instructions/config/claude/CLAUDE.md": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "shared/agent-instructions/config/claude/skills/huyang/SKILL.md": "3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f",
    "shared/agent-instructions/config/claude/skills/implementation-session/SKILL.md": "37e202ab9bd16de8fd7f776966079c71b6f554bcaa3eb152099507fad0f4a567",
    "shared/agent-instructions/config/claude/skills/implementation-session/references/prompt-template.md": "4c46d852f6737f9c62b85b34275109c8f586b64fd60ae342a58047d35245f595",
    "shared/agent-instructions/config/claude/skills/ov-memory-curation/SKILL.md": "c9ae9a8555c7533d2cf24a9aaa235a165da35cf3215b194902363de31e53e82f",
    "shared/agent-instructions/config/claude/skills/ov-memory/SKILL.md": "e0f282759a22b73c35e45fac382d9121ea60782533411a58552ae4b0aec47453",
    "shared/agent-instructions/config/claude/skills/publish-agent-tooling/SKILL.md": "65a8b78f7ce01bf9b61ea0b89db395659ee0fdadde3111d2eb528d7b1c6292cf",
    "shared/agent-instructions/config/claude/skills/t3-campaign/SKILL.md": "420fdd7e51cc689a1eea80db6444c2e8233f1fc657a7bc2d99ec2fa8ea25181d",
    "shared/agent-instructions/config/claude/skills/t3-campaign/references/executor-briefs.md": "fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8",
    "shared/agent-instructions/config/claude/skills/t3-task/SKILL.md": "b3af2689fb0815f4bcdf052991a9db2d75b2a20f1c2736fc0cfc71254c59539b",
    "shared/agent-instructions/config/claude/skills/t3-wait/SKILL.md": "4695ba110bc471feff890f0b4c0429be09ded9824c0c1d04b6fefe075765d955",
    "shared/agent-instructions/owner.json": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "shared/catalog-guidance/config/bin/agents-tool-project": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "shared/catalog-guidance/guidance/fixture-cli.json": "42c026616fbaa9111ca91fdcb229ef11aacd1d85294eb0de0f75e822936db202",
    "shared/catalog-guidance/guidance/fixture-mcp.json": "c77844249d667114540692312878cb9704e17767884953aaffd0d31461958674",
    "shared/catalog-guidance/guidance/fixture-third-party.json": "b159186d27cfd929e772b6ecb464d8dd80ce4f6a01da95477606a4c7e65c1f01",
    "shared/catalog-guidance/guidance/huyang.json": "b41eeba148797d10d45b7bc727513fbc9c062eae69db1487df5aa53c408c80be",
    "shared/catalog-guidance/guidance/manifest.json": "ae511a13e2cb9e8b9967824a54f2c88bcb1799a6aa83bc760821bf7f91186e9a",
    "shared/catalog-guidance/guidance/required-global-policy.json": "fd02a30aacfbd1f6dda95f429d6e970385e6bd0ca7dce722e3d3da87acefc3f5",
    "shared/catalog-guidance/owner.json": "55f76514d0efba3c80e62ecc9ba4d2d17518669ae5bc70d53e720d8178886aa2",
    "shared/catalog-guidance/templates/discovery.txt": "3fc46a2002e917bc1c8a1850769fc093a8b7703296955a61e67ca98ba7594334"
  },
  "state": "candidate-inert"
}
`,
}

func catalogFixture(t *testing.T, kind CatalogDocumentKind) map[string]any {
	t.Helper()
	v, err := canonjson.DecodeObject([]byte(catalogTestFixtures[kind]))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func catalogTestBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, e := canonjson.MarshalFile(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func catalogReject(t *testing.T, kind CatalogDocumentKind, v any) {
	t.Helper()
	if err := ValidateCatalogDocumentEncoding(kind, catalogTestBytes(t, v)); err == nil {
		t.Fatal("invalid obligation accepted")
	}
}
func TestCatalogFrozenPositive(t *testing.T) {
	for _, kind := range []CatalogDocumentKind{CatalogOwner5, CatalogRequest4, CatalogSummary4, CatalogReceipt5} {
		t.Run(string(kind), func(t *testing.T) {
			raw := []byte(catalogTestFixtures[kind])
			v := catalogFixture(t, kind)
			if !bytes.Equal(raw, catalogTestBytes(t, v)) {
				t.Fatal("fixture canonical roundtrip")
			}
			d, err := ParseCatalogDocumentEncoding(kind, raw)
			if err != nil {
				t.Fatal(err)
			}
			if d.Kind() != kind || !bytes.Equal(d.Encoding(), raw) {
				t.Fatal("validated document changed")
			}
			raw[0] = 'x'
			copyBytes := d.Encoding()
			copyBytes[0] = 'x'
			if d.Encoding()[0] != '{' {
				t.Fatal("validated bytes aliased")
			}
			if kind == CatalogSummary4 && v["guidance"].(map[string]any)["status"] != "required-interface-blocked" {
				t.Fatal("blocking audit fixture lost")
			}
			if kind == CatalogReceipt5 {
				c := v["catalog"].(map[string]any)
				if c["authorized"] != "unknown" || c["enforcement"] != "unqualified" {
					t.Fatal("fixture must carry no runtime grant")
				}
			}
		})
	}
}
func TestCatalogRootInventories(t *testing.T) {
	inventories := map[CatalogDocumentKind]string{
		CatalogOwner5:   "catalog_input_contract evidence_source_files future_merges import_commit import_repository import_source_files import_tree instruction_files overlay_source_files owner policy_input_contract retired_reference schema_version skill_roots source_repository state",
		CatalogRequest4: "catalog_path catalog_sha256 guidance_manifest_path guidance_manifest_sha256 imported_files imported_output_root observation_path observation_sha256 platform policy_input_path policy_input_sha256 producer_input_path producer_input_sha256 producer_receipt_path producer_receipt_sha256 profile_path profile_sha256 schema_version state template_path template_sha256 tokenizer_manifest_path tokenizer_manifest_sha256 tokenizer_root",
		CatalogSummary4: "catalog_sha256 discovery_tokens files guidance modified_paths policy_sha256 producer_receipt_sha256 request_sha256 resident_tokens schema_version state template_sha256 tokenizer tokenizer_manifest_sha256",
		CatalogReceipt5: "catalog files guidance owner owner_descriptor_sha256 policy schema_version source_commit source_repository source_sha256 state",
	}
	for kind, inventory := range inventories {
		v := catalogFixture(t, kind)
		keys := strings.Fields(inventory)
		if len(v) != len(keys) {
			t.Fatal("root inventory size")
		}
		for _, k := range keys {
			if _, ok := v[k]; !ok {
				t.Fatal("root inventory", k)
			}
		}
	}
	r := catalogFixture(t, CatalogReceipt5)
	for field, count := range map[string]int{"catalog": 15, "policy": 12, "source_sha256": 26, "guidance": 9} {
		if len(r[field].(map[string]any)) != count {
			t.Fatal("nested inventory", field)
		}
	}
	if len(r["files"].([]any)) != 48 {
		t.Fatal("payload inventory")
	}
}

// Every object occurrence in independently held fixtures is exercised, including
// all source-map entries, policy/catalog, decisions/owners and file/package rows.
type catalogTestStep struct {
	key   string
	index int
	array bool
}

func catalogObjectPaths(v any, path []catalogTestStep, out *[][]catalogTestStep) {
	switch x := v.(type) {
	case map[string]any:
		*out = append(*out, append([]catalogTestStep(nil), path...))
		for k, e := range x {
			catalogObjectPaths(e, append(append([]catalogTestStep(nil), path...), catalogTestStep{key: k}), out)
		}
	case []any:
		for i, e := range x {
			catalogObjectPaths(e, append(append([]catalogTestStep(nil), path...), catalogTestStep{index: i, array: true}), out)
		}
	}
}
func catalogTestAt(v any, path []catalogTestStep) map[string]any {
	for _, s := range path {
		if s.array {
			v = v.([]any)[s.index]
		} else {
			v = v.(map[string]any)[s.key]
		}
	}
	return v.(map[string]any)
}
func TestCatalogEveryClosedObject(t *testing.T) {
	for _, kind := range []CatalogDocumentKind{CatalogOwner5, CatalogRequest4, CatalogSummary4, CatalogReceipt5} {
		base := catalogFixture(t, kind)
		var paths [][]catalogTestStep
		catalogObjectPaths(base, nil, &paths)
		for i, path := range paths {
			t.Run(fmt.Sprintf("%s/object%d", kind, i), func(t *testing.T) {
				v := catalogFixture(t, kind)
				o := catalogTestAt(v, path)
				o["unknown_wire_key"] = true
				catalogReject(t, kind, v)
				for key := range catalogTestAt(base, path) {
					t.Run("missing/"+key, func(t *testing.T) {
						v := catalogFixture(t, kind)
						delete(catalogTestAt(v, path), key)
						catalogReject(t, kind, v)
					})
					t.Run("duplicate/"+key, func(t *testing.T) {
						v := catalogFixture(t, kind)
						catalogTestAt(v, path)["zz_duplicate_marker"] = nil
						raw := catalogTestBytes(t, v)
						raw = bytes.Replace(raw, []byte(canonjson.EscapeASCII("zz_duplicate_marker")), []byte(canonjson.EscapeASCII(key)), 1)
						if ValidateCatalogDocumentEncoding(kind, raw) == nil {
							t.Fatal("nested duplicate accepted")
						}
					})
				}
			})
		}
	}
}
func TestCatalogVersionsAndKinds(t *testing.T) {
	for _, kind := range []CatalogDocumentKind{CatalogOwner5, CatalogRequest4, CatalogSummary4, CatalogReceipt5} {
		for _, version := range []any{true, false, nil, json.Number("1"), json.Number("2"), json.Number("3"), json.Number("99"), "5", json.Number("5.0"), json.Number("5e0")} {
			v := catalogFixture(t, kind)
			v["schema_version"] = version
			catalogReject(t, kind, v)
		}
		for _, other := range []CatalogDocumentKind{CatalogOwner5, CatalogRequest4, CatalogSummary4, CatalogReceipt5, "", "unknown"} {
			if other == kind {
				continue
			}
			if ValidateCatalogDocumentEncoding(other, []byte(catalogTestFixtures[kind])) == nil {
				t.Fatal("wrong kind accepted")
			}
		}
	}
}
func TestCatalogWireEncoding(t *testing.T) {
	raw := []byte(catalogTestFixtures[CatalogRequest4])
	cases := [][]byte{
		nil, bytes.TrimSuffix(raw, []byte("\n")), append(bytes.Clone(raw), '\n'), append(bytes.Clone(raw), []byte("{}")...),
		append([]byte{0xef, 0xbb, 0xbf}, raw...), append([]byte{0xff}, raw...),
		bytes.Replace(raw, []byte(`"candidate-inert"`), []byte(`"\ud800"`), 1),
		bytes.Replace(raw, []byte(`"candidate-inert"`), []byte(`"\udc00"`), 1),
		bytes.Replace(raw, []byte(`"candidate-inert"`), []byte(`"\u0000"`), 1),
		bytes.Replace(raw, []byte(`"schema_version": 4`), []byte(`"schema_version": 4.0`), 1),
		bytes.Replace(raw, []byte(`"schema_version": 4`), []byte(`"schema_version": 4e0`), 1),
		bytes.Replace(raw, []byte("  "), []byte("\t"), 1),
		bytes.Replace(raw, []byte("candidate-inert"), []byte(`candidate\u002dinert`), 1),
	}
	for i, b := range cases {
		if ValidateCatalogDocumentEncoding(CatalogRequest4, b) == nil {
			t.Fatalf("encoding case %d accepted", i)
		}
	}
	for _, kind := range []CatalogDocumentKind{CatalogOwner5, CatalogRequest4, CatalogSummary4, CatalogReceipt5} {
		limit := 262144
		if kind == CatalogSummary4 {
			limit = 65536
		}
		b := append([]byte(catalogTestFixtures[kind]), bytes.Repeat([]byte(" "), limit+1)...)
		if ValidateCatalogDocumentEncoding(kind, b) == nil {
			t.Fatal("byte bound accepted")
		}
	}
	for _, b := range [][]byte{
		[]byte(strings.Repeat("[", 13) + "0" + strings.Repeat("]", 13)),
		[]byte("[" + strings.Repeat("0,", 1024) + "0]"),
	} {
		if ValidateCatalogDocumentEncoding(CatalogRequest4, b) == nil {
			t.Fatal("structural resource bound accepted")
		}
	}
	// Check the shared bounded seam at the limits rather than merely wrong-root rejection.
	if err := boundedSharedJSON([]byte(strings.Repeat("[", 11)+"0"+strings.Repeat("]", 11)), 12, 1024); err != nil {
		t.Fatal(err)
	}
	if err := boundedSharedJSON([]byte(strings.Repeat("[", 12)+"0"+strings.Repeat("]", 12)), 12, 1024); err == nil {
		t.Fatal("depth boundary")
	}
	if err := boundedSharedJSON([]byte("["+strings.Repeat("0,", 1023)+"0]"), 12, 1024); err != nil {
		t.Fatal(err)
	}
	if err := boundedSharedJSON([]byte("["+strings.Repeat("0,", 1024)+"0]"), 12, 1024); err == nil {
		t.Fatal("container boundary")
	}
}
func TestCatalogPathsPinsValues(t *testing.T) {
	for _, bad := range []string{"relative", "/", "/a/", "/a//b", "/a/./b", "/a/../b", "/a\\b", "/a\x00b", "/a\nb", "/a\x7fb", "/a\u0085b", "/" + strings.Repeat("a", 256), "/" + strings.Repeat("a/", 256) + "b"} {
		for _, field := range []string{"catalog_path", "guidance_manifest_path", "profile_path", "observation_path", "imported_output_root", "tokenizer_root"} {
			v := catalogFixture(t, CatalogRequest4)
			v[field] = bad
			catalogReject(t, CatalogRequest4, v)
		}
	}
	for _, path := range []string{"/a/b", "/caf\u00e9/input", "/astral-\U0001f600/input"} {
		v := catalogFixture(t, CatalogRequest4)
		v["profile_path"] = path
		if err := ValidateCatalogDocumentEncoding(CatalogRequest4, catalogTestBytes(t, v)); err != nil {
			t.Fatal("valid Unicode canonical absolute path", err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), "sha256:" + strings.Repeat("a", 64)} {
		for _, kind := range []CatalogDocumentKind{CatalogRequest4, CatalogSummary4, CatalogReceipt5} {
			v := catalogFixture(t, kind)
			key := "catalog_sha256"
			if kind == CatalogReceipt5 {
				key = "owner_descriptor_sha256"
			}
			v[key] = bad
			catalogReject(t, kind, v)
		}
	}
	for _, bad := range []string{strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("A", 40), "HEAD"} {
		v := catalogFixture(t, CatalogReceipt5)
		v["source_commit"] = bad
		catalogReject(t, CatalogReceipt5, v)
	}
	for _, bad := range []any{json.Number("0"), json.Number("1201"), true, json.Number("1.0"), json.Number("1e2")} {
		v := catalogFixture(t, CatalogSummary4)
		v["discovery_tokens"] = bad
		catalogReject(t, CatalogSummary4, v)
	}
	v := catalogFixture(t, CatalogRequest4)
	v["platform"] = "windows-amd64"
	catalogReject(t, CatalogRequest4, v)
	for _, key := range []string{"import_source_files", "overlay_source_files", "instruction_files", "skill_roots", "future_merges", "evidence_source_files"} {
		v := catalogFixture(t, CatalogOwner5)
		a := v[key].([]any)
		a[0] = "invented/path"
		catalogReject(t, CatalogOwner5, v)
	}
}
func TestCatalogClosureAndBindings(t *testing.T) {
	for _, field := range []string{"files", "imported_files"} {
		kind := CatalogSummary4
		if field == "imported_files" {
			kind = CatalogRequest4
		}
		for _, mutation := range []string{"count", "order", "duplicate", "path", "mode", "hash"} {
			v := catalogFixture(t, kind)
			a := v[field].([]any)
			switch mutation {
			case "count":
				v[field] = a[:len(a)-1]
			case "order":
				a[0], a[1] = a[1], a[0]
			case "duplicate":
				a[1] = a[0]
			case "path":
				a[0].(map[string]any)["path"] = "invented/path"
			case "mode":
				a[0].(map[string]any)["mode"] = json.Number("384")
			case "hash":
				a[0].(map[string]any)["sha256"] = "bad"
			}
			catalogReject(t, kind, v)
		}
	}
	for _, mutation := range []string{"count", "order", "duplicate", "path", "mode", "kind", "destination"} {
		v := catalogFixture(t, CatalogReceipt5)
		a := v["files"].([]any)
		switch mutation {
		case "count":
			v["files"] = a[:47]
		case "order":
			a[0], a[1] = a[1], a[0]
		case "duplicate":
			a[1] = a[0]
		case "path":
			a[0].(map[string]any)["path"] = "invented/path"
		case "mode":
			a[0].(map[string]any)["mode"] = json.Number("493")
		case "kind":
			a[0].(map[string]any)["kind"] = "skill"
		case "destination":
			a[0].(map[string]any)["logical_destination"] = "wrong"
		}
		catalogReject(t, CatalogReceipt5, v)
	}
	for _, field := range []string{"derivation", "authorized", "configured", "installed", "enforcement", "readiness", "platform", "tokenizer"} {
		v := catalogFixture(t, CatalogReceipt5)
		v["catalog"].(map[string]any)[field] = "approved"
		catalogReject(t, CatalogReceipt5, v)
	}
	for _, field := range []string{"derivation", "input_contract", "upstream_repository", "upstream_commit", "upstream_tree", "current_authorization", "configured_catalog_validation", "readiness", "interpretation", "policy_format"} {
		v := catalogFixture(t, CatalogReceipt5)
		v["policy"].(map[string]any)[field] = "approved"
		catalogReject(t, CatalogReceipt5, v)
	}
	for _, field := range []string{"catalog_sha256", "policy_sha256", "producer_receipt_sha256", "template_sha256"} {
		v := catalogFixture(t, CatalogSummary4)
		v[field] = strings.Repeat("0", 64)
		catalogReject(t, CatalogSummary4, v)
	}
	for _, field := range []string{"manifest_sha256", "profile_sha256", "observation_sha256", "global_policy_sha256", "layout_sha256", "projection_sha256"} {
		v := catalogFixture(t, CatalogReceipt5)
		v["guidance"].(map[string]any)[field] = strings.Repeat("0", 64)
		catalogReject(t, CatalogReceipt5, v)
	}
	v := catalogFixture(t, CatalogReceipt5)
	sources := v["source_sha256"].(map[string]any)
	delete(sources, "shared/catalog-guidance/owner.json")
	sources["invented/source"] = strings.Repeat("a", 64)
	catalogReject(t, CatalogReceipt5, v)
	v = catalogFixture(t, CatalogReceipt5)
	v["source_sha256"].(map[string]any)["shared/catalog-guidance/guidance/huyang.json"] = strings.Repeat("0", 64)
	catalogReject(t, CatalogReceipt5, v)
	for _, field := range []string{"path", "tool_id", "sha256"} {
		v := catalogFixture(t, CatalogReceipt5)
		v["guidance"].(map[string]any)["packages"].([]any)[0].(map[string]any)[field] = "invalid"
		catalogReject(t, CatalogReceipt5, v)
	}
}
func TestCatalogProjectionObligations(t *testing.T) {
	for _, mutation := range []string{"tool-id", "tool-order", "rule-id", "rule-duplicate", "capability-key", "decision", "source-kind", "source-status", "source-path", "owning-path", "source-pin", "evidence-pin", "status", "handoff", "file-count", "file-order", "file-path", "file-mode", "file-bytes", "empty", "fragment", "catalog-evidence"} {
		v := catalogFixture(t, CatalogSummary4)
		g := v["guidance"].(map[string]any)
		decisions := g["decisions"].([]any)
		d := decisions[3].(map[string]any)
		o := d["source_owner"].(map[string]any)
		files := g["files"].([]any)
		switch mutation {
		case "tool-id":
			d["tool_id"] = "t3-steward"
		case "tool-order":
			decisions[0], decisions[1] = decisions[1], decisions[0]
		case "rule-id":
			d["rule_ids"] = []any{"huyang.invented.v1"}
		case "rule-duplicate":
			d["rule_ids"] = []any{"huyang.repository-boundary.v1", "huyang.repository-boundary.v1"}
		case "capability-key":
			d["capabilities"] = []any{"runtime-grant"}
		case "decision":
			d["decision"] = "runtime-ready"
		case "source-kind":
			o["kind"] = "approved"
		case "source-status":
			o["status"] = "approved"
		case "source-path":
			o["source_path"] = "../source"
		case "owning-path":
			o["owning_path"] = "shared/elsewhere"
		case "source-pin":
			o["source_sha256"] = "bad"
		case "evidence-pin":
			o["evidence_sha256"] = "bad"
		case "status":
			g["status"] = "fixture-projection-only"
		case "handoff":
			g["owner_handoff"] = "approved"
		case "file-count":
			g["files"] = files[:10]
		case "file-order":
			files[0], files[1] = files[1], files[0]
		case "file-path":
			files[0].(map[string]any)["path"] = "../discovery.txt"
		case "file-mode":
			files[0].(map[string]any)["mode"] = json.Number("493")
		case "file-bytes":
			files[0].(map[string]any)["bytes"] = json.Number("-1")
		case "empty":
			files[6].(map[string]any)["bytes"] = json.Number("1")
		case "fragment":
			d["fragment_sha256"] = strings.Repeat("0", 64)
		case "catalog-evidence":
			decisions[0].(map[string]any)["source_owner"].(map[string]any)["evidence_sha256"] = strings.Repeat("0", 64)
		}
		catalogReject(t, CatalogSummary4, v)
	}
	// Changing the embedded projection and updating its digest must still fail
	// when its included payload/source bindings disagree.
	v := catalogFixture(t, CatalogReceipt5)
	g := v["guidance"].(map[string]any)
	projection := g["projection"].(map[string]any)
	projection["decisions"].([]any)[3].(map[string]any)["guidance_sha256"] = strings.Repeat("0", 64)
	b := catalogTestBytes(t, projection)
	g["projection_sha256"] = digestOf(b)
	catalogReject(t, CatalogReceipt5, v)
	// Self-contained source/input and unchanged-source copies cannot disagree.
	for _, source := range []string{
		"shared/catalog-guidance/guidance/manifest.json",
		"shared/catalog-guidance/guidance/required-global-policy.json",
		"shared/catalog-guidance/templates/discovery.txt",
		"shared/agent-instructions/config/claude/skills/huyang/SKILL.md",
	} {
		v := catalogFixture(t, CatalogReceipt5)
		v["source_sha256"].(map[string]any)[source] = strings.Repeat("0", 64)
		catalogReject(t, CatalogReceipt5, v)
	}
	v = catalogFixture(t, CatalogSummary4)
	v["guidance"].(map[string]any)["decisions"].([]any)[3].(map[string]any)["help_sha256"] = strings.Repeat("0", 64)
	catalogReject(t, CatalogSummary4, v)
	// Resource attacks retain the valid envelope and replace one field.
	v = catalogFixture(t, CatalogRequest4)
	v["imported_files"] = make([]any, 1025)
	catalogReject(t, CatalogRequest4, v)
	v = catalogFixture(t, CatalogOwner5)
	var nested any = "x"
	for i := 0; i < 13; i++ {
		nested = []any{nested}
	}
	v["future_merges"] = nested
	catalogReject(t, CatalogOwner5, v)
	// The opaque validated type cannot be confused with historical source types.
	if reflect.TypeOf(CatalogDocument{}) == reflect.TypeOf(SharedOwner{}) {
		t.Fatal("historical API changed")
	}
}
