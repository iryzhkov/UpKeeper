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

## Publish validated agent-tooling changes

When working on agent tooling (Huyang, t3-steward, UpKeeper, agent99, shared
AGENTS.md/CLAUDE.md files, skills or MCP configuration), finish a validated fix by
publishing it through UpKeeper without waiting for a separate request to push.

Run the applicable checks, commit and push the source to its configured remotes,
and confirm the exact source commit's required GitHub CI succeeds. Publish any
required release artifact and validate the intended controller installation.
For instruction changes, edit the owning sources and regenerate derived files.
Then run `upkeeper push` from the fleet host where the intended tooling and agent
environment have been validated, using a clean, current UpKeeper checkout with
push access to the configured remotes. Normandy is not required. Capture publishes
that host's observed pins and agent environment, so review the whole candidate
release and preserve unrelated pins before publication. A Git push alone does not update the
UpKeeper release manifest. Verify the manifest diff contains only intended changes;
preserve unrelated work, worker configuration and secret references. Do not capture
unvalidated local builds or unrelated newer component versions.

Confirm GitHub CI for the published UpKeeper release commit before convergence.
Use UpKeeper for deployment; the laptop converges through its local
`upkeeper pull --self` timer. Report the release commit, validation and any pending
or blocked publication/convergence honestly. If capture or a gate is blocked,
record the precise blocker and preserve the safeguards rather than declaring the
fix distributed. An explicit user instruction to keep work local takes precedence.

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
