//go:build linux

package author

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

type baselineOriginalRecord struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Object string `json:"object"`
	Bytes  int    `json:"bytes"`
	SHA    string `json:"sha256"`
	Type   string `json:"type"`
}
type baselineImportedRecord struct {
	Path  string `json:"path"`
	Mode  string `json:"git_mode"`
	Blob  string `json:"git_blob"`
	Bytes int    `json:"bytes"`
	SHA   string `json:"sha256"`
	Type  string `json:"type"`
}
type baselineLineage struct {
	Original baselineOriginalRecord `json:"original"`
	Imported baselineImportedRecord `json:"imported"`
	Diff     string                 `json:"literal_diff"`
}
type baselineComparison struct {
	Kind, Path, Actual, Expected, Status string
}

func baselineLineages() []baselineLineage {
	var v []baselineLineage
	if json.Unmarshal([]byte(baselineLineageJSON), &v) != nil || len(v) != 13 {
		panic("invalid owning lineage constant")
	}
	return v
}

const baselineLineageJSON = "[{\"bytes_equal\":false,\"imported\":{\"bytes\":24398,\"git_blob\":\"fa4593b1132d124a12a2b78415ee2b513248c424\",\"git_mode\":\"100755\",\"path\":\"shared/agent-instructions/config/bin/agents-instructions-gen\",\"sha256\":\"04f32b6a02ff9db34c54792d2de8b2b79fc014b7220ef70ffcbd6fc21bcdfaed\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/bin/agents-instructions-gen\",\"literal_diff\":\"--- original/config/bin/agents-instructions-gen\\n+++ shared/agent-instructions/config/bin/agents-instructions-gen\\n@@ -38,14 +38,13 @@\\n \\n usage() {\\n   cat <<'USAGE'\\n-Usage: agents-instructions-gen [--root DIR] [--policy FILE] [--check]\\n-\\n-Generate ~/.config/agents/ from an omarchy-setup checkout. The checkout is the only\\n+Usage: agents-instructions-gen --shared-root --root DIR --policy FILE [--check]\\n+\\n+Derive candidate instructions in private scratch HOME from explicit shared source. The checkout is the only\\n source; nothing is ever read back out of $HOME.\\n \\n-  --root DIR  The checkout to read. Default: $OMARCHY_SETUP_ROOT, else the checkout this\\n-              script is running from. A copy installed in ~/.local/bin is not in a\\n-              checkout, so it needs one of the two.\\n+  --shared-root  Required private shared-source transport.\\n+  --root DIR  Explicit shared source directory containing owner.json. Required.\\n   --policy FILE  Explicit reviewed route-policy/v1 input; parsed by t3-steward policy\\n                  show --file FILE --json. Never discovered under HOME. Native roles\\n                  require this option; without it legacy generation remains compatible.\\n@@ -62,6 +61,7 @@\\n root_source=''\\n mode=install\\n policy=''\\n+shared_root=0\\n while (($#)); do\\n   case $1 in\\n   --root)\\n@@ -89,6 +89,10 @@\\n     policy=${1#--policy=}\\n     shift\\n     ;;\\n+  --shared-root)\\n+    shared_root=1\\n+    shift\\n+    ;;\\n   --check)\\n     mode=check\\n     shift\\n@@ -100,6 +104,10 @@\\n   *) die \\\"unknown option: $1 (try --help)\\\" ;;\\n   esac\\n done\\n+\\n+# Shared transport is private derivation only; no installed or inferred root.\\n+((shared_root)) || die \\\"candidate generator requires --shared-root; host installation is not implemented\\\"\\n+[[ -n $root && -f $root/owner.json && -n $policy ]] || die \\\"candidate generation requires explicit shared source --root and --policy\\\"\\n \\n # ------------------------------------------------------------------- root --\\n # Three ways to name the checkout, tried in order, and no fourth. The failure names every\\n@@ -149,7 +157,7 @@\\n # Global agent instructions\\n \\n The shared instructions for the harnesses that do not read ~/.claude/CLAUDE.md. Generated\\n-by `agents-instructions-gen` from omarchy-setup's config/bin/agents-instructions-gen; edit\\n+by `agents-instructions-gen` from UpKeeper's shared/agent-instructions config/bin/agents-instructions-gen; edit\\n it there and regenerate rather than editing an installed copy.\\n \\n Each section below is a summary. The full guidance lives in a skill, loaded on demand from\\n@@ -183,7 +191,7 @@\\n \\n ## Publish validated agent-tooling changes\\n \\n-When working on agent tooling (Huyang, t3-steward, UpKeeper, agent99, shared\\n+When working on agent tooling (Huyang, t3-steward, UpKeeper, shared\\n AGENTS.md/CLAUDE.md files, skills or MCP configuration), finish a validated fix through\\n UpKeeper without waiting for a separate request to push: validate and publish the source,\\n confirm exact source CI, then review and publish the full release. Use ordinary\\n@@ -288,7 +296,7 @@\\n   cat <<'HDR'\\n # OV memory (shared agent instructions)\\n \\n-Generated from omarchy-setup's config/claude/skills/ov-memory{,-curation}/SKILL.md,\\n+Generated from UpKeeper's shared/agent-instructions config/claude/skills/ov-memory{,-curation}/SKILL.md,\\n which are the source of truth. Edit the skill in that repository and regenerate\\n with `agents-instructions-gen`; do not edit this file directly.\\n \\n@@ -302,7 +310,7 @@\\n   cat <<'HDR'\\n # T3 steward: tasks, campaigns and wait (shared agent instructions)\\n \\n-Generated from omarchy-setup's config/claude/skills/t3-task/SKILL.md,\\n+Generated from UpKeeper's shared/agent-instructions config/claude/skills/t3-task/SKILL.md,\\n config/claude/skills/t3-campaign/SKILL.md and config/claude/skills/t3-wait/SKILL.md,\\n which are the source of truth and are also what ~/.claude/skills/ holds. Edit the\\n skill in that repository, then regenerate with `agents-instructions-gen`; do not\\n@@ -335,7 +343,7 @@\\n   cat <<'HDR'\\n # Huyang: reading and editing code (shared agent instructions)\\n \\n-Generated from the \\\"Reading and editing code\\\" section of omarchy-setup's\\n+Generated from the \\\"Reading and editing code\\\" section of UpKeeper's shared/agent-instructions\\n config/claude/CLAUDE.md and from its config/claude/skills/huyang/SKILL.md, which are\\n the sources of truth. Edit them in that repository and regenerate with\\n `agents-instructions-gen`; do not edit this file directly.\\n@@ -351,7 +359,7 @@\\n {\\n   echo '# Jocasta: shared working documents'\\n   echo\\n-  echo \\\"Generated from the Working documents: Jocasta section of omarchy-setup's\\\"\\n+  echo \\\"Generated from the Working documents: Jocasta section of UpKeeper's shared/agent-instructions\\\"\\n   echo 'config/claude/CLAUDE.md. Edit it there and run agents-instructions-gen.'\\n   echo\\n   # The source paragraph points readers at this file; inside it that pointer is noise.\\n\",\"mode_equal\":true,\"original\":{\"bytes\":23968,\"mode\":\"100755\",\"object\":\"1799fc2d5243213c74215e5b700f9484b5149ab6\",\"path\":\"config/bin/agents-instructions-gen\",\"sha256\":\"ff383c79b0269decfaa43e374b4e7004f72792e1db1a71be373d6704655f79da\",\"type\":\"blob\"},\"original_path\":\"config/bin/agents-instructions-gen\",\"transformation\":\"private shared-root transport, explicit owner/root/policy guard, UpKeeper attribution and agent99 exclusion\"},{\"bytes_equal\":true,\"imported\":{\"bytes\":14584,\"git_blob\":\"49728c0fa19234bfab57eae419ccb9e1940d2a1f\",\"git_mode\":\"100755\",\"path\":\"shared/agent-instructions/config/bin/agents-role-project\",\"sha256\":\"325ec02f4974a9ce102fd131ba6953ae48f4d45c1a2353fa02681088f1d113fd\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/bin/agents-role-project\",\"mode_equal\":false,\"original\":{\"bytes\":14584,\"mode\":\"100644\",\"object\":\"49728c0fa19234bfab57eae419ccb9e1940d2a1f\",\"path\":\"config/bin/agents-role-project\",\"sha256\":\"325ec02f4974a9ce102fd131ba6953ae48f4d45c1a2353fa02681088f1d113fd\",\"type\":\"blob\"},\"original_path\":\"config/bin/agents-role-project\",\"transformation\":\"unchanged upstream role-helper blob copied executable: Git100644 -> Git100755\"},{\"bytes_equal\":false,\"imported\":{\"bytes\":16274,\"git_blob\":\"99e0822cca034a12d51ef86de19033334211d8a2\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/CLAUDE.md\",\"sha256\":\"59c21f214f8df452f1871375e061ba7d367a5a13e5dff7ece4f52260e2f61abd\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/CLAUDE.md\",\"literal_diff\":\"--- original/config/claude/CLAUDE.md\\n+++ shared/agent-instructions/config/claude/CLAUDE.md\\n@@ -193,7 +193,7 @@\\n \\n ## Publish validated agent-tooling changes\\n \\n-When working on agent tooling (Huyang, t3-steward, UpKeeper, agent99, shared\\n+When working on agent tooling (Huyang, t3-steward, UpKeeper, shared\\n AGENTS.md/CLAUDE.md files, skills or MCP configuration), finish a validated fix by publishing it\\n through UpKeeper without waiting for a separate request to push: run the checks, push the source\\n and confirm its GitHub CI, then `upkeeper push` from a host where the change is validated, using\\n\",\"mode_equal\":true,\"original\":{\"bytes\":16283,\"mode\":\"100644\",\"object\":\"ac6084ec238247469fcbebf5eba72fa330d89469\",\"path\":\"config/claude/CLAUDE.md\",\"sha256\":\"62a4ede6202f63c777d0b6f06f191b65fbac2a0b2bd1b8101fc4c8ffb317b750\",\"type\":\"blob\"},\"original_path\":\"config/claude/CLAUDE.md\",\"transformation\":\"remove the literal agent99 option from the publication-tool scope list; all other bytes exact\"},{\"bytes_equal\":true,\"imported\":{\"bytes\":6427,\"git_blob\":\"52f83f4af77f148e590cb8fd5613d3fda392c1c6\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/skills/huyang/SKILL.md\",\"sha256\":\"3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/skills/huyang/SKILL.md\",\"mode_equal\":true,\"original\":{\"bytes\":6427,\"mode\":\"100644\",\"object\":\"52f83f4af77f148e590cb8fd5613d3fda392c1c6\",\"path\":\"config/claude/skills/huyang/SKILL.md\",\"sha256\":\"3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f\",\"type\":\"blob\"},\"original_path\":\"config/claude/skills/huyang/SKILL.md\",\"transformation\":\"exact byte and Git-mode identity under shared/agent-instructions prefix\"},{\"bytes_equal\":true,\"imported\":{\"bytes\":4541,\"git_blob\":\"6294e894a89271cd9f087a3cecc19b19300d845a\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/skills/implementation-session/SKILL.md\",\"sha256\":\"37e202ab9bd16de8fd7f776966079c71b6f554bcaa3eb152099507fad0f4a567\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/skills/implementation-session/SKILL.md\",\"mode_equal\":true,\"original\":{\"bytes\":4541,\"mode\":\"100644\",\"object\":\"6294e894a89271cd9f087a3cecc19b19300d845a\",\"path\":\"config/claude/skills/implementation-session/SKILL.md\",\"sha256\":\"37e202ab9bd16de8fd7f776966079c71b6f554bcaa3eb152099507fad0f4a567\",\"type\":\"blob\"},\"original_path\":\"config/claude/skills/implementation-session/SKILL.md\",\"transformation\":\"exact byte and Git-mode identity under shared/agent-instructions prefix\"},{\"bytes_equal\":true,\"imported\":{\"bytes\":3979,\"git_blob\":\"fb32ce51ee4eb5db8ff5cdbc40ae1e56315cfb52\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/skills/implementation-session/references/prompt-template.md\",\"sha256\":\"4c46d852f6737f9c62b85b34275109c8f586b64fd60ae342a58047d35245f595\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/skills/implementation-session/references/prompt-template.md\",\"mode_equal\":true,\"original\":{\"bytes\":3979,\"mode\":\"100644\",\"object\":\"fb32ce51ee4eb5db8ff5cdbc40ae1e56315cfb52\",\"path\":\"config/claude/skills/implementation-session/references/prompt-template.md\",\"sha256\":\"4c46d852f6737f9c62b85b34275109c8f586b64fd60ae342a58047d35245f595\",\"type\":\"blob\"},\"original_path\":\"config/claude/skills/implementation-session/references/prompt-template.md\",\"transformation\":\"exact byte and Git-mode identity under shared/agent-instructions prefix\"},{\"bytes_equal\":true,\"imported\":{\"bytes\":4582,\"git_blob\":\"3e34515d7a537555b0229ebfcebc1af134d653f6\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/skills/ov-memory-curation/SKILL.md\",\"sha256\":\"c9ae9a8555c7533d2cf24a9aaa235a165da35cf3215b194902363de31e53e82f\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/skills/ov-memory-curation/SKILL.md\",\"mode_equal\":true,\"original\":{\"bytes\":4582,\"mode\":\"100644\",\"object\":\"3e34515d7a537555b0229ebfcebc1af134d653f6\",\"path\":\"config/claude/skills/ov-memory-curation/SKILL.md\",\"sha256\":\"c9ae9a8555c7533d2cf24a9aaa235a165da35cf3215b194902363de31e53e82f\",\"type\":\"blob\"},\"original_path\":\"config/claude/skills/ov-memory-curation/SKILL.md\",\"transformation\":\"exact byte and Git-mode identity under shared/agent-instructions prefix\"},{\"bytes_equal\":true,\"imported\":{\"bytes\":7259,\"git_blob\":\"87f30ad47712cdb071f7921a79ab84160a7e0649\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/skills/ov-memory/SKILL.md\",\"sha256\":\"e0f282759a22b73c35e45fac382d9121ea60782533411a58552ae4b0aec47453\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/skills/ov-memory/SKILL.md\",\"mode_equal\":true,\"original\":{\"bytes\":7259,\"mode\":\"100644\",\"object\":\"87f30ad47712cdb071f7921a79ab84160a7e0649\",\"path\":\"config/claude/skills/ov-memory/SKILL.md\",\"sha256\":\"e0f282759a22b73c35e45fac382d9121ea60782533411a58552ae4b0aec47453\",\"type\":\"blob\"},\"original_path\":\"config/claude/skills/ov-memory/SKILL.md\",\"transformation\":\"exact byte and Git-mode identity under shared/agent-instructions prefix\"},{\"bytes_equal\":false,\"imported\":{\"bytes\":5030,\"git_blob\":\"5d66a52eb87a5ad78466eb8dce5c8fa197b75c99\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/skills/publish-agent-tooling/SKILL.md\",\"sha256\":\"65a8b78f7ce01bf9b61ea0b89db395659ee0fdadde3111d2eb528d7b1c6292cf\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/skills/publish-agent-tooling/SKILL.md\",\"literal_diff\":\"--- original/config/claude/skills/publish-agent-tooling/SKILL.md\\n+++ shared/agent-instructions/config/claude/skills/publish-agent-tooling/SKILL.md\\n@@ -4,7 +4,7 @@\\n   The publication path for a validated change to agent tooling: run checks,\\n   publish source and CI, prepare or verify an UpKeeper release, review the full\\n   release, and gate fleet convergence. Use whenever work touches Huyang,\\n-  t3-steward, UpKeeper, agent99, shared AGENTS.md or CLAUDE.md files, skills, or\\n+  t3-steward, UpKeeper, shared AGENTS.md or CLAUDE.md files, skills, or\\n   MCP configuration. Triggers: upkeeper push, release manifest, capture,\\n   authored release, converge, publish the fix, instruction change, skill change,\\n   MCP registration.\\n\",\"mode_equal\":true,\"original\":{\"bytes\":5039,\"mode\":\"100644\",\"object\":\"f7f3cdf4d4f183c2e5f304add853bf8826cd3833\",\"path\":\"config/claude/skills/publish-agent-tooling/SKILL.md\",\"sha256\":\"8b16260ce2c5dd30ea6a16f9ddec1dfb133194fa041e45dd9d2e9657fed207e2\",\"type\":\"blob\"},\"original_path\":\"config/claude/skills/publish-agent-tooling/SKILL.md\",\"transformation\":\"remove the literal agent99 option from the publication-tool scope list; all other bytes exact\"},{\"bytes_equal\":true,\"imported\":{\"bytes\":22618,\"git_blob\":\"754cb468f5eb22b0b91ad9267e0f098c8b360cef\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/skills/t3-campaign/SKILL.md\",\"sha256\":\"420fdd7e51cc689a1eea80db6444c2e8233f1fc657a7bc2d99ec2fa8ea25181d\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/skills/t3-campaign/SKILL.md\",\"mode_equal\":true,\"original\":{\"bytes\":22618,\"mode\":\"100644\",\"object\":\"754cb468f5eb22b0b91ad9267e0f098c8b360cef\",\"path\":\"config/claude/skills/t3-campaign/SKILL.md\",\"sha256\":\"420fdd7e51cc689a1eea80db6444c2e8233f1fc657a7bc2d99ec2fa8ea25181d\",\"type\":\"blob\"},\"original_path\":\"config/claude/skills/t3-campaign/SKILL.md\",\"transformation\":\"exact byte and Git-mode identity under shared/agent-instructions prefix\"},{\"bytes_equal\":true,\"imported\":{\"bytes\":10411,\"git_blob\":\"8c2b3fcdea37336573905f747ccc7d019101f634\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/skills/t3-campaign/references/executor-briefs.md\",\"sha256\":\"fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/skills/t3-campaign/references/executor-briefs.md\",\"mode_equal\":true,\"original\":{\"bytes\":10411,\"mode\":\"100644\",\"object\":\"8c2b3fcdea37336573905f747ccc7d019101f634\",\"path\":\"config/claude/skills/t3-campaign/references/executor-briefs.md\",\"sha256\":\"fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8\",\"type\":\"blob\"},\"original_path\":\"config/claude/skills/t3-campaign/references/executor-briefs.md\",\"transformation\":\"exact byte and Git-mode identity under shared/agent-instructions prefix\"},{\"bytes_equal\":true,\"imported\":{\"bytes\":7130,\"git_blob\":\"9763298de8b0264439ecebf551572d7968bff362\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/skills/t3-task/SKILL.md\",\"sha256\":\"b3af2689fb0815f4bcdf052991a9db2d75b2a20f1c2736fc0cfc71254c59539b\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/skills/t3-task/SKILL.md\",\"mode_equal\":true,\"original\":{\"bytes\":7130,\"mode\":\"100644\",\"object\":\"9763298de8b0264439ecebf551572d7968bff362\",\"path\":\"config/claude/skills/t3-task/SKILL.md\",\"sha256\":\"b3af2689fb0815f4bcdf052991a9db2d75b2a20f1c2736fc0cfc71254c59539b\",\"type\":\"blob\"},\"original_path\":\"config/claude/skills/t3-task/SKILL.md\",\"transformation\":\"exact byte and Git-mode identity under shared/agent-instructions prefix\"},{\"bytes_equal\":true,\"imported\":{\"bytes\":6081,\"git_blob\":\"26e64ff923ccccce0ea09ea4bcc257f1ff760b2f\",\"git_mode\":\"100644\",\"path\":\"shared/agent-instructions/config/claude/skills/t3-wait/SKILL.md\",\"sha256\":\"4695ba110bc471feff890f0b4c0429be09ded9824c0c1d04b6fefe075765d955\",\"type\":\"blob\"},\"imported_path\":\"shared/agent-instructions/config/claude/skills/t3-wait/SKILL.md\",\"mode_equal\":true,\"original\":{\"bytes\":6081,\"mode\":\"100644\",\"object\":\"26e64ff923ccccce0ea09ea4bcc257f1ff760b2f\",\"path\":\"config/claude/skills/t3-wait/SKILL.md\",\"sha256\":\"4695ba110bc471feff890f0b4c0429be09ded9824c0c1d04b6fefe075765d955\",\"type\":\"blob\"},\"original_path\":\"config/claude/skills/t3-wait/SKILL.md\",\"transformation\":\"exact byte and Git-mode identity under shared/agent-instructions prefix\"}]"
const baselineComparisonJSON = "[{\"actual\":\"6dbef9e57cd905eb11704515891c5cb9e15bb4e153aabef812c4975805020758\",\"expected\":\"6dbef9e57cd905eb11704515891c5cb9e15bb4e153aabef812c4975805020758\",\"kind\":\"baseline-file\",\"path\":\".claude/agents/critical-reviewer.md\",\"status\":\"MET\"},{\"actual\":\"4afdf4c1cc0cd2b4be386ed0532cb3eaa43a8ac3a76e8e48589d40e301e7d366\",\"expected\":\"4afdf4c1cc0cd2b4be386ed0532cb3eaa43a8ac3a76e8e48589d40e301e7d366\",\"kind\":\"baseline-file\",\"path\":\".claude/agents/executor.md\",\"status\":\"MET\"},{\"actual\":\"58e47dae31e82c260a8ea3b4f10c1c522c449bdad81538dadd4c3ab3f71187e3\",\"expected\":\"58e47dae31e82c260a8ea3b4f10c1c522c449bdad81538dadd4c3ab3f71187e3\",\"kind\":\"baseline-file\",\"path\":\".claude/agents/planner.md\",\"status\":\"MET\"},{\"actual\":\"13354f6d60ad4b307cfaa885923b16a29b8944b2a4510d641f8bf3d9901c3992\",\"expected\":\"13354f6d60ad4b307cfaa885923b16a29b8944b2a4510d641f8bf3d9901c3992\",\"kind\":\"baseline-file\",\"path\":\".claude/agents/reader.md\",\"status\":\"MET\"},{\"actual\":\"9613123a697cf4521f27498dcfc51d31cbe3cf5db26caada0452357e73ac4ea3\",\"expected\":\"9613123a697cf4521f27498dcfc51d31cbe3cf5db26caada0452357e73ac4ea3\",\"kind\":\"baseline-file\",\"path\":\".claude/agents/reviewer.md\",\"status\":\"MET\"},{\"actual\":\"1de7c144dd4a8b64ba2af7a067cb1d3417284d9e679e85d2f75eede955696951\",\"expected\":\"1de7c144dd4a8b64ba2af7a067cb1d3417284d9e679e85d2f75eede955696951\",\"kind\":\"baseline-file\",\"path\":\".claude/omarchy-setup/CLAUDE.md\",\"status\":\"MET\"},{\"actual\":\"cf60fdccc1adf91141ac94fb2db08c62f235057c2a75eede12edd4210cf14142\",\"expected\":\"cf60fdccc1adf91141ac94fb2db08c62f235057c2a75eede12edd4210cf14142\",\"kind\":\"baseline-file\",\"path\":\".codex/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"44f75e02abc4e168453bc913c56b5d4b7d5f8a7b097510e7fc5dae2a0b0d25ae\",\"expected\":\"44f75e02abc4e168453bc913c56b5d4b7d5f8a7b097510e7fc5dae2a0b0d25ae\",\"kind\":\"baseline-file\",\"path\":\".codex/critical-reviewer.config.toml\",\"status\":\"MET\"},{\"actual\":\"e9559ddfad5dfd37c56ef116778348ee3a7985c6e349583df2aaef8aaec25665\",\"expected\":\"e9559ddfad5dfd37c56ef116778348ee3a7985c6e349583df2aaef8aaec25665\",\"kind\":\"baseline-file\",\"path\":\".codex/executor.config.toml\",\"status\":\"MET\"},{\"actual\":\"072818376f0d476ae38a24199a7863bb549a3db82fd6a2e28e59d0bfd720a343\",\"expected\":\"072818376f0d476ae38a24199a7863bb549a3db82fd6a2e28e59d0bfd720a343\",\"kind\":\"baseline-file\",\"path\":\".codex/planner.config.toml\",\"status\":\"MET\"},{\"actual\":\"d6b4a41a5de7ca9708321ceb7596420b6b858b5fa20694a2bd3643c2be06e0b3\",\"expected\":\"d6b4a41a5de7ca9708321ceb7596420b6b858b5fa20694a2bd3643c2be06e0b3\",\"kind\":\"baseline-file\",\"path\":\".codex/reader.config.toml\",\"status\":\"MET\"},{\"actual\":\"7725eeb075952ddf9df017e7af68c267fe2688c91af562d81061bffaa977e076\",\"expected\":\"7725eeb075952ddf9df017e7af68c267fe2688c91af562d81061bffaa977e076\",\"kind\":\"baseline-file\",\"path\":\".codex/reviewer.config.toml\",\"status\":\"MET\"},{\"actual\":\"4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9\",\"expected\":\"4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9\",\"kind\":\"baseline-file\",\"path\":\".config/agents/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"d325c443031af38500a36f89d964e0f6f240e5acc4f2dabe6e0b011f5dfce8e4\",\"expected\":\"d325c443031af38500a36f89d964e0f6f240e5acc4f2dabe6e0b011f5dfce8e4\",\"kind\":\"baseline-file\",\"path\":\".config/agents/huyang.md\",\"status\":\"MET\"},{\"actual\":\"423e462fb26bc75972d13717f53bd21e29425f5cb3a9f6d37f6080e95216f6e6\",\"expected\":\"423e462fb26bc75972d13717f53bd21e29425f5cb3a9f6d37f6080e95216f6e6\",\"kind\":\"baseline-file\",\"path\":\".config/agents/jocasta.md\",\"status\":\"MET\"},{\"actual\":\"23bba5308a40c68f6107a72be1b0bb34113570439c9d09b134bdffc2356ea6c1\",\"expected\":\"23bba5308a40c68f6107a72be1b0bb34113570439c9d09b134bdffc2356ea6c1\",\"kind\":\"baseline-file\",\"path\":\".config/agents/ov-memory.md\",\"status\":\"MET\"},{\"actual\":\"fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8\",\"expected\":\"fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8\",\"kind\":\"baseline-file\",\"path\":\".config/agents/t3-campaign-executor-briefs.md\",\"status\":\"MET\"},{\"actual\":\"31f405e47c776324044cc2d7dbfa5efb044ae293d81a48967312dcc06d925ff5\",\"expected\":\"31f405e47c776324044cc2d7dbfa5efb044ae293d81a48967312dcc06d925ff5\",\"kind\":\"baseline-file\",\"path\":\".config/agents/t3-steward.md\",\"status\":\"MET\"},{\"actual\":\"1de7c144dd4a8b64ba2af7a067cb1d3417284d9e679e85d2f75eede955696951\",\"expected\":\"1de7c144dd4a8b64ba2af7a067cb1d3417284d9e679e85d2f75eede955696951\",\"kind\":\"layout-main-pin\",\"path\":\".claude/omarchy-setup/CLAUDE.md\",\"status\":\"MET\"},{\"actual\":\"cf60fdccc1adf91141ac94fb2db08c62f235057c2a75eede12edd4210cf14142\",\"expected\":\"cf60fdccc1adf91141ac94fb2db08c62f235057c2a75eede12edd4210cf14142\",\"kind\":\"layout-main-pin\",\"path\":\".codex/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9\",\"expected\":\"4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9\",\"kind\":\"layout-main-pin\",\"path\":\".config/agents/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"5178e9c6dea5f76c3022fb59d85fbadc928d503a3a8a630ffa49bee9177ce9cc\",\"expected\":\"5178e9c6dea5f76c3022fb59d85fbadc928d503a3a8a630ffa49bee9177ce9cc\",\"kind\":\"exact-model-block-inclusion\",\"path\":\".claude/omarchy-setup/CLAUDE.md\",\"status\":\"MET\"},{\"actual\":\"81e4e1d0b911c999c18748e410a52d245249e4fe07ac2b217acdc10e76ccc74c\",\"expected\":\"81e4e1d0b911c999c18748e410a52d245249e4fe07ac2b217acdc10e76ccc74c\",\"kind\":\"exact-model-block-inclusion\",\"path\":\".codex/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"81e4e1d0b911c999c18748e410a52d245249e4fe07ac2b217acdc10e76ccc74c\",\"expected\":\"81e4e1d0b911c999c18748e410a52d245249e4fe07ac2b217acdc10e76ccc74c\",\"kind\":\"exact-model-block-inclusion\",\"path\":\".config/agents/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"d325c443031af38500a36f89d964e0f6f240e5acc4f2dabe6e0b011f5dfce8e4\",\"expected\":\"d325c443031af38500a36f89d964e0f6f240e5acc4f2dabe6e0b011f5dfce8e4\",\"kind\":\"huyang-help\",\"path\":\".config/agents/huyang.md\",\"status\":\"MET\"},{\"actual\":\"4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9\",\"expected\":\"4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9\",\"kind\":\"huyang-owner-evidence\",\"path\":\".config/agents/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"1de7c144dd4a8b64ba2af7a067cb1d3417284d9e679e85d2f75eede955696951\",\"expected\":\"1de7c144dd4a8b64ba2af7a067cb1d3417284d9e679e85d2f75eede955696951\",\"kind\":\"global-main-pin\",\"path\":\".claude/omarchy-setup/CLAUDE.md\",\"status\":\"MET\"},{\"actual\":\"cf60fdccc1adf91141ac94fb2db08c62f235057c2a75eede12edd4210cf14142\",\"expected\":\"cf60fdccc1adf91141ac94fb2db08c62f235057c2a75eede12edd4210cf14142\",\"kind\":\"global-main-pin\",\"path\":\".codex/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9\",\"expected\":\"4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9\",\"kind\":\"global-main-pin\",\"path\":\".config/agents/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"5178e9c6dea5f76c3022fb59d85fbadc928d503a3a8a630ffa49bee9177ce9cc\",\"expected\":\"5178e9c6dea5f76c3022fb59d85fbadc928d503a3a8a630ffa49bee9177ce9cc\",\"kind\":\"layout-model-block-pin\",\"path\":\".claude/omarchy-setup/CLAUDE.md\",\"status\":\"MET\"},{\"actual\":\"81e4e1d0b911c999c18748e410a52d245249e4fe07ac2b217acdc10e76ccc74c\",\"expected\":\"81e4e1d0b911c999c18748e410a52d245249e4fe07ac2b217acdc10e76ccc74c\",\"kind\":\"layout-model-block-pin\",\"path\":\".codex/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"81e4e1d0b911c999c18748e410a52d245249e4fe07ac2b217acdc10e76ccc74c\",\"expected\":\"81e4e1d0b911c999c18748e410a52d245249e4fe07ac2b217acdc10e76ccc74c\",\"kind\":\"layout-model-block-pin\",\"path\":\".config/agents/AGENTS.md\",\"status\":\"MET\"},{\"actual\":\"04f32b6a02ff9db34c54792d2de8b2b79fc014b7220ef70ffcbd6fc21bcdfaed\",\"expected\":\"04f32b6a02ff9db34c54792d2de8b2b79fc014b7220ef70ffcbd6fc21bcdfaed\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/bin/agents-instructions-gen\",\"status\":\"MET\"},{\"actual\":\"325ec02f4974a9ce102fd131ba6953ae48f4d45c1a2353fa02681088f1d113fd\",\"expected\":\"325ec02f4974a9ce102fd131ba6953ae48f4d45c1a2353fa02681088f1d113fd\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/bin/agents-role-project\",\"status\":\"MET\"},{\"actual\":\"59c21f214f8df452f1871375e061ba7d367a5a13e5dff7ece4f52260e2f61abd\",\"expected\":\"59c21f214f8df452f1871375e061ba7d367a5a13e5dff7ece4f52260e2f61abd\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/CLAUDE.md\",\"status\":\"MET\"},{\"actual\":\"3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f\",\"expected\":\"3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/skills/huyang/SKILL.md\",\"status\":\"MET\"},{\"actual\":\"37e202ab9bd16de8fd7f776966079c71b6f554bcaa3eb152099507fad0f4a567\",\"expected\":\"37e202ab9bd16de8fd7f776966079c71b6f554bcaa3eb152099507fad0f4a567\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/skills/implementation-session/SKILL.md\",\"status\":\"MET\"},{\"actual\":\"4c46d852f6737f9c62b85b34275109c8f586b64fd60ae342a58047d35245f595\",\"expected\":\"4c46d852f6737f9c62b85b34275109c8f586b64fd60ae342a58047d35245f595\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/skills/implementation-session/references/prompt-template.md\",\"status\":\"MET\"},{\"actual\":\"c9ae9a8555c7533d2cf24a9aaa235a165da35cf3215b194902363de31e53e82f\",\"expected\":\"c9ae9a8555c7533d2cf24a9aaa235a165da35cf3215b194902363de31e53e82f\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/skills/ov-memory-curation/SKILL.md\",\"status\":\"MET\"},{\"actual\":\"e0f282759a22b73c35e45fac382d9121ea60782533411a58552ae4b0aec47453\",\"expected\":\"e0f282759a22b73c35e45fac382d9121ea60782533411a58552ae4b0aec47453\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/skills/ov-memory/SKILL.md\",\"status\":\"MET\"},{\"actual\":\"65a8b78f7ce01bf9b61ea0b89db395659ee0fdadde3111d2eb528d7b1c6292cf\",\"expected\":\"65a8b78f7ce01bf9b61ea0b89db395659ee0fdadde3111d2eb528d7b1c6292cf\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/skills/publish-agent-tooling/SKILL.md\",\"status\":\"MET\"},{\"actual\":\"420fdd7e51cc689a1eea80db6444c2e8233f1fc657a7bc2d99ec2fa8ea25181d\",\"expected\":\"420fdd7e51cc689a1eea80db6444c2e8233f1fc657a7bc2d99ec2fa8ea25181d\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/skills/t3-campaign/SKILL.md\",\"status\":\"MET\"},{\"actual\":\"fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8\",\"expected\":\"fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/skills/t3-campaign/references/executor-briefs.md\",\"status\":\"MET\"},{\"actual\":\"b3af2689fb0815f4bcdf052991a9db2d75b2a20f1c2736fc0cfc71254c59539b\",\"expected\":\"b3af2689fb0815f4bcdf052991a9db2d75b2a20f1c2736fc0cfc71254c59539b\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/skills/t3-task/SKILL.md\",\"status\":\"MET\"},{\"actual\":\"4695ba110bc471feff890f0b4c0429be09ded9824c0c1d04b6fefe075765d955\",\"expected\":\"4695ba110bc471feff890f0b4c0429be09ded9824c0c1d04b6fefe075765d955\",\"kind\":\"layout-owning-source-pin\",\"path\":\"config/claude/skills/t3-wait/SKILL.md\",\"status\":\"MET\"},{\"actual\":\"ff883da15c4877df6973bf9cada9c5fa33531bd96e4116dbc170eb3e5075d65e\",\"expected\":\"ff883da15c4877df6973bf9cada9c5fa33531bd96e4116dbc170eb3e5075d65e\",\"kind\":\"layout-imported-owner-pin\",\"path\":\"owner.json\",\"status\":\"MET\"},{\"actual\":\"eb4007baaf0592dc7c103c759793c1313b8a6d6650668cf733ed9cbe7633e8aa\",\"expected\":\"eb4007baaf0592dc7c103c759793c1313b8a6d6650668cf733ed9cbe7633e8aa\",\"kind\":\"layout-policy-pin\",\"path\":\"inputs/route-policy.json\",\"status\":\"MET\"},{\"actual\":\"fd02a30aacfbd1f6dda95f429d6e970385e6bd0ca7dce722e3d3da87acefc3f5\",\"expected\":\"fd02a30aacfbd1f6dda95f429d6e970385e6bd0ca7dce722e3d3da87acefc3f5\",\"kind\":\"layout-global-policy-pin\",\"path\":\"guidance/required-global-policy.json\",\"status\":\"MET\"},{\"actual\":\"ae511a13e2cb9e8b9967824a54f2c88bcb1799a6aa83bc760821bf7f91186e9a\",\"expected\":\"ae511a13e2cb9e8b9967824a54f2c88bcb1799a6aa83bc760821bf7f91186e9a\",\"kind\":\"layout-guidance-manifest-pin\",\"path\":\"guidance/manifest.json\",\"status\":\"MET\"},{\"actual\":\"6c0d6ae65c7aebda0d4785115841e7cf2e65e3ddd2a71485b71cce0e488fa2d4\",\"expected\":\"6c0d6ae65c7aebda0d4785115841e7cf2e65e3ddd2a71485b71cce0e488fa2d4\",\"kind\":\"retained-operational-reference-pin\",\"path\":\"operational_reference\",\"status\":\"MET\"},{\"actual\":\"6c0d6ae65c7aebda0d4785115841e7cf2e65e3ddd2a71485b71cce0e488fa2d4\",\"expected\":\"6c0d6ae65c7aebda0d4785115841e7cf2e65e3ddd2a71485b71cce0e488fa2d4\",\"kind\":\"global-operational-reference-pin\",\"path\":\"operational_reference\",\"status\":\"MET\"},{\"actual\":\"3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f\",\"expected\":\"3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f\",\"kind\":\"huyang-package-owning-source-pin\",\"path\":\"shared/agent-instructions/config/claude/skills/huyang/SKILL.md\",\"status\":\"MET\"}]"

func baselineContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if parent == nil {
		return nil, nil, errors.New("baseline context required")
	}
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(parent, catalogContextLimit)
	return ctx, cancel, nil
}

// baselineArchive validates the complete finite data-only closure in memory.
// No member is materialized or executed. Fixed compressed identities are owning
// constants; the manifest is checked independently rather than treated as approval.
func baselineArchive(ctx context.Context, raw []byte, pin, manifest string, count int) (map[string][]byte, error) {
	if len(raw) > 2<<20 || digestOf(raw) != pin {
		return nil, errors.New("baseline approved archive identity")
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	expanded := &io.LimitedReader{R: gz, N: (8 << 20) + 1}
	tr := tar.NewReader(expanded)
	data := map[string][]byte{}
	modes := map[string]int64{}
	total := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(data) >= 512 || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > 1<<20 || (h.Mode != 0644 && (manifest != "file-manifest.json" || h.Mode != 0600)) || !baselineSafeName(h.Name) || data[h.Name] != nil {
			return nil, fmt.Errorf("baseline archive member type/name/size/mode/duplicate: %q type=%d bytes=%d mode=%o", h.Name, h.Typeflag, h.Size, h.Mode)
		}
		for k := range h.PAXRecords {
			if k != "mtime" {
				return nil, errors.New("baseline archive extended metadata")
			}
		}
		if h.Linkname != "" || len(h.Xattrs) != 0 {
			return nil, errors.New("baseline archive link/extended attributes")
		}
		total += h.Size
		if total > 8<<20 {
			return nil, errors.New("baseline archive expanded bound")
		}
		b, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil || int64(len(b)) != h.Size {
			return nil, errors.New("baseline archive member read")
		}
		// Empty members must also have a non-nil presence sentinel.
		data[h.Name] = append([]byte{}, b...)
		modes[h.Name] = h.Mode
	}
	if _, err := io.Copy(io.Discard, expanded); err != nil {
		return nil, err
	}
	if expanded.N == 0 {
		return nil, errors.New("baseline archive expanded stream bound")
	}
	if len(data) != count {
		return nil, errors.New("baseline archive exact closure")
	}
	var records []any
	v, err := baselineDecode(data[manifest])
	if err != nil {
		return nil, err
	}
	if manifest == "file-manifest.json" {
		root, ok := v.(map[string]any)
		if !ok || !baselineKeys(root, "excluded", "files", "member_rule", "schema", "schema_version") {
			return nil, errors.New("baseline drafts manifest keys")
		}
		records, ok = root["files"].([]any)
		if !ok {
			return nil, errors.New("baseline drafts manifest records")
		}
	} else {
		var ok bool
		records, ok = v.([]any)
		if !ok {
			return nil, errors.New("baseline evidence manifest records")
		}
	}
	if len(records) != count-1 {
		return nil, errors.New("baseline manifest record count")
	}
	seen := map[string]bool{}
	for _, r := range records {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, errors.New("baseline manifest record")
		}
		if manifest == "file-manifest.json" {
			if !baselineKeys(m, "bytes", "mode", "path", "sha256") {
				return nil, errors.New("baseline manifest mode/keys")
			}
		} else if !baselineKeys(m, "bytes", "path", "sha256") {
			return nil, errors.New("baseline manifest keys")
		}
		path, ok := m["path"].(string)
		if !ok || !baselineSafeName(path) || path == manifest || seen[path] {
			return nil, errors.New("baseline manifest path/duplicate")
		}
		seen[path] = true
		if manifest == "file-manifest.json" && m["mode"] != json.Number(strconv.FormatInt(modes[path], 10)) {
			return nil, errors.New("baseline manifest exact recorded mode")
		}
		b, exists := data[path]
		if !exists || m["bytes"] != json.Number(strconv.Itoa(len(b))) || m["sha256"] != digestOf(b) {
			return nil, errors.New("baseline manifest size/hash binding")
		}
	}
	return data, ctx.Err()
}
func baselineSafeName(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\\x00") {
		return false
	}
	for _, c := range strings.Split(path, "/") {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	return true
}
func baselineKeys(m map[string]any, keys ...string) bool {
	if len(m) != len(keys) {
		return false
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

// Duplicate-safe decoding also bounds depth/containers. Retained physical
// receipts are not canonical audit identity and may use their original format.
func baselineDecode(raw []byte) (any, error) {
	if len(raw) == 0 || len(raw) > 262144 {
		return nil, errors.New("baseline JSON byte bound")
	}
	if e := boundedSharedJSON(raw, 12, 1024); e != nil {
		return nil, e
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > 12 {
			return nil, errors.New("baseline JSON depth")
		}
		t, e := d.Token()
		if e != nil {
			return nil, e
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				m := map[string]any{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return nil, e
					}
					s, ok := k.(string)
					if !ok {
						return nil, errors.New("baseline JSON key")
					}
					if _, ok := m[s]; ok {
						return nil, errors.New("baseline duplicate JSON key")
					}
					if len(m) >= 1024 {
						return nil, errors.New("baseline JSON container")
					}
					v, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					m[s] = v
				}
				_, e = d.Token()
				return m, e
			case '[':
				a := []any{}
				for d.More() {
					if len(a) >= 1024 {
						return nil, errors.New("baseline JSON container")
					}
					v, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					a = append(a, v)
				}
				_, e = d.Token()
				return a, e
			}
			return nil, errors.New("baseline JSON delimiter")
		}
		return t, nil
	}
	v, e := read(0)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); !errors.Is(e, io.EOF) {
		return nil, errors.New("baseline JSON trailing value")
	}
	return v, nil
}
func baselineCustody() *snapshotCustody {
	return &snapshotCustody{dirs: map[string]*snapshotDirectory{}}
}
func baselineHold(c *snapshotCustody, path string, limit int64, mode uint32) ([]byte, error) {
	if !sharedPhysicalPath(path) {
		return nil, errors.New("baseline physical absolute path")
	}
	fd, e := sharedHeld(path, false)
	if e != nil {
		return nil, e
	}
	var st syscall.Stat_t
	e = syscall.Fstat(fd, &st)
	syscall.Close(fd)
	if e != nil || st.Size <= 0 || st.Size > limit || st.Blocks*512 < st.Size {
		return nil, errors.New("baseline physical size/sparse file")
	}
	if e := c.directory(filepath.Dir(path), "", true); e != nil {
		return nil, e
	}
	return c.file(path, snapshotSpec{mode: mode, limit: limit}, "")
}
func baselineVerifyCustody(ctx context.Context, c *snapshotCustody) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := c.verifyDirectories(); e != nil {
		return e
	}
	for _, check := range c.checks {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := check(); e != nil {
			return catalogContextErr(ctx, e)
		}
	}
	return ctx.Err()
}
func AcquireCatalogGeneratedBaselineSnapshot(parent context.Context, archive, review string) (*CatalogGeneratedBaselineSnapshot, error) {
	ctx, cancel, e := baselineContext(parent)
	if e != nil {
		return nil, e
	}
	defer cancel()
	if archive == review || snapshotOverlap(archive, review) {
		return nil, errors.New("baseline archive/review alias")
	}
	c := baselineCustody()
	ok := false
	defer func() {
		if !ok {
			c.release()
		}
	}()
	raw, e := baselineHold(c, archive, 2<<20, 0400)
	if e != nil {
		return nil, e
	}
	proof, e := baselineHold(c, review, 65536, 0400)
	if e != nil {
		return nil, e
	}
	if digestOf(proof) != baselineReviewSHA {
		return nil, errors.New("baseline independently reviewed proof identity")
	}
	data, e := baselineArchive(ctx, raw, baselineEvidenceSHA, "evidence-file-manifest.json", 268)
	if e != nil {
		return nil, e
	}
	if e := baselineGeneratedRelationships(ctx, data); e != nil {
		return nil, e
	}
	data["@archive"] = raw
	data["@review"] = proof
	var resources []catalogResource
	for _, path := range []string{archive, review} {
		r, e := c.resource(path, false)
		if e != nil {
			return nil, e
		}
		resources = append(resources, r)
	}
	s := &CatalogGeneratedBaselineSnapshot{owner: baselineOwner{initialized: true, data: data, resources: resources, resourceCheck: baselineBindResources(resources), verify: func(ctx context.Context) error { return baselineVerifyCustody(ctx, c) }, release: c.release}}
	if e := s.owner.verifyLocked(ctx); e != nil {
		return nil, e
	}
	ok = true
	return s, nil
}
func AcquireCatalogHistoricalTemplateSnapshot(parent context.Context, archive string) (*CatalogHistoricalTemplateSnapshot, error) {
	ctx, cancel, e := baselineContext(parent)
	if e != nil {
		return nil, e
	}
	defer cancel()
	c := baselineCustody()
	ok := false
	defer func() {
		if !ok {
			c.release()
		}
	}()
	raw, e := baselineHold(c, archive, 2<<20, 0400)
	if e != nil {
		return nil, e
	}
	data, e := baselineArchive(ctx, raw, baselineDraftsSHA, "file-manifest.json", 104)
	if e != nil {
		return nil, e
	}
	if len(data["history/templates/discovery.txt"]) != 39340 || digestOf(data["history/templates/discovery.txt"]) != baselineHistoricalSHA || len(data["templates/discovery.txt"]) != 3297 || digestOf(data["templates/discovery.txt"]) != baselineCurrentSHA {
		return nil, errors.New("baseline complete historical/current template identity")
	}
	data["@archive"] = raw
	r, e := c.resource(archive, false)
	if e != nil {
		return nil, e
	}
	resources := []catalogResource{r}
	s := &CatalogHistoricalTemplateSnapshot{owner: baselineOwner{initialized: true, data: data, resources: resources, resourceCheck: baselineBindResources(resources), verify: func(ctx context.Context) error { return baselineVerifyCustody(ctx, c) }, release: c.release}}
	if e := s.owner.verifyLocked(ctx); e != nil {
		return nil, e
	}
	ok = true
	return s, nil
}

