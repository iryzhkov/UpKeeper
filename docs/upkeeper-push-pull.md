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

## The resident agent instructions are gated before anything is captured

A capture photographs the pushing host's home directory. The shared `CLAUDE.md`, the
files under `~/.config/agents` and every skill under `~/.claude/skills` are read from
that home directory, stored as mode `file` assets, and written over the same paths on
every other host at its next convergence. No repository installs those files onto a
host, and `agent-environment` is applied after `omarchy-setup` in every convergence, so
the captured bytes win over anything a setup module wrote earlier in the same run.

That means a host that has drifted from the checkout its instructions are generated from
would publish its drift to the whole fleet, and destroy every other host's copy while
doing it. Nothing downstream can notice: the manifest is well formed, the assets
validate, and the convergence succeeds. The pushing host is the only place the
difference is still visible, so it is the only place it can be refused.

Before a capture observes anything, it runs the omarchy-setup checkout's own
`agents-instructions-gen --root <checkout> --check`, which writes nothing and answers 0
for a converged host, 3 for a diverged one, and any other status for a check that could
not run. The comparison is delegated rather than reimplemented so that "diverged" keeps
one definition, in the repository that owns the files.

- **Diverged.** The push refuses, lists each path with the direction of its difference,
  explains the photograph, and names the command that converges the host. Nothing is
  written, staged, committed or pushed.
- **Could not run.** No checkout, no installed generator, an installed generator that
  predates `--check`, a check that could not be started or did not finish inside its
  60-second bound, an unexpected exit status, or a report of a divergence that names no
  path: the push refuses just as firmly and names which of those it is. "I could not
  check" is not "there is nothing to check".
- **The old generator is recognised and never executed.** The copy that predates the
  check ignores every argument it is given, rewrites `~/.config/agents` and
  `~/.codex/AGENTS.md`, and exits 0. Running it as a check would modify the host it was
  meant to report on and call that success, so the installed command is inspected for
  `--root` and `--check` before it is run at all.
- Both refusals exit 30, the preflight-failure code: a check that had to pass before
  anything was published did not pass.
- A `--components` selection that does not name `agent-environment` is not gated, because
  such a capture restores the resident assets the release already published and therefore
  promotes nothing. That is also the way to publish an unrelated component from a host
  whose check is broken.

`--publish-diverged-instructions` is the only way through, and it covers the diverged
case only. It has to be typed in full, no general `--force` reaches it, and it prints
every path it is promoting, to standard error, before it promotes it. It deliberately
does not cover a check that could not run: an override has to say what it is promoting,
and an unanswered check cannot name it.

The gate does not promise that the repository is right, only that the host matches it. A
wrong instruction that has been committed and converged is a different problem, and it is
solved by reviewing the omarchy-setup diff rather than by the capture.

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
