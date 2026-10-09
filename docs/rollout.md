# Native binary-only rollout

`upkeeper rollout check --config rollout.json` reads tags, exact-commit CI,
remote heads, release assets, coordinator/workers, timers and the excluded
laptop's mask. It does not create files, stop services, publish or enroll.
`upkeeper rollout plan --config rollout.json` prints every step and its
rollback without executing commands. `upkeeper rollout run --config rollout.json
--yes` prints that plan, then executes and checkpoints it.

A rollout is an operator-authored maintenance window. Use a dedicated local
release checkout and journal directory, outside candidate-managed paths. The
previous release is a full immutable Git commit, not a branch or captured host.
All mutation, including resume, requires `--yes`. Start with `FleetApproval`
empty: the canary and soak run under standing authority, then the command stops
before the first fleet host. Present that evidence to the operator, record the
explicit go in `FleetApproval`, and resume with `--yes`. Authorization is saved
separately in the journal before the first fleet host and retained on later
resumes; a different nonempty approval refuses. The journal binds all operational
configuration fields; retain them unchanged throughout resume.
Use an absolute `StatePath` and `Checkout`. Prepare the isolated checkout on
its main branch, at the agreed main commit, with the configured remote names
(origin and gitea in the example) pointing to the intended release repositories.
The native command refuses detached HEAD or a feature branch because authored
push publishes the current branch. The frozen review package is written beside
the journal as `state.json.review-package.json`.

Configuration is strict JSON with the following fields (names are case
insensitive; underscores are not aliases):

```json
{
  "Version": "0.11.0-rc.119",
  "PreviousRelease": "45973ebbfa79553a0ee7caf9b5c562b5cfffa49f",
  "StewardRepo": "iryzhkov/t3-steward",
  "UpKeeperRepo": "iryzhkov/UpKeeper",
  "Checkout": "/absolute/path/to/release-checkout",
  "SetupRepository": "/absolute/path/to/omarchy-setup",
  "StatePath": "/absolute/path/to/rollout/state.json",
  "LocalHost": "omarchy-pc",
  "CoordinatorHost": "normandy",
  "Canary": "agent-b",
  "LastHost": "omarchy-pc",
  "LaptopHost": "laptop",
  "Remotes": [
    "origin",
    "gitea"
  ],
  "FleetOrder": ["normandy", "homelab", "agent-a", "omarchy-pc"],
  "TimerHosts": ["normandy", "homelab", "omarchy-pc", "laptop"],
  "DrainedHosts": ["homelab", "normandy"],
  "ActiveWorkers": ["agent-a", "agent-b", "omarchy-pc"],
  "Targets": {
    "normandy": "normandy",
    "homelab": "homelab",
    "agent-a": "igor@192.168.130.2",
    "agent-b": "igor@192.168.130.3",
    "omarchy-pc": "local",
    "laptop": "laptop"
  },
  "QuietCommand": ["/absolute/path/to/coordinator-resize-window"],
  "BackupMaxAgeSeconds": 3600,
  "ReadinessSeconds": 180,
  "SoakSeconds": 1800,
  "ReviewFile": "/absolute/path/to/review.md",
  "FleetApproval": ""
}
```

Host names and transport destinations are configuration. The canary cannot be
the coordinator; the excluded laptop cannot be converged or enrolled; active
and drained workers cannot overlap; the configured last host must be last.
Reachable laptop timers are fenced and the worker must stay masked.
Configured drained workers remain drained, even while readiness and version
checks apply to them. No all-worker enrollment or undrain is performed.

The timer fence uses the small Fence interface (Set and Check), so U1 can
replace both timer stopping and verification with native holds. The coordinator
backup uses CoordinatorBackup.CreateAndVerify, allowing the Steward-owned
backup/drill command to replace the legacy implementation. Both have explicit
production-backend injection points; the default adapters retain this release’s
reviewed behavior.
The default fence records prior timer state durably before stopping timers;
it refuses an active self-update service. Recheck fences before publication
and every host because a reboot can re-arm enabled timers. Timers remain
stopped on success and failure. Restoring the prior states is a separate
reviewed operation: the rc119 script's unconditional timer-release refusal
is preserved. Full-preview actions matter even when labelled converged,
including mise, fleet configuration, laptop updates and bootstrap effects.

