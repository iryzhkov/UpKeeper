# Agent tooling and model discovery

The optional agent-tooling release component closes the gap between MCP
registrations and the programs they launch. It carries reviewed source bytes
for homelab-cli (including ha, toolfeedback and T3 helpers), OV memory's local
MCP client, and local-file-llm, plus exact Python requirements and npm's lockfile.
The OV server, its data and each host's credentials remain host-local.

Every normal apply runs `mise upgrade --yes --no-prune` using the host's global
version ranges and release-age settings. UpKeeper does not bump pinned ranges.
This is a floating upgrade policy: machines with different ranges or platform
support may install different versions. Codex, Claude, OpenCode and Node must
already be configured in mise. Failed upgrades stop the run before activation.

Source bundles are staged under ~/.local/share/upkeeper-tools/releases/<digest>.
OV gets a fresh virtualenv and local-file-llm gets npm ci. CLI help, Python
dependency checks, MCP tools/list, OV memory_status, local_status and Codex
model/list verify the installation without inference or memory writes.
Only a verified bundle becomes current; existing command bindings are backed
up and restored if activation fails. A failed incomplete stage is preserved
for inspection; remove that inactive directory after diagnosing the failure
before retrying. Rollback restores frozen bundles and bindings but does not
undo mise upgrades or perform another floating upgrade.

Model policy is `discover`. The release never freezes a required model catalog.
Status reports the live catalog; new or retired models do not create release
drift. Client/account failures still fail the discovery check. Steward fleet
profiles can opt a provider into all discovered models with `["*"]`, using a
Steward release that supports that policy. Only concrete models reported by
an authenticated, enabled, installed provider become advertised routes.
Empty lists still disable routes; explicit names still restrict them.
Client updates and normal provider-cache refresh make new models available
without another UpKeeper manifest release.

## Capture and rollout

Bootstrap with `upkeeper push --components agent-tooling agent-environment`
from the validated publishing host. Review the complete candidate release,
preserving unrelated pins and authored worker intent, and follow the normal
source/release CI gates before convergence. Historical releases without the
component remain valid. After the initial rollout, capture reads the active
managed bundle, avoiding accidental downgrade from stale copies in Work.
To publish newer source tools, stage and validate their intended bundle on the
publishing host before capture; dependency environments are rebuilt per host.

Agent environment capture includes personal skills in .claude/skills,
.codex/skills and .agents/skills; Codex's bundled .system skills are excluded.
Shared Codex model, reasoning, service-tier, personality, feature and TUI
settings are projected, while account/provider, project trust and hooks remain
local. Missing shared settings clear the corresponding target override.
All three harnesses receive stable MCP wrappers; public environment settings
are rebased and merged with host-local credential fields.

`upkeeper status` observes desired/release.json through the installed agents.
It never invokes apply or mise upgrade. A target with an older binary reports
observation unavailable until upgraded. JSON includes per-component drift,
installed agent CLI versions and the current Codex model catalog.
Use an adequate per-host timeout for dependency and network health probes.