// baselineGit has finite command/pipe bounds and synchronously kills and reaps
// on overflow or cancellation. It never invokes archived programs or network.
func (op *baselineOriginalOperation) git(parent context.Context, root string, args ...string) ([]byte, error) {
	if !baselineGitGrammar(args) {
		return nil, errors.New("baseline Git command grammar")
	}
	if len(args) > 0 && args[0] == "cat-file" {
		if err := op.verifyPhysical(parent); err != nil {
			return nil, err
		}
		if op.objectRead != nil {
			op.objectRead("typed")
		}
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	argv := []string{"--no-lazy-fetch", "--no-replace-objects", "--literal-pathspecs", "--git-dir=" + root, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.useReplaceRefs=false", "-c", "protocol.allow=never", "-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never", "-c", "credential.helper="}
	cmd := exec.CommandContext(ctx, "/usr/bin/git", append(argv, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_ALLOW_PROTOCOL=", "GIT_NO_LAZY_FETCH=1", "GIT_PROTOCOL_FROM_USER=0"}
	out, stderr := &baselineOutput{limit: 1 << 20}, &baselineOutput{limit: 1 << 20}
	kill := func() { cancel() }
	out.overflow = kill
	stderr.overflow = kill
	cmd.Stdout = out
	cmd.Stderr = stderr
	if e := cmd.Start(); e != nil {
		return nil, catalogContextErr(parent, e)
	}
	if op.afterStart != nil {
		op.afterStart(cmd, append([]string(nil), args...))
	}
	e := cmd.Wait()
	if op.afterWait != nil {
		op.afterWait(cmd, append([]string(nil), args...))
	}
	if e != nil {
		return nil, catalogContextErr(parent, e)
	}
	if out.exceeded || stderr.exceeded || stderr.buffer.Len() != 0 {
		return nil, errors.New("baseline Git overflow/stderr")
	}
	return bytes.Clone(out.buffer.Bytes()), parent.Err()
}

type baselineOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow func()
	exceeded bool
}

func (b *baselineOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.exceeded = true
		b.overflow()
		return 0, errors.New("baseline Git bounded output")
	}
	return b.buffer.Write(p)
}

// Hooks are private to one initial acquisition. No executable/runner injection
// or mutable global exists. Retained verification creates a fresh hook-free op.
type baselineOriginalOperation struct {
	custody       *snapshotCustody
	contentChecks []func(context.Context) error
	beforeBarrier func()
	objectRead    func(string)
	afterStart    func(*exec.Cmd, []string)
	afterWait     func(*exec.Cmd, []string)
}

func (op *baselineOriginalOperation) verifyPhysical(ctx context.Context) error {
	if err := baselineVerifyCustody(ctx, op.custody); err != nil {
		return err
	}
	for _, check := range op.contentChecks {
		if err := baselineVerifyCustody(ctx, op.custody); err != nil {
			return err
		}
		if op.objectRead != nil {
			op.objectRead("compressed")
		}
		if err := check(ctx); err != nil {
			return catalogContextErr(ctx, err)
		}
	}
	return baselineVerifyCustody(ctx, op.custody)
}
func baselineGitGrammar(args []string) bool {
	if len(args) == 1 && args[0] == "--version" {
		return true
	}
	if strings.Join(args, "\x00") == "config\x00--local\x00--no-includes\x00--list\x00-z" ||
		strings.Join(args, "\x00") == "for-each-ref\x00--format=%(refname)\x00refs/replace/" ||
		strings.Join(args, "\x00") == "rev-parse\x00--verify\x00HEAD" {
		return true
	}
	if len(args) != 3 || args[0] != "cat-file" {
		return false
	}
	if args[1] != "commit" && args[1] != "tree" && args[1] != "blob" {
		return false
	}
	if len(args[2]) != 40 {
		return false
	}
	for _, c := range args[2] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Version probe also passes --no-lazy-fetch: unsupported Git fails closed
// before reading any store data. The qualified floor is Git 2.48.
func (op *baselineOriginalOperation) supported(ctx context.Context, root string) error {
	b, err := op.git(ctx, root, "--version")
	if err != nil {
		return err
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(string(b), "git version %d.%d.%d", &major, &minor, &patch); err != nil || major != 2 || minor < 48 {
		return errors.New("baseline unsupported Git no-lazy-fetch version")
	}
	return nil
}

// Original stores retain their separate 512-name aggregate bound rather than
// borrowing the tighter frozen catalog directory bound.
func baselineOriginalNames(c *snapshotCustody, path string) ([]string, error) {
	d := c.dirs[path]
	if d == nil {
		return nil, errors.New("baseline directory not held")
	}
	fd, err := syscall.Open("/proc/self/fd/"+strconv.Itoa(d.fd), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "baseline held directory")
	defer file.Close()
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || !sharedSame(st, d.stat) {
		return nil, errors.New("baseline enumeration binding")
	}
	names, err := file.Readdirnames(513)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > 512 || syscall.Fstat(fd, &st) != nil || !sharedSame(st, d.stat) {
		return nil, errors.New("baseline enumeration bound/change")
	}
	sort.Strings(names)
	return names, nil
}

type baselineMetadataFile struct {
	path     string
	fd, proc int
	stat     syscall.Stat_t
}

func (f baselineMetadataFile) verify() error {
	other, err := sharedHeld(f.path, false)
	if err != nil {
		return err
	}
	defer syscall.Close(other)
	var held, current syscall.Stat_t
	if syscall.Fstat(f.fd, &held) != nil || syscall.Fstat(other, &current) != nil || !sharedSame(f.stat, held) || !sharedSame(f.stat, current) || held.Nlink != 1 || current.Nlink != 1 {
		return errors.New("baseline metadata file custody changed")
	}
	return nil
}
func (f baselineMetadataFile) read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.verify(); err != nil {
		return nil, err
	}
	fd, err := syscall.Openat(f.proc, strconv.Itoa(f.fd), syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "baseline held data")
	defer file.Close()
	var opened syscall.Stat_t
	if syscall.Fstat(fd, &opened) != nil || !sharedSame(f.stat, opened) {
		return nil, errors.New("baseline data descriptor binding")
	}
	raw, err := io.ReadAll(io.NewSectionReader(file, 0, f.stat.Size+1))
	if err != nil {
		return nil, err
	}
	if syscall.Fstat(fd, &opened) != nil || !sharedSame(f.stat, opened) {
		return nil, errors.New("baseline data descriptor changed")
	}
	if int64(len(raw)) != f.stat.Size {
		return nil, errors.New("baseline bounded file changed")
	}
	if err := f.verify(); err != nil {
		return nil, err
	}
	return raw, ctx.Err()
}
func AcquireCatalogOriginalImportSnapshot(parent context.Context, root string) (*CatalogOriginalImportSnapshot, error) {
	return acquireCatalogOriginalImport(parent, root, &baselineOriginalOperation{})
}
func acquireCatalogOriginalImport(parent context.Context, root string, op *baselineOriginalOperation) (_ *CatalogOriginalImportSnapshot, err error) {
	ctx, cancel, e := baselineContext(parent)
	if e != nil {
		return nil, e
	}
	defer cancel()
	defer func() {
		if err != nil {
			err = catalogContextErr(ctx, err)
		}
	}()
	if !sharedPhysicalPath(root) {
		return nil, errors.New("baseline original physical root")
	}
	c := baselineCustody()
	op.custody = c
	ok := false
	defer func() {
		if !ok {
			c.release()
		}
	}()
	if e := c.directory(root, root, true); e != nil {
		return nil, e
	}
	if e := op.supported(ctx, root); e != nil {
		return nil, e
	}
	proc, e := syscall.Open("/proc/self/fd", sharedOPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	c.closes = append(c.closes, func() { syscall.Close(proc) })
	var fs syscall.Statfs_t
	var pst syscall.Stat_t
	if syscall.Fstatfs(proc, &fs) != nil || fs.Type != 0x9fa0 || syscall.Fstat(proc, &pst) != nil || pst.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return nil, errors.New("baseline procfs unavailable")
	}
	count, total := 0, int64(0)
	var files []baselineMetadataFile
	// Enumerate names only through held descriptors, capturing no object data.
	var walk func(string) error
	walk = func(path string) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		count++
		if count > 512 {
			return errors.New("baseline bare-store finite names")
		}
		fd, e := sharedHeld(path, true)
		if e != nil {
			return e
		}
		syscall.Close(fd)
		if e := c.directory(path, "", true); e != nil {
			return e
		}
		if c.dirs[path].stat.Uid != uint32(os.Getuid()) {
			return errors.New("baseline directory owner")
		}
		names, e := baselineOriginalNames(c, path)
		if e != nil {
			return e
		}
		captured := append([]string(nil), names...)
		c.checks = append(c.checks, func() error {
			now, e := baselineOriginalNames(c, path)
			if e != nil {
				return e
			}
			if strings.Join(now, "\x00") != strings.Join(captured, "\x00") {
				return errors.New("baseline store names changed")
			}
			return nil
		})
		for _, name := range names {
			p := filepath.Join(path, name)
			if strings.HasPrefix(p, root+"/objects/pack/") && strings.HasSuffix(p, ".promisor") {
				return errors.New("baseline promisor storage")
			}
			st, e := os.Lstat(p)
			if e != nil {
				return e
			}
			if st.IsDir() {
				if e := walk(p); e != nil {
					return e
				}
				continue
			}
			count++
			if count > 512 || !st.Mode().IsRegular() {
				return errors.New("baseline bare-store names/special")
			}
			fd, e := sharedHeld(p, false)
			if e != nil {
				return e
			}
			var stat syscall.Stat_t
			if syscall.Fstat(fd, &stat) != nil || stat.Nlink != 1 || stat.Uid != uint32(os.Getuid()) ||
				(stat.Mode&07777 != 0444 && stat.Mode&07777 != 0644 && stat.Mode&07777 != 0755) ||
				stat.Size <= 0 || stat.Size > 1<<20 || stat.Blocks*512 < stat.Size {
				syscall.Close(fd)
				return errors.New("baseline metadata file mode/owner/links/size")
			}
			total += stat.Size
			if total > 8<<20 {
				syscall.Close(fd)
				return errors.New("baseline bare-store finite bytes")
			}
			f := baselineMetadataFile{p, fd, proc, stat}
			files = append(files, f)
			c.closes = append(c.closes, func() { syscall.Close(fd) })
			c.checks = append(c.checks, f.verify)
		}
		return nil
	}
	if e := walk(root); e != nil {
		return nil, e
	}
	if e := baselineVerifyCustody(ctx, c); e != nil {
		return nil, e
	}
	if e := op.configuration(ctx, root); e != nil {
		return nil, e
	}
	if op.beforeBarrier != nil {
		op.beforeBarrier()
	}
	// Aggregate metadata/config/ref/name barrier before ANY compressed/typed data.
	if e := baselineVerifyCustody(ctx, c); e != nil {
		return nil, e
	}
	for _, f := range files {
		if e := baselineVerifyCustody(ctx, c); e != nil {
			return nil, e
		}
		if strings.HasPrefix(f.path, root+"/objects/") && op.objectRead != nil {
			op.objectRead("compressed")
		}
		b, e := f.read(ctx)
		if e != nil {
			return nil, e
		}
		want := digestOf(b)
		op.contentChecks = append(op.contentChecks, func(current context.Context) error {
			raw, e := f.read(current)
			if e != nil {
				return e
			}
			if digestOf(raw) != want {
				return errors.New("baseline store bytes changed")
			}
			return nil
		})
	}
	// Content checks receive the current operation context; the acquisition
	// context and initial observation hooks are never retained.
	contentChecks := append([]func(context.Context) error(nil), op.contentChecks...)
	verify := func(current context.Context) error {
		fresh := &baselineOriginalOperation{custody: c, contentChecks: contentChecks}
		if e := fresh.verifyPhysical(current); e != nil {
			return e
		}
		if e := fresh.configuration(current, root); e != nil {
			return e
		}
		if e := fresh.verifyPhysical(current); e != nil {
			return e
		}
		if e := fresh.objects(current, root, nil); e != nil {
			return e
		}
		return fresh.verifyPhysical(current)
	}
	data := map[string][]byte{}
	if e := baselineVerifyCustody(ctx, c); e != nil {
		return nil, e
	}
	if e := op.objects(ctx, root, data); e != nil {
		return nil, e
	}
	r, e := c.resource(root, true)
	if e != nil {
		return nil, e
	}
	s := &CatalogOriginalImportSnapshot{owner: baselineOwner{initialized: true, data: data, resources: []catalogResource{r}, resourceCheck: baselineBindResources([]catalogResource{r}), verify: verify, release: c.release}}
	if e := s.owner.verifyLocked(ctx); e != nil {
		return nil, e
	}
	ok = true
	return s, nil
}
func (op *baselineOriginalOperation) configuration(ctx context.Context, root string) error {
	for _, p := range []string{"commondir", "objects/info/alternates", "objects/info/http-alternates", "info/grafts"} {
		if _, e := os.Lstat(filepath.Join(root, p)); !errors.Is(e, os.ErrNotExist) {
			return errors.New("baseline indirect Git store")
		}
	}
	config, e := op.git(ctx, root, "config", "--local", "--no-includes", "--list", "-z")
	if e != nil {
		return e
	}
	origin, bare := 0, false
	for _, row := range strings.Split(string(config), "\x00") {
		if row == "" {
			continue
		}
		k, v, ok := strings.Cut(row, "\n")
		if !ok {
			return errors.New("baseline config encoding")
		}
		switch k {
		case "core.repositoryformatversion":
			if v != "0" {
				return errors.New("baseline Git format")
			}
		case "core.bare":
			if v != "true" {
				return errors.New("baseline non-bare")
			}
			bare = true
		case "core.filemode":
			if v != "true" && v != "false" {
				return errors.New("baseline filemode")
			}
		case "remote.origin.url":
			if v != "https://github.com/iryzhkov/omarchy-setup.git" {
				return errors.New("baseline original origin")
			}
			origin++
		case "remote.origin.fetch":
			if v != "+refs/heads/*:refs/remotes/origin/*" {
				return errors.New("baseline remote fetch config")
			}
		default:
			return errors.New("baseline unsupported Git configuration")
		}
	}
	if origin != 1 || !bare {
		return errors.New("baseline exact bare/origin")
	}
	refs, e := op.git(ctx, root, "for-each-ref", "--format=%(refname)", "refs/replace/")
	if e != nil || len(refs) != 0 {
		return errors.New("baseline replacements")
	}
	// Selected HEAD identity is ref metadata: it is refused here, before the
	// aggregate barrier and any compressed or typed object read, and again
	// before typed traversal in objects.
	if e := op.selectedHead(ctx, root); e != nil {
		return e
	}
	return ctx.Err()
}

// selectedHead resolves HEAD to an object ID without reading object data.
func (op *baselineOriginalOperation) selectedHead(ctx context.Context, root string) error {
	head, e := op.git(ctx, root, "rev-parse", "--verify", "HEAD")
	if e != nil || string(head) != sharedUpstreamCommit+"\n" {
		return catalogContextErr(ctx, errors.New("baseline selected HEAD"))
	}
	return nil
}
func (op *baselineOriginalOperation) objects(ctx context.Context, root string, data map[string][]byte) error {
	if e := op.selectedHead(ctx, root); e != nil {
		return e
	}
	raw, e := op.git(ctx, root, "cat-file", "commit", sharedUpstreamCommit)
	if e != nil || snapshotObjectID("commit", raw) != sharedUpstreamCommit || !bytes.HasPrefix(raw, []byte("tree "+sharedUpstreamTree+"\n")) {
		return catalogContextErr(ctx, errors.New("baseline original typed commit"))
	}
	trees := map[string][]byte{}
	var tree func(string) ([]byte, error)
	tree = func(id string) ([]byte, error) {
		if b, ok := trees[id]; ok {
			return b, nil
		}
		b, e := op.git(ctx, root, "cat-file", "tree", id)
		if e != nil || snapshotObjectID("tree", b) != id {
			return nil, catalogContextErr(ctx, errors.New("baseline original typed tree"))
		}
		trees[id] = b
		return b, nil
	}
	for _, r := range baselineLineages() {
		id := sharedUpstreamTree
		parts := strings.Split(r.Original.Path, "/")
		for i, name := range parts {
			b, e := tree(id)
			if e != nil {
				return e
			}
			entries, e := baselineTree(b)
			if e != nil {
				return e
			}
			row, ok := entries[name]
			if !ok {
				return errors.New("baseline original ancestor/path")
			}
			if i < len(parts)-1 {
				if row.mode != "40000" {
					return errors.New("baseline original ancestor type")
				}
				id = row.id
			} else if row.mode != r.Original.Mode || row.id != r.Original.Object {
				return errors.New("baseline original mode/blob")
			}
		}
		b, e := op.git(ctx, root, "cat-file", "blob", r.Original.Object)
		if e != nil || snapshotObjectID("blob", b) != r.Original.Object || len(b) != r.Original.Bytes || digestOf(b) != r.Original.SHA {
			return catalogContextErr(ctx, errors.New("baseline original typed blob bytes"))
		}
		if data != nil {
			data[r.Original.Path] = b
		}
	}
	if len(trees) != 15 {
		return errors.New("baseline exact ancestor tree count")
	}
	return baselineVerifyContext(ctx)
}
func baselineVerifyContext(ctx context.Context) error { return ctx.Err() }

func baselineObject(raw []byte) map[string]any {
	v, e := canonjson.DecodeObject(raw)
	if e != nil {
		return nil
	}
	return v
}
func baselineGeneratedRelationships(ctx context.Context, data map[string][]byte) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	provenance := baselineObject(data["source-provenance.json"])
	if provenance["commit"] != "33e8e9106f290b4c9c2519b968db596c0beef336" || provenance["tree"] != "fa33bb06b71896d5651404c77dd7808e83b39c60" || provenance["archive_sha256"] != "2527ed763caed5f21b5362584c43d22822369f1b0cfa0a2678787591aad71162" {
		return errors.New("baseline retained imported provenance")
	}
	policy := data["retained-data/inputs/route-policy.json"]
	if digestOf(policy) != "eb4007baaf0592dc7c103c759793c1313b8a6d6650668cf733ed9cbe7633e8aa" {
		return errors.New("baseline explicit route policy")
	}
	paths := PolicyInstructionPaths()
	sort.Strings(paths)
	if len(paths) != 18 {
		return errors.New("baseline output path inventory")
	}
	for run := 0; run < 2; run++ {
		prefix := fmt.Sprintf("generated/run-%d/", run)
		count := 0
		total := 0
		for p, b := range data {
			if strings.HasPrefix(p, prefix) {
				count++
				total += len(b)
			}
		}
		if count != 18 || total > 2<<20 {
			return errors.New("baseline actual output aggregate/inventory")
		}
		v, e := baselineDecode(data[fmt.Sprintf("generated/run-%d-manifest.json", run)])
		if e != nil {
			return e
		}
		rows, ok := v.([]any)
		if !ok || len(rows) != 18 {
			return errors.New("baseline output receipts")
		}
		for i, path := range paths {
			r, ok := rows[i].(map[string]any)
			if !ok || !baselineKeys(r, "bytes", "dev", "ino", "mode", "mtime_ns", "nlink", "path", "relative", "sha256", "type", "uid") {
				return errors.New("baseline output receipt shape")
			}
			raw, exists := data[prefix+path]
			if !exists || len(raw) > 65536 || r["relative"] != path || r["type"] != "regular" || r["mode"] != json.Number("420") || r["nlink"] != json.Number("1") || r["bytes"] != json.Number(strconv.Itoa(len(raw))) || r["sha256"] != digestOf(raw) {
				return errors.New("baseline actual output receipt binding")
			}
			if !bytes.Equal(raw, data["expected/baseline/"+path]) || !bytes.Equal(raw, data["generated/run-0/"+path]) {
				return errors.New("baseline actual relocation/expected bytes")
			}
		}
	}
	receipts, e := baselineDecode(data["receipts.json"])
	if e != nil {
		return e
	}
	rows, ok := receipts.([]any)
	if !ok {
		return errors.New("baseline raw receipts")
	}
	generators := 0
	seenGenerators := map[string]bool{}
	for _, row := range rows {
		r, ok := row.(map[string]any)
		if !ok {
			return errors.New("baseline command receipt")
		}
		label, ok := r["label"].(string)
		if !ok {
			return errors.New("baseline receipt label")
		}
		for _, stream := range []string{"stdout", "stderr"} {
			m, ok := r[stream].(map[string]any)
			if !ok {
				return errors.New("baseline receipt stream")
			}
			b, exists := data["commands/"+label+"."+stream]
			if !exists || m["bytes"] != json.Number(strconv.Itoa(len(b))) || m["sha256"] != digestOf(b) {
				return errors.New("baseline command captured stream")
			}
		}
		if strings.HasPrefix(label, "generator-install-") || strings.HasPrefix(label, "generator-check-") {
			if seenGenerators[label] || (label != "generator-install-0" && label != "generator-install-1" && label != "generator-check-0" && label != "generator-check-1") {
				return errors.New("baseline exact generator label inventory")
			}
			seenGenerators[label] = true
			if r["exit"] != json.Number("0") || r["termination"] != nil || len(data["commands/"+label+".stderr"]) != 0 {
				return errors.New("baseline generator receipt failure")
			}
			generators++
		}
	}
	if generators != 4 {
		return errors.New("baseline actual generator receipts")
	}
	checks, e := baselineDecode(data["source-input-rechecks.json"])
	if e != nil {
		return e
	}
	rechecks, ok := checks.([]any)
	if !ok || len(rechecks) != 2 {
		return errors.New("baseline source/policy rechecks")
	}
	for _, row := range rechecks {
		r, ok := row.(map[string]any)
		if !ok {
			return errors.New("baseline source recheck")
		}
		before, _ := canonjson.MarshalFile(r["source_before"])
		after, _ := canonjson.MarshalFile(r["source_after"])
		pb, _ := canonjson.MarshalFile(r["policy_before"])
		pa, _ := canonjson.MarshalFile(r["policy_after"])
		if !bytes.Equal(before, after) || !bytes.Equal(pb, pa) {
			return errors.New("baseline generated source/policy changed")
		}
		sources, ok := r["source_before"].([]any)
		if !ok || len(sources) != 14 {
			return errors.New("baseline source14 recheck")
		}
		seenSources := map[string]bool{}
		for _, x := range sources {
			m, ok := x.(map[string]any)
			if !ok || m["nlink"] != json.Number("1") || m["type"] != "regular" {
				return errors.New("baseline source receipt custody")
			}
			relative, ok := m["relative"].(string)
			if !ok || seenSources[relative] {
				return errors.New("baseline source receipt duplicate/path")
			}
			seenSources[relative] = true
			found := false
			for _, p := range baselineLineages() {
				if m["relative"] == p.Imported.Path {
					mode := json.Number("420")
					if p.Imported.Mode == "100755" {
						mode = "493"
					}
					if m["mode"] != mode || m["sha256"] != p.Imported.SHA || m["bytes"] != json.Number(strconv.Itoa(p.Imported.Bytes)) {
						return errors.New("baseline imported source receipt")
					}
					found = true
				}
			}
			if m["relative"] == "shared/agent-instructions/owner.json" && m["sha256"] == "ff883da15c4877df6973bf9cada9c5fa33531bd96e4116dbc170eb3e5075d65e" {
				found = true
			}
			if !found {
				return errors.New("baseline unknown imported source")
			}
		}
		policy, ok := r["policy_before"].(map[string]any)
		if !ok || policy["sha256"] != digestOf(data["retained-data/inputs/route-policy.json"]) || policy["nlink"] != json.Number("1") || policy["mode"] != json.Number("256") {
			return errors.New("baseline policy receipt")
		}
	}
	return ctx.Err()
}

// baselineTransform applies only the pinned finite unified literal hunks into
// an internal buffer, with exact positions, before bytes and occurrence counts.
func baselineTransform(raw []byte, diff string) ([]byte, error) {
	if diff == "" {
		return bytes.Clone(raw), nil
	}
	lines := strings.SplitAfter(string(raw), "\n")
	out := []string{}
	cursor := 0
	d := strings.Split(diff, "\n")
	for i := 2; i < len(d); {
		if d[i] == "" {
			i++
			continue
		}
		if !strings.HasPrefix(d[i], "@@ ") {
			return nil, errors.New("baseline transformation header")
		}
		fields := strings.Fields(d[i])
		startText := strings.Split(strings.TrimPrefix(fields[1], "-"), ",")[0]
		start, e := strconv.Atoi(startText)
		if e != nil || start < 1 || start-1 < cursor {
			return nil, errors.New("baseline transformation position")
		}
		start--
		out = append(out, lines[cursor:start]...)
		i++
		before, after := []string{}, []string{}
		for i < len(d) && !strings.HasPrefix(d[i], "@@ ") {
			line := d[i]
			if line == "" {
				i++
				break
			}
			switch line[0] {
			case ' ':
				before = append(before, line[1:]+"\n")
				after = append(after, line[1:]+"\n")
			case '-':
				before = append(before, line[1:]+"\n")
			case '+':
				after = append(after, line[1:]+"\n")
			default:
				return nil, errors.New("baseline literal hunk syntax")
			}
			i++
		}
		old := strings.Join(before, "")
		if start+len(before) > len(lines) || strings.Join(lines[start:start+len(before)], "") != old || bytes.Count(raw, []byte(old)) != 1 {
			return nil, errors.New("baseline exact literal hunk/count")
		}
		out = append(out, after...)
		cursor = start + len(before)
	}
	out = append(out, lines[cursor:]...)
	return []byte(strings.Join(out, "")), nil
}
func inspectCatalogBaselineLocked(ctx context.Context, c *CatalogSnapshot, o *CatalogOriginalImportSnapshot, g *CatalogGeneratedBaselineSnapshot, h *CatalogHistoricalTemplateSnapshot, guidance CatalogGuidanceAudit, producer CatalogProducerAudit) (CatalogBaselineAudit, error) {
	fail := func(e error) (CatalogBaselineAudit, error) { return CatalogBaselineAudit{}, e }
	if e := snapshotImportedOwner(c.sources["shared/agent-instructions/owner.json"]); e != nil {
		return fail(e)
	}
	layout := baselineObject(c.inputs["inputs/guidance-layout.json"])
	bindings := catalogObject(layout["bindings"])
	global := baselineObject(c.inputs["inputs/required-global-policy.json"])
	provenance := catalogObject(global["provenance"])
	huyang := baselineObject(c.inputs["inputs/tool-guidance/huyang.json"])
	models := baselineObject(g.owner.data["retained-data/inputs/model-blocks.json"])
	if !bytes.Equal(c.inputs["inputs/route-policy.json"], g.owner.data["retained-data/inputs/route-policy.json"]) || !bytes.Equal(c.inputs["inputs/guidance-layout.json"], h.owner.data["templates/discovery.txt"]) || provenance["prior_template_sha256"] != baselineHistoricalSHA {
		return fail(errors.New("baseline policy/current/historical bindings"))
	}
	if len(h.owner.data["history/templates/discovery.txt"]) != 39340 || digestOf(h.owner.data["history/templates/discovery.txt"]) != baselineHistoricalSHA {
		return fail(errors.New("baseline full historical member"))
	}
	originals, transforms := []any{}, []any{}
	for _, r := range baselineLineages() {
		if e := ctx.Err(); e != nil {
			return fail(e)
		}
		before, after := o.owner.data[r.Original.Path], c.sources[r.Imported.Path]
		if len(before) != r.Original.Bytes || digestOf(before) != r.Original.SHA || len(after) != r.Imported.Bytes || digestOf(after) != r.Imported.SHA {
			return fail(errors.New("baseline complete original/imported bytes"))
		}
		transformed, e := baselineTransform(before, r.Diff)
		if e != nil || !bytes.Equal(transformed, after) {
			return fail(errors.New("baseline exact transformation hunks"))
		}
		kind := "identity"
		if r.Diff != "" {
			kind = "literal-hunks"
		} else if r.Original.Mode != r.Imported.Mode {
			kind = "mode-only"
		}
		originals = append(originals, map[string]any{"path": r.Original.Path, "mode": r.Original.Mode, "type": "blob", "git_blob": r.Original.Object, "bytes": r.Original.Bytes, "sha256": r.Original.SHA})
		transforms = append(transforms, map[string]any{"original_path": r.Original.Path, "imported_path": r.Imported.Path, "original_sha256": r.Original.SHA, "imported_sha256": r.Imported.SHA, "original_mode": r.Original.Mode, "imported_mode": r.Imported.Mode, "kind": kind})
	}
	var comparisons []baselineComparison
	if json.Unmarshal([]byte(baselineComparisonJSON), &comparisons) != nil || len(comparisons) != 52 {
		return fail(errors.New("baseline closed comparison inventory"))
	}
	for _, r := range comparisons {
		if e := ctx.Err(); e != nil {
			return fail(e)
		}
		actual, expected := "", ""
		generated := g.owner.data["generated/run-0/"+r.Path]
		switch r.Kind {
		case "baseline-file":
			actual = digestOf(generated)
			expected = digestOf(h.owner.data["baseline/"+r.Path])
		case "layout-main-pin":
			actual = digestOf(generated)
			expected, _ = catalogObject(bindings["baseline_main_sha256"])[r.Path].(string)
		case "global-main-pin":
			actual = digestOf(generated)
			expected, _ = catalogObject(provenance["baseline_main_sha256"])[r.Path].(string)
		case "exact-model-block-inclusion", "layout-model-block-pin":
			block, ok := models[r.Path].(string)
			if !ok || bytes.Count(generated, []byte(block)) != 1 {
				return fail(errors.New("baseline complete model block inclusion"))
			}
			actual = digestOf([]byte(block))
			family := "codex"
			if r.Path == ".claude/omarchy-setup/CLAUDE.md" {
				family = "claude"
			}
			expected, _ = catalogObject(bindings["model_block_sha256"])[family].(string)
		case "huyang-help":
			actual = digestOf(generated)
			expected, _ = catalogObject(huyang["help"])["sha256"].(string)
		case "huyang-owner-evidence":
			actual = digestOf(generated)
			expected, _ = catalogObject(huyang["owner"])["evidence_sha256"].(string)
		case "layout-owning-source-pin":
			actual = digestOf(c.sources["shared/agent-instructions/"+r.Path])
			expected, _ = catalogObject(bindings["imported_source_sha256"])[r.Path].(string)
		case "layout-imported-owner-pin":
			actual = digestOf(c.sources["shared/agent-instructions/owner.json"])
			expected, _ = bindings["imported_owner_sha256"].(string)
		case "layout-policy-pin":
			actual = digestOf(c.inputs["inputs/route-policy.json"])
			expected, _ = bindings["policy_sha256"].(string)
		case "layout-global-policy-pin":
			actual = digestOf(c.inputs["inputs/required-global-policy.json"])
			expected, _ = bindings["required_global_policy_sha256"].(string)
		case "layout-guidance-manifest-pin":
			actual = digestOf(c.inputs["inputs/tool-guidance-manifest.json"])
			expected, _ = bindings["guidance_manifest_sha256"].(string)
		case "retained-operational-reference-pin", "global-operational-reference-pin":
			reference, _ := global["operational_reference"].(string)
			actual = digestOf([]byte(reference))
			expected, _ = bindings["operational_reference_sha256"].(string)
			if r.Kind == "global-operational-reference-pin" {
				expected, _ = provenance["reference_sha256"].(string)
			}
		case "huyang-package-owning-source-pin":
			actual = digestOf(c.sources[r.Path])
			expected, _ = catalogObject(huyang["owner"])["source_sha256"].(string)
		default:
			return fail(errors.New("baseline unknown comparison category"))
		}
		if actual != expected || actual != r.Actual || expected != r.Expected {
			return fail(fmt.Errorf("baseline comparison %s %s", r.Kind, r.Path))
		}
	}
	runs := []any{}
	paths := PolicyInstructionPaths()
	sort.Strings(paths)
	for i := 0; i < 2; i++ {
		outputs := []any{}
		for _, p := range paths {
			b := g.owner.data[fmt.Sprintf("generated/run-%d/%s", i, p)]
			outputs = append(outputs, map[string]any{"path": p, "mode": 0644, "bytes": len(b), "sha256": digestOf(b)})
		}
		runs = append(runs, map[string]any{"relocation_index": i, "outputs": outputs, "receipts_sha256": digestOf(g.owner.data[fmt.Sprintf("generated/run-%d-manifest.json", i)])})
	}
	historical := map[string]any{"role": "historical-template-owned-artifact", "archive_sha256": baselineDraftsSHA, "manifest_path": "file-manifest.json", "manifest_sha256": digestOf(h.owner.data["file-manifest.json"]), "member_path": "history/templates/discovery.txt", "mode": 0644, "bytes": 39340, "sha256": baselineHistoricalSHA, "current_template_sha256": baselineCurrentSHA}
	verified := append(guidance.VerifiedCategories(), producer.VerifiedCategories()...)
	verified = append(verified, "static acquired original13 lineage", "exact imported transformation", "retained imported18 generated-byte binding; two actual relocations and all52 comparisons", "historical prior-template binding")
	blockers := guidance.Blockers()
	future := append(guidance.FutureRequired(), producer.FutureRequired()...)
	future = append(future, "genuine isolated original b32 derivation; retained imported evidence is not original execution", "fresh Stage A and expected receipt match", "current candidate independent source review and exact source CI", "owning runtime interpreter and finite transitive closure; genuine parser/generator execution", "timeout/overflow/cancellation/lost-reply and cleanup fault qualification; physical hard deadlines NOT_MET", "projection/protocol/preservation/token budgets; installed help/reference/personal merge checks", "store durability/recovery/old branches and fleet gates; Stage B NOT_MET")
	generatedSection := map[string]any{"scope": "retained-isolated-imported-proof", "source_commit": "33e8e9106f290b4c9c2519b968db596c0beef336", "source_tree": "fa33bb06b71896d5651404c77dd7808e83b39c60", "evidence_sha256": baselineEvidenceSHA, "review_sha256": baselineReviewSHA, "policy_sha256": digestOf(c.inputs["inputs/route-policy.json"]), "runs": runs}
	root := map[string]any{"schema": "d1-catalog-baseline-audit/v1", "schema_version": 1, "selected_source": map[string]any{"repository": "https://github.com/iryzhkov/UpKeeper.git", "commit": c.commit, "tree": c.tree}, "original_import": map[string]any{"repository": "https://github.com/iryzhkov/omarchy-setup.git", "commit": sharedUpstreamCommit, "tree": sharedUpstreamTree, "records": originals}, "transformation": map[string]any{"id": "d1-original-b32-to-upkeeper33e8e91/v1", "records": transforms}, "imported_source": map[string]any{"commit": "33e8e9106f290b4c9c2519b968db596c0beef336", "tree": "fa33bb06b71896d5651404c77dd7808e83b39c60", "archive_sha256": "2527ed763caed5f21b5362584c43d22822369f1b0cfa0a2678787591aad71162", "owner_sha256": digestOf(c.sources["shared/agent-instructions/owner.json"])}, "generated_baseline": generatedSection, "historical_template": historical, "verified_categories": verified, "blockers": blockers, "future_required": future}
	encoding, e := canonjson.MarshalFile(root)
	if e != nil {
		return fail(e)
	}
	orig, e := canonjson.MarshalFile(originals)
	if e != nil {
		return fail(e)
	}
	gen, e := canonjson.MarshalFile(generatedSection)
	if e != nil {
		return fail(e)
	}
	hist, e := canonjson.MarshalFile(historical)
	if e != nil {
		return fail(e)
	}
	return CatalogBaselineAudit{commit: c.commit, tree: c.tree, encoding: encoding, originals: orig, generated: gen, history: hist, verified: verified, blockers: blockers, future: future}, nil
}

type baselineTreeEntry struct{ mode, id string }

func baselineTree(raw []byte) (map[string]baselineTreeEntry, error) {
	m := map[string]baselineTreeEntry{}
	for len(raw) > 0 {
		i := bytes.IndexByte(raw, 0)
		if i < 0 || len(raw) < i+21 {
			return nil, errors.New("baseline tree encoding")
		}
		mode, name, ok := strings.Cut(string(raw[:i]), " ")
		if !ok || !baselineSafeName(name) || strings.Contains(name, "/") {
			return nil, errors.New("baseline tree name")
		}
		if _, ok := m[name]; ok {
			return nil, errors.New("baseline duplicate tree name")
		}
		m[name] = baselineTreeEntry{mode, fmt.Sprintf("%x", raw[i+1:i+21])}
		raw = raw[i+21:]
	}
	return m, nil
}
