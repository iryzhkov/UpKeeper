# Unattended task: implement dev-fleet Phase 2 only

Work in the T3 project `dev-fleet development` on the host that owns
`~/Work/dev-fleet`. You have no conversation context. Do not ask questions; if authority,
evidence, or a required decision is missing, stop safely and end with
`BACKLOG STATUS: needs-input`.

Before acting, follow the machine's AGENTS.md instructions and search OV memory. Re-read in full:

- `/home/igor/Work/plans/dev-fleet-plan.md`
- `~/Work/dev-fleet/docs/discovery-2026-09-10.md`
- every repository README and document relevant to architecture, recovery, threats, and handoff
- `desired/components.yml`, inventory, tests, CI, and the complete git history/current status

Reconcile the committed claims with live read-only state. Do not assume the previous session
completed anything merely because a prompt says it did. Preserve every dirty or unexplained
worktree, installed path, config, service, active bridge/session, backlog item, wait, and
persistent-state file. Never reset, clean, stash, force-push, weaken SSH/TLS, accept a host key
automatically, expose a secret, or broaden this phase.

## Scope: Phase 2 only

Implement the t3-steward reference role and its tests. Stop before agent99, Neovim,
homelab-cli, agent configuration, reconciliation timers, promotion, or assisted repair.

Obey every plan requirement for:

- immutable source/release pins, checksums, architecture and prerequisite validation;
- strict selectors/paths/refs, explicit timeouts, sanitized output, and no command-line secrets;
- deterministic all-target lock ordering and release-on-contention;
- clean preflight, staging, canary, serial activation, health/stability observation, and rollback;
- immutable version directories and atomic symlink/unit activation;
- ownership/adoption refusal for unmanaged or locally modified paths;
- mode-preserving backup of the SQLite database plus WAL/sidecars before any migration-capable
  binary runs, integrity checks, bounded retention, and no secret/state backup in git;
- explicit schema compatibility with tested rollback or forward recovery;
- preservation of `~/.config/t3-steward`, backlog contents, waits, and
  `~/.local/state/t3-steward/state.db`;
- deterministic unit rendering, `systemd-analyze --user verify` when present, daemon reload,
  bounded restart/readiness, exact binary/service/backlog/wait verification;
- active canary choice, failure injection, no later-host activation after canary failure,
  interrupted-run resume/recovery, and rollback that proves queue/database integrity;
- no dependency on the potentially broken local steward for deterministic recovery;
- truthful check mode and idempotence, with no real-fleet deployment from CI.

Do not deploy until every Phase 0/1 blocker is demonstrably resolved, the working tree is clean,
the required checks are green, and the plan's canary/backup/rollback preconditions are met. If a
real canary is authorized by the committed state, make the smallest safe t3-steward-only canary,
capture sanitized evidence, inject the required failed version, prove rollback, then roll out
serially exactly as the plan permits. Never claim support for an untested architecture or
check-mode path.

Run the complete gate: format/lint, unit tests, Ansible syntax, truthful check mode, redaction,
idempotence, both architectures where feasible, state migration/rollback failure injection,
lock contention, lost SSH/controller interruption, service health timeout, and unavailable
Forgejo/feed. Update discovery/handoff documentation with exact sanitized evidence. Commit coherent
changes and push to `gitea`; do not bypass a protected branch or force-push.

## Continuation contract

Create `docs/prompts/phase-3-agent99.md` as a standalone unattended prompt. Queue exactly one
Phase 3 successor with `t3-backlog` only if Phase 2 is complete, every required test and live gate
passed, the tree is clean, all work and the successor prompt are committed and pushed to
`gitea`, no unresolved choice/credential/authority/dirty state/security concern/failed host
verification remains, and no same-phase task is pending, running, or needs-input in
`t3-steward backlog list --all`.

Never skip a phase, queue more than one successor, queue a duplicate, or queue after a partial,
failed, or uncertain result. Validate the existing T3 project resolves to this workspace. Confirm
the queued task path and `pending` status. Do not use `t3-run`, wait for the successor, poll it,
or change repository/hosts after queueing. If any condition is false, queue nothing and leave an
exact handoff command/file.

End with exactly one of:

- `BACKLOG STATUS: done` — Phase 2 complete but no successor is appropriate;
- `BACKLOG STATUS: continue` — Phase 2 complete and exactly one validated Phase 3 task queued;
- `BACKLOG STATUS: needs-input` — a named decision/authority/evidence blocker remains.
