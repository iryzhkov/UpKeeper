# UpKeeper system model

Citadel Track U0, 2026-09-12. The active Citadel replan controls stage order.
[The push/pull guide](../upkeeper-push-pull.md) describes operator commands;
[the F03 guide](../upkeeper-mvp.md) describes the separate saga API.

## Purpose, scenarios and current state

UpKeeper publishes a controller-observed release and converges selected inventory
hosts. It owns fleet software and the managed agent environment. Any validated fleet host can be the
capture controller. Independent per-host timers refresh the CI-gated release and
converge locally; see [fleet convergence](../fleet-self-convergence.md). Targets receive a fixed Python program and validated payload
over strict SSH; the inventory machine-id match executes locally.

Successful capture observes clean, remotely reachable setup/Huyang commits, a
release-version steward, its reported T3 compatibility range, installed npm T3 and
owned environment projections. It refuses dirty repositories, unreachable pins,
malformed existing manifests, local steward builds, incompatible pairs and
credential-bearing MCP configuration.

Successful pull validates, locks, observes, materializes target-specific owned
projections, activates, verifies and reports. A lost response is uncertainty:
the controller records failure and does not retry. Observe the host before
deciding how to proceed.

U0 implements release components and worker bootstrap distribution. U1 retires
legacy updater units before every pull and loads historical release manifests for
operator rollback without moving the controller checkout. U2 adds target-side repository reads
through pull --self as the sole exception to the no-target-repository invariant.
It validates a clean invoking checkout, freezes its HEAD and release/companion files,
and matches exactly one inventory machine-id. Identity changes fail before execution;
there is no SSH fallback. Locks, records and component safeguards are shared with pull.

U3 adds Normandy's daily all-host convergence and the homelab-infra drift report seam.
The schedule freezes clean committed input, skips only pre-dispatch network unavailability,
and records a durable host hold before dispatch. Uncertain or failed effects retain that hold
for operator observation, preventing automatic daily retries. It posts only changes or failures.
See [scheduled convergence](../scheduled-convergence.md) for installation and recovery.

U4 adds the laptop's own timer. It refreshes only fast-forward main commits whose
exact GitHub push gate passed, then re-executes that code and uses the local-only
self-pull transaction. The self-managed inventory flag prevents controller pulls
from probing or dispatching to the laptop. Its local journal/run record, not a
controller skip, is the deployment observation. Worker enrollment remains separate.
See [laptop convergence](../laptop-self-convergence.md).

## State and ownership

| State | Authority / mutator | Copies, freshness and recovery |
| --- | --- | --- |
| Inventory identity, address and SSH principal | dev-fleet inventory | Machine-id for local execution; strict host verification remotely |
| Release intent | Controller capture and committed desired/release.json | GitHub/gitea copies; release digest travels with pull and its record |
| Installed checkout, binary, package | UpKeeper pull | Observe actual HEAD, binary identity and npm package |
| T3 served version and running threads | Steward check against T3 | Fresh observation before effects; absent/busy evidence defers |
| Agent file set / owned projections | UpKeeper release | F05 spec plus exact payload digests; preserve unowned host settings |
| Previous whole environment | Target-local UpKeeper transaction | Private prior bytes, absent-path markers and F05 prior identities |
| Worker bootstrap | UpKeeper per inventory host | Ref-only worker-bootstrap.json; configured is not enrolled/observed |
| Catalog, enrollment, epochs, leases, execution | Steward coordinator/worker | S4 owns runtime consumption; UpKeeper never duplicates catalog policy |
| Secret values | Host secret provisioning authority | Target-local 0600 store; only opaque references are distributed |
| Conversations | T3 | UpKeeper defers restart; it does not create or settle conversations |
| Huyang journals / worktrees | Huyang / Git | Recovery references only; environment rollback does not mutate these |
| Pull locks / reports | Controller and target UpKeeper | Per-host locks, redacted records; uncertainty requires observation |
| F03 intent / effects | Saga caller and journal | Revision-fenced hash chain; UNKNOWN retains locks until reconciliation |

OS packages, desktop, toolchains, dotfiles, sshd and secret provisioning remain
omarchy-setup concerns. agent99 remains independent. UpKeeper does not acquire
workflow or scheduler authority through deployment.

## Boundaries and intentional seams

Inventory supplies transport addresses and principals. A worker bootstrap cannot
supply shell commands, a new SSH destination, catalogs or credential bytes.
S4 consumes [the distribution contract](../worker-distribution-v1.md), then
observes capabilities and authenticates enrollment separately.

