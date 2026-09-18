# UpKeeper push and pull

UpKeeper captures a controller-observed release and converges named inventory hosts.
Read [the system model](architecture/system-model.md) for ownership, failure semantics
and the distinction from the F03 saga library.

The [bootstrap and --self guide](self-bootstrap.md) describes the U2 local entry point.

## Release components

| Component | Capture and authority | Pull behavior |
| --- | --- | --- |
| omarchy-setup | Clean reachable main commit | Fast-forward checkout, 28-scripts only |
| agent-config | Legacy manifest file list; empty in U0 captures | Legacy file handler, retained for reading old manifests |
| agent-environment | F05 AgentEnvironmentSpec v1 and owned file projections | Stage, merge owned keys, activate, verify, whole-environment compensation |
| Huyang | Clean reachable main commit and probe expectation | Build pinned commit, restart/probe, restore previous commit on failure |
| t3-steward | Installed release version | Exact checksum-verified GitHub artifact, candidate compatibility interrogation, restart and verification |
| t3 | Installed npm package/version and range from steward version | Pinned npm install, fresh zero-running-thread guard before install/restart, served-version verification |
| worker-configuration | Reviewed per-host bootstrap entries preserved by push | Reference availability check, host-scoped configuration write; no enrollment |

A new capture replaces 45-agents module execution with the explicit environment
component. U1 removes the timer and module installation paths. Old four-component manifests
remain readable, but steward installation always uses UpKeeper's pinned artifact
handler. It interrogates the candidate's T3 range for historical manifests without
a frozen pair. Historical setup modules 26-t3 and 45-agents are refused.

## Capture and publication

```bash
./scripts/upkeeper push
./scripts/upkeeper push github gitea
./scripts/upkeeper push --no-push
```

Capture requires a clean release repository and clean installed checkouts whose exact
commits are reachable on origin/main. It refuses unpublished/local steward builds,
missing compatibility output, incompatible npm versions, malformed existing manifests,
and credential-bearing MCP registration fields. It never silently substitutes defaults
for a damaged existing manifest. After capture, push commits only release paths and
pushes to explicitly named remotes or all configured remotes.

The release embeds frozen agent payloads, including home-owned skills and executable
permissions. OS-supplied skill symlinks outside the home (such as /usr/share/omarchy)
remain package-owned and are preserved; UpKeeper neither captures nor writes them. The Claude import block and owned settings keys, shared agent instructions,
Codex global instructions, Claude/Codex/OpenCode MCP registrations and OpenCode instruction
list are captured. Provider/account state and unrelated settings are not captured.
Capture errors must be resolved at their owning source, never by weakening validation.

Worker entries are explicit reviewed inputs, not inferred from normandy's duplicated
coordinator YAML. See [the exact S4 contract](worker-distribution-v1.md).
U0 leaves the worker hosts map empty.

## Pull and observation

```bash
./scripts/upkeeper pull --hosts homelab omarchy-pc --dry-run
./scripts/upkeeper pull --hosts homelab --components agent-environment
./scripts/upkeeper pull --hosts homelab --components t3
./scripts/upkeeper pull --all
```

The controller validates and carries an in-memory Python module bundle and release
payload over argument-vector SSH with BatchMode, StrictHostKeyChecking and bounded
timeouts. Inventory machine identity selects local execution. Optional setup_checkout locates
an existing host checkout (the laptop uses Work/omarchy-setup); it does not clone,
relocate or bootstrap it. Inventory paths are validated under .local or Work. Targets never read the
repository. pull --self is the explicit target-side repository exception described below.

Each host is locked on controller and target; dry run observes without writes or service
effects. Components run in fixed order. Selecting either t3 or t3-steward in a new release
selects the pair. A failure or deferred T3 restart stops later components on that host.
A successful earlier component is not automatically rolled back by a later failure.

Environment materialization rebases structured home paths, preserves the local Claude
text outside the managed block, merges owned JSON keys and permission allows, preserves
Codex project/provider tables, and adds OpenCode instructions without removing local ones.
MCP credentials are never copied from normandy. The target uses the shared F05 parser and
drift projection against full materialized files, and retains a private complete prior
snapshot before activation. Synchronous failure restores and verifies that snapshot;
unexplained concurrent changes are preserved and reported rollback-failed.

