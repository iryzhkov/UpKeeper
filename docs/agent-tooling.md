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

## Agent tool profiles

An inventory host may declare which agent tools it gets with `agent_tools` in
`inventory/hosts.yml`, a list drawn from `homelab-cli`, `ov-memory` and
`local-file-llm`:

```json
"agent-a": { "...": "...", "agent_tools": ["homelab-cli", "ov-memory"] }
```

Without the key a host gets every tool, exactly as before profiles existed: the
same staged release directory, bindings, MCP registrations and health checks.
An empty list, an unknown name, a name given twice, a non-string entry or an
explicit `null` is refused when the inventory is loaded, and the agent checks the
profile it receives again before it changes anything.

With a profile, the tooling component stages only the selected tools' sources
and dependencies (no `npm ci` without local-file-llm, no OV virtualenv without
ov-memory), binds only their commands, and health-checks only them, so a tool a
host does not have is never started and nothing it would contact is reached. The
agent-environment component registers only the selected tools' MCP servers in
the Claude, Codex and OpenCode configuration on that host. The release is shared
by every host, so this filtering happens per host when the agent applies it, not
when the release is captured. The staged release directory's name includes the
effective profile, so changing a profile stages afresh; naming all three tools
is the same as declaring none.

When a tool is dropped from a profile, UpKeeper removes what it made for that
tool: the command bindings in `~/.local/bin` that are symlinks to its own
target in the active bundle (`ov-memory-mcp`, `local-file-llm-mcp`, or the
homelab-cli commands), and the MCP registrations of that name that run its
wrapper in `~/.local/bin`. A binding or registration of the same name that
points anywhere else was made by someone else and is kept, as are every other
registration and the host's own credentials and formatting. A dropped tool's
registration is removed whole, including any `env` entries the host added to
it; the previous harness files stay in that run's journal under
`~/.local/state/dev-fleet/environments/<run>/prior.json`. Keep the OV key in
its key file rather than in a harness registration, so that dropping and
re-adding ov-memory never loses it.

The agent VMs agent-a and agent-b use `["homelab-cli", "ov-memory"]`. They are
on VLAN 130, which by design cannot reach the Ollama server on omarchy-pc that
local-file-llm uses, so its health check could only fail there; the firewall
stays closed and the VMs simply do not get the tool.

Credentials follow the profile. The OpenViking API key is required only on a
host whose profile includes ov-memory; it may be `OV_API_KEY`, `api_key` in
`~/.config/ov-mcp/config.toml`, the key file `~/.config/ov-mcp/key`, or the
keyring. When it is missing the component is `blocked: credential missing ...`
with the instructions to create it, before anything is staged or probed.
`secret-tool` is a prerequisite only when ov-memory is selected and the key is
none of the first three, so the keyring is the only place it could be; Debian's
`python3-venv` is a prerequisite only with ov-memory. A failed health check
reports the tool's own error text, bounded and with secrets redacted, as
`<tool> health check: tools/call <name> failed: <text>`.

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
Repository-owned skills are sourced from the pinned omarchy-setup checkout instead
of installed copies and projected to both .claude/skills and .agents/skills by
UpKeeper alone.
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