The default backup implementation uses the reviewed stopped-snapshot path:
quiet predicate, both database holders stopped, WAL checkpoint and no holder,
local private snapshot, verify, restart, scratch configuration with production
identity and bindings but rewritten write paths and archive disabled, offline
restore, SQLite integrity and foreign-key checks. Overrides are scrubbed
during the drill. Backup evidence is required within an hour of the
coordinator convergence. `BackupCommand`, when configured, is an argv vector
for the future Steward-owned coordinator backup/restore-drill command. Its
successful stdout must contain key=value lines for snapshot, scratch,
restore_integrity=ok and restore_fk_violations=0, attesting the same stopped
snapshot and verified scratch restore. Do not
configure it to a verify-only command.

The authoring step freezes only the two Steward version keys, verifies
all four release archives and binary metadata, records canonical
`fleet-plan` digest and parent, and checks each scoped preview target.
The full preview is retained to disposition every effect and rc118 findings
B1–B4. Publication requires an external independent review; this command
does not submit reviews. A missing review stops with an unpublished
candidate. Read the journal/package evidence and provide a review whose
first line is `VERDICT: ACCEPT`, naming the exact parent and canonical
release digest and a line `PACKAGE <digest>`. Re-run the same command with
`--yes` after the review arrives. Changed bytes or fences refuse publication.
CI must then pass for the exact published release before any convergence.

After the idle canary, the command observes the configured soak before
walking the fleet. Each host must install the verified binary and run both
Steward services in new generations, preserve the T3 Code generation and
drains, and report readiness on the coordinator epoch. Coordinator health
waits through the observed 20–60 second reconnect window; drained hosts
are held to the same readiness gate. Epoch changes are allowed only for
the coordinator step, followed by all-worker reconnection checks.

Resume skips verified hosts. An interrupted host effect is ambiguous, so
resume rolls it back to the immutable previous release and stops rather
than repeating convergence. A failed host gate automatically rolls that
host back and records a terminal failure. Failed enrollment or final fleet
verification rolls completed hosts back in reverse order; the durable recovery
queue resumes at its interrupted host. Enrollment itself is retained for forward
recovery. A failed rollback is retried on resume. Interrupted publication requires receipt/remote reconciliation;
it is never blindly republished. Preserve the journal and snapshots.
A coordinator database incompatible with the previous binary requires
supervised restore: stop both holders, prove no open database, preserve
failed DB/sidecars/artifacts in a new private directory, restore the
immutable snapshot into absent targets, restart and verify. Enrollment
recovery is forward per-worker `--current-catalog`, not snapshot restore
merely to undo enrollment.

The reviewed operator script remains in its original bundled location.
This unit does not delete it or change its independent review procedure.

| rc119 runbook section | Native phase |
| --- | --- |
| 1 tag/build, 2 assets, 3 base/remotes | check; repeated at run preflight |
| 4 timer fence | run: fence; prior states in journal |
| 5 snapshot rehearsal, 11a binding snapshot | run: backup before coordinator |
| 6 author and previews | run: prepare |
| 7 independent release review | external review of frozen package |
| 8 authored publication and release CI | run: publish, release-ci |
| 9 canary and soak | run: canary host, soak |
| 10 operator fleet go | run: record FleetApproval after canary/soak, then resume --yes |
| 11 ordered fleet and rollback | run: one host at a time; automatic immutable rollback |
| 12 per-worker enrollment | run: enroll |
| 13 final verification | run: verify |
| 14 timer disposition, 15 fence release | separately reviewed follow-up; timers stay stopped |
| 16 ledger/feed/role-routing | operator follow-up outside this command |

Qualification uses fake command runners, hosts, remotes, GitHub, systemd
and Steward clients. No tests require a network or perform a fleet change.
The command does not settle the source gaps about migration reversibility,
version skew, workload continuity, NAS privacy or timer re-arm on reboot;
its fail-closed gates and retained recovery evidence are the boundaries.