The T3 guard defers on a failed check, unknown running-thread count or any running thread.
Deferral is drift (exit 10), not convergence. A fresh check is repeated after npm install
before restart. This is a point-in-time guard, not a scheduler admission lock.
U1 pull retires t3-update.timer/service before component effects on each host,
even for rollback or component-limited pulls. An active updater is deferred after
disabling its timer; it is never killed mid-install. Unknown unit state fails.
The T3 component seeds an absent t3code.service when the pinned package is installed,
with loopback binding and optional host-local oauth.env, warning when that file is
missing. Existing units are preserved. Pull does not enroll workers.

### Coordinator reload

When a pull changes the coordinator's catalog input
(`~/.config/t3-steward/coordinator-fleet.json`) on the coordinator host and
`t3-steward.service` is active, the `steward-fleet-configuration` component sends the unit
SIGHUP and then waits, up to 30 seconds and polling every 500 ms, for the receipt the
coordinator writes for that signal at
`~/.local/state/t3-steward/coordinator/reload-receipt.json`. The coordinator owns the
verdict; UpKeeper only reads it, and only a receipt whose `requestedAt` is at or after the
signal counts. An `accepted` or `unchanged` receipt converges the component, the action
reads "coordinator reloaded its configuration (digest ...)" and the result carries
`coordinator_reload` with the outcome and the digest now in effect. A `rejected` receipt
fails the component with the coordinator's error and every blocking assignment, one per
line, each with the worker, assignment, attempt, its state and the command that unblocks it;
the effective configuration and its digest are unchanged, and the projection is put back to
the document that run replaced (or removed, when the run created it) so that the file the
coordinator reads at its next start is one it can load. No receipt in time fails the
component as unverified, naming `t3-steward coordinator reload --json` to run on the host
and saying whether a receipt file existed at all, which is how a coordinator older than
rc.69 (one that writes no receipt) is told apart from a slow one. A dry run neither signals
nor waits, and an inactive unit is not signalled: it reads the projection when it next
starts. `fleet-plan` says, under any change that alters a worker's catalog revision (a
project it is eligible for, or its own models, quota pools, executor slots, capabilities, cpu
class or provider instances), that the reload is refused while that worker holds a retained
assignment.

## Records and recovery

Controller results are redacted under ~/.local/state/dev-fleet/runs/. Environment
prior bytes and modes remain target-local under
~/.local/state/dev-fleet/environments/<run-id>/; steward backups are under
~/.local/state/dev-fleet/binary-rollbacks/. They are evidence for deliberate recovery,
not an automatic process-crash replay mechanism.

Lost SSH responses/timeouts are reported and never automatically retried. Observe the
host and its private prior record before taking another action. Pull does not claim
the F03 PREPARED/STARTED/UNKNOWN journal semantics. Its component compensation is not
atomic fleet rollback. Operator rollback reads immutable release intent without moving the controller checkout:

```bash
./scripts/upkeeper rollback --to <release-commit> --hosts omarchy-pc --components t3-steward --dry-run
./scripts/upkeeper rollback --to <release-commit> --hosts omarchy-pc --components t3-steward
./scripts/upkeeper pull --hosts omarchy-pc --components t3-steward
```

The hash must resolve to an ancestor of controller HEAD (HEAD itself is allowed).
Manifest and legacy companion files come from that same commit; missing or corrupt
history fails before transport. Inventory and deployment code remain current.
Reports and locks name the resolved commit and rollback command. New manifests
select the T3 pair together; old manifests restore the steward alone after proving
the currently served T3 fits the candidate. No operation rediscovers a latest release
or recreates retired updater units. Use component selection when historical setup
modules have been retired. Setup retains fast-forward semantics; Huyang and steward
use exact historical pins. Binary rollback does not downgrade steward databases or
protocol state: select a version compatible with that host's persisted state.
This is an ordinary pull with the same locking, deferral and lost-response limits.

Exit codes retain the existing interface: 0 converged/updated, 10 ahead/deferred,
20 partial/offline when no deployment-failure result takes precedence, 40 failed or
rolled-back, 50 rollback-failed, 60 lock contention. A dry-run update describes a planned
change and retains the existing exit-0 reporting convention. A transport failure
currently carries result failed and therefore returns 40; it is never success.
