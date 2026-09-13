# UpKeeper push and pull

UpKeeper captures a controller-observed release and converges named inventory hosts.
Read [the system model](architecture/system-model.md) for ownership, failure semantics
and the distinction from the F03 saga library.

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
component. The old timer and module files remain installed until U1. Old four-component
manifests remain readable and retain their legacy behavior, including t3-update
delegation; capture upgrades their component set after all observations pass.

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

The release embeds frozen agent payloads, including installed skills and executable
permissions. The Claude import block and owned settings keys, shared agent instructions,
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
timeouts. Inventory machine identity selects local execution. Targets never read the
repository. The planned --self exception belongs to U2.

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
No U0 pull seeds T3 services, removes timers, enrolls workers or implements --self.

## Records and recovery

Controller results are redacted under ~/.local/state/dev-fleet/runs/. Environment
prior bytes and modes remain target-local under
~/.local/state/dev-fleet/environments/<run-id>/; steward backups are under
~/.local/state/dev-fleet/binary-rollbacks/. They are evidence for deliberate recovery,
not an automatic process-crash replay mechanism.

Lost SSH responses/timeouts are reported and never automatically retried. Observe the
host and its private prior record before taking another action. Pull does not claim
the F03 PREPARED/STARTED/UNKNOWN journal semantics. Its component compensation is not
atomic fleet rollback. The operator rollback --to command remains U1.

Exit codes retain the existing interface: 0 converged/updated, 10 ahead/deferred,
20 partial/offline when no deployment-failure result takes precedence, 40 failed or
rolled-back, 50 rollback-failed, 60 lock contention. A dry-run update describes a planned
change and retains the existing exit-0 reporting convention. A transport failure
currently carries result failed and therefore returns 40; it is never success.