Environment capture owns the Claude managed import and imported instructions,
home-owned skill files, Claude settings keys, shared agent instructions, Codex global
instructions, named MCP registrations and OpenCode instruction-list additions.
It omits account state, provider credentials, unrelated JSON keys, Codex project
tables and unrelated MCP registrations. Unsupported/malformed shared-file layouts
fail instead of causing whole-file replacement. MCP executable/service prerequisites
are not installed by a file projection.

The capture AgentEnvironmentSpec v1 freezes provenance and projection bytes.
On the target, rendering rebases structured home paths, merges owned keys and
materializes another complete F05 spec with full-file desired/prior identities.
classify_asset_drift supplies MATCH/MISMATCH/MISSING/UNAVAILABLE semantics.
The bounded projection checks prove desired identities and preservation rules,
not live provider availability or semantic instruction behavior.

The transmitted program imports this same code from in-memory module payloads.
It needs Python 3.11+, not a target dev-fleet checkout, package install or import
cache. Dry run writes no files, starts no services and reads no secret values.

## Pull transaction and failure semantics

Order: setup, legacy agent-config (empty for new releases), agent-environment,
Huyang, steward, T3, worker-configuration. Selecting either half of the new T3 pair
selects both. Per-host locks surround observation and mutation; a local target
re-enters its controller's lock. Foreign/malformed locks fence mutation.

An environment change freezes the complete prior bytes and stages all desired
bytes outside active paths before activation. Each file is compared with its
observed prior bytes immediately before replacement. Final verification covers
every F05 desired asset. A synchronous failure restores every activated file,
removes newly created files and proves the complete prior environment. Concurrent
unexplained changes are preserved and reported rollback-failed.

A steward change downloads the pinned archive, verifies its published checksum
and interrogates the candidate binary for the frozen compatibility range.
Currently served T3 must fit that range too. Failed steward activation restores
and verifies the previous binary/service.

U1 seeds an absent t3code.service once the pinned package is installed, preserving
existing units and host-local credentials. Missing oauth.env is a warning.
T3 npm installation and restart both require a fresh successful steward check
reporting zero running threads. Missing, failed or busy evidence yields deferred
(exit 10). Installed and served versions are verified after restart. Worker
configuration checks reference availability before projecting only the selected
host's bootstrap. It retains a private prior copy and never enrolls a worker.

The first failed, rolled-back or deferred component stops later components on
that host. Previously verified components and other hosts are independent.
This is not an atomic fleet transaction. npm/service failures are reported for
observation; no implicit npm downgrade is attempted.

## Recovery, uncertainty and limits

Environment compensation covers synchronous exceptions. Process death or lost
SSH may leave staged/partially activated files. The private prior snapshot survives,
but automatic crash-recovery replay is not implemented. A timeout does not prove
an effect failed or is safe to repeat. Inspect actual state and retained snapshots
before recovery. Controller run records redact results and do not contain secret
values, environment payloads or private prior contents.

The idle check is a point-in-time observation, not a coordinator admission lock.
An admitted thread between observation and restart remains a reliability risk.
Pull disables and deletes legacy updater units before deployment. A running
updater is deferred after timer disable; failed observation stops component effects. File projection
does not prove instruction semantics or provider readiness. U0 does not close the
hardening waiver or claim S5's multi-host soak.

## Two mutation paths remain visible

release.py, push.py and pull.py are the live path. upkeeper.py, its Ansible adapter
and rollout/environment qualification fixtures are the F03 saga path. The latter
freezes approvals and revisions, writes PREPARED/STARTED effect records, retains
UNKNOWN and reconciles before retry. Pull shares file contracts, lock/reporting
primitives and validation concepts; it does not claim that saga journal or its
external-effect reconciliation.

F03 remains available for separately authorized homelab-infra work and is the
reference for strengthening pull. A prior file snapshot or run log is not a
durable reconciliation protocol. The replan explicitly accepts that distinction.

## Invariants and evidence

Enforced: exact component sets, reachable immutable pins, clean capture and
target checkouts, compatibility provenance, owned-key preservation, verified
environment payloads, contained target paths, host-scoped ref-only worker bootstrap,
strict SSH, target repository reads only through --self, no automatic retries and
no dry-run writes. The setup handoff installs prerequisites and invokes --self last;
failed machine-bootstrap modules prevent that handoff. It adds no schedule.

Not proved by local tests: fleet atomicity, restart admission fencing, live provider
behavior, enrollment, process-crash recovery or soak reliability.

make gate runs Ruff, Ansible lint, full pytest, syntax/check-mode and redaction.
U0 tests execute the transmitted agent in an isolated empty home, inject partial
activation/concurrent edits, preserve private settings, reject corrupt payloads,
escaping links and credential-bearing registrations, check secret-reference
availability and prove T3 deferral without idle evidence. Citadel's U0 handoff
records commits, exact gate/CI results and remaining live-capture blockers.
