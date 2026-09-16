# Global agent instructions: moved

This file is no longer the source of the Codex instructions. The agent-config
component is legacy and captures an empty file list; the live `~/.codex/AGENTS.md`
is generated on each host by `agents-instructions-gen` from omarchy-setup's
`config/bin/agents-instructions-gen`, and reaches the fleet through the
agent-environment component, which captures the host's own file.

A copy kept here could only drift from what the hosts actually run, which it did:
it lagged the live file by a campaign section and by the hardened rule about shell
file tools inside a repository. Edit the omarchy-setup source and regenerate, then
publish with `upkeeper push`.
