# Global agent instructions

## OV memory

This machine's agents share a long-term memory store (OpenViking on homelab),
reached through the `ov-memory` MCP server. Search it before answering questions
about past work, infrastructure or decisions; write to it only when asked.

Full guidance, including what belongs in which namespace and how to curate the
store: **~/.config/agents/ov-memory.md**

## Reading and editing code

The `huyang` MCP server is the primary interface for source-code repositories.
Call `workspace_open` for the repository root first, retain its workspace ID and
revision, then use Huyang's semantic reads and transactional edits. Shell still runs
builds, tests, Git, and commands whose output is the point.

Full guidance, including tool mapping, revisions, transactions, and evidence:
**~/.config/agents/huyang.md**

## Local file LLM

The `local-file-llm` MCP server delegates bulk file reading and summarising to a
local model on gaming-pc (ollama at 192.168.90.212). Use it for work that is
large but not subtle: scanning many files for a pattern, first-pass summaries,
log triage. Keep judgement calls here.

## T3 steward: backlog and wait

The `t3-steward` daemon runs on every T3 host. Two things it does for agents:

- **Backlog**: work that does not need the user now is queued rather than started on
  the spot: `t3-backlog --project "<T3 project>" --title "<title>" < prompt`. It runs as
  an unattended T3 thread in the next quiet slot with quota headroom, on whichever host
  the task names (`--host`). Use it when the user says later, tonight, when I am not
  around, or backlog. The printed intake path is not proof that a workflow was created:
  verify that the intake source is accepted (it may remain as the durable idempotent source)
  and that exactly one new run appears in `t3-steward backlog list --project "<T3 project>"
  --json`. On ambiguity, inspect status and logs; never
  blindly submit a duplicate. `t3-steward backlog start` is an explicit operator override that
  bypasses quota forecast/admission/freshness/runway and automatic quota throttling through worker
  delivery. Use it only with explicit user authority and manual quota monitoring. Automatic work
  remains fenced; worker, dependency, lock, revision, and effect-safety checks still apply.
- **Wait**: never poll in a loop for something external (a PR review, CI, a long job).
  Register the check and end the turn: `t3-steward wait add --thread <T3 thread id>
  --name "<what>" -- <command>`. Exit 0 means done, exit 2 means give up, anything else
  means not yet; the steward polls with backoff and wakes the thread with the outcome.
  The thread id is in the thread URL shown by the T3 app.

Full guidance, fields, prompt-writing advice and status commands:
**~/.config/agents/t3-steward.md**
