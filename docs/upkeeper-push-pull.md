# UpKeeper push and pull

UpKeeper captures a controller-observed release and converges named inventory hosts.
Read [the system model](architecture/system-model.md) for ownership, failure semantics
and the limits relative to the historical F03 saga API, retired from new Python
0.2.0 installs (last-supported source 40e29c6c4b0b1dcc258e42f4aeba6ecc023c2460,
package 0.1.0).

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

## Authored releases

```bash
git switch -c release/steward-rc96 origin/main
./bin/upkeeper author --steward 0.11.0-rc.96 --omarchy-setup <commit> --dry-run
./bin/upkeeper author --steward 0.11.0-rc.96 --omarchy-setup <commit>
git commit -m "Pin Steward 0.11.0-rc.96" -- desired/release.json
./bin/upkeeper push --authored --expect-head <that commit> --expect-release-sha256 <digest>
```

A release that only moves the Steward pin and the omarchy-setup commit does not need a
capture, and should not take one: a capture photographs one host. `author` computes the
release from its inputs instead. It sets `t3-steward.version` and
`t3.compatibility.steward_version` to the Steward version and `omarchy-setup.commit` to the
commit, then refreshes every agent-environment file that derives from that commit:

- the skills under `config/claude/skills`, projected to `.claude/skills` and
  `.agents/skills` exactly as capture projects them, with the modes Git records;
- `~/.config/agents/*.md` and `~/.codex/AGENTS.md`, as the commit's own
  `config/bin/agents-instructions-gen` writes them into an empty scratch home;
- `~/.claude/omarchy-setup/CLAUDE.md`, from `config/claude/CLAUDE.md`.

A file the commit adds is added, and a file it no longer produces is retired with the tombstone
capture would write. Skills are matched by name: every released file under
`.claude/skills/<name>`, `.agents/skills/<name>` or `.codex/skills/<name>` for a skill the new
commit or the previously pinned commit has under `config/claude/skills` is replaced by the new
commit's files, so a skill the new commit deleted is retired, while a personal skill that
happens to share a repository skill's name is replaced too. The whole `~/.config/agents/`
prefix counts as generated: a released file there that the generator no longer writes is
retired, whoever put it there. The previously pinned commit has to be in the clone; when it is
not, `author` refuses rather than keep a dropped skill in the release.
When anything changed, the spec is rebuilt with the builder capture uses and the instruction
provenance is moved to the checkout's `HEAD`; when nothing did, the component is left byte for
byte as it was, so authoring the release that is already published reports no changes.

The commit is read out of the omarchy-setup clone (`--omarchy-setup-repo`, default
`~/Work/omarchy-setup`) after fetching origin, and must be reachable from the ref the release
follows. The clone's configured origin URL must name the repository the release gives as
omarchy-setup's `source` (https, `ssh://` and `git@host:` spellings, with or without `.git`,
are the same repository), so a fork or stale mirror is refused. Archive entries that link
outside the extracted tree are refused as well.

**`author` runs the pinned commit's `config/bin/agents-instructions-gen` on the operator's
machine, as the operator, without a sandbox.** Its only confinement is an empty scratch
`HOME` and `TMPDIR` and a minimal environment; it can still read and write anything the
operator can. Pin only commits you would run yourself, and review the generator's diff in
omarchy-setup before authoring a release from it. Generated files are published as 0644, or
0755 when executable, whatever the operator's umask. Nothing is read from the home directory of the host running the command, and the
scratch tree and home are removed afterwards. The checkout may carry no local change except to
`desired/release.json` itself, so that a mistaken run can be recomputed before it is
committed. The result is validated the way authored publication validates it, written in the
manifest's canonical encoding with every other byte preserved, and never committed or pushed.
`author` does not change the T3 version or its tested range, or the Steward session hook
digest: a Steward release that changes either still needs a capture.

`push --authored` publishes the reviewed bytes as they stand, without recapturing anything,
once `HEAD` and the release digest match both fences. Every host the release names (Steward
fleet hosts, clients and coordinator, workers, Jocasta hosts) has to be in the working
`inventory/hosts.yml`, or nothing is published. When the inventory committed at the expected
head lacks one of them, the working inventory is committed with the release, so the published
commit is consistent; an inventory edit the release does not need stays uncommitted.

## The resident agent instructions are gated before anything is captured

A capture photographs the pushing host's shared instruction files and personal skills,
but repository-owned skills are never taken from installed home copies. UpKeeper reads
their complete trees from the exact pinned omarchy-setup checkout, replaces same-named
captured copies, and emits each one under `~/.claude/skills` for Claude/OpenCode and
`~/.agents/skills` for Codex. The agent-environment component is the only publisher and
converger of those skill bytes; omarchy-setup supplies reviewed source and bootstrap only.
When a source-owned regular file disappears, capture carries its reviewed bytes forward as a
retirement tombstone. Pull removes it only while the target still matches those bytes, retains the
tombstone across later releases for intermittently connected hosts, and refuses unexplained edits.

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

The release embeds frozen agent payloads, including source-projected fleet skills,
home-owned personal skills and executable permissions. OS-supplied skill symlinks outside
the home (such as /usr/share/omarchy)
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
A blocked component (see [Fresh host](#fresh-host)) does not: only the components that
depend on it are skipped.
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

## Fresh host

A host that has never been converged needs a few things UpKeeper does not install or
copy. Everything else, Huyang included, is installed by the first `pull`.

### Before the first converge

1. Put the released `upkeeper` binary at `~/.local/bin/upkeeper`, add the host to
   `inventory/hosts.yml` with its machine identity and SSH host key, and make sure the
   controller can reach it over SSH with the key already in `known_hosts`.
2. Install the OS prerequisites. `upkeeper doctor --limit HOST` lists every missing one as
   a `blocker:` line with the command that installs it, looking where the agent will look
   (`~/.local/bin` and the mise shims count even though a non-interactive SSH session has
   neither on its `PATH`). The current catalogue, by component:

   | Component | Needs | Debian package | Arch package |
   | --- | --- | --- | --- |
   | omarchy-setup | git | git | git |
   | agent-tooling | mise, bash, curl, jq, python3, ssh, readlink | (mise from mise.run), bash, curl, jq, python3, openssh-client, coreutils | mise, bash, curl, jq, python, openssh, coreutils |
   | agent-tooling, when it stages a release | secret-tool; Python's venv and ensurepip | libsecret-tools, python3-venv | libsecret, python |
   | huyang | git; go 1.21 or newer and make when it builds | git, golang-go, make | git, go, make |

   Go 1.21 and later fetch the toolchain Huyang's `go.mod` names. Debian 12's golang-go is
   1.19 and is reported as a blocker; install a newer Go from go.dev or with mise there.

   mise also has to provide the node, codex, claude and opencode tools in its global
   configuration, as on every other host.
3. Create the per-host credentials by hand. UpKeeper never copies a secret between hosts:
   - the OpenViking API key for ov-memory: `umask 077; mkdir -p ~/.config/ov-mcp; cat >
     ~/.config/ov-mcp/key` (paste the key, then Ctrl-D), or store it in the keyring with
     `secret-tool store --label='OpenViking API key' service openviking key api`;
     `OV_API_KEY` or `api_key` in `~/.config/ov-mcp/config.toml` also count. A host that
   has already staged the release and keeps its key in one of those places does not need
   secret-tool;
   - `~/.config/claude/oauth.env`, if T3 should authenticate providers (the seeded T3 unit
     reads it when present and warns when it is not);
   - the worker's Steward protocol credential that its `worker-configuration` entry names.

A host may instead opt in to having missing OS packages installed by the agent, with
`"install_os_packages": true` in its inventory entry. The agent then runs `apt-get install`
or `pacman -S` only through `sudo -n`, so only where sudo needs no password; anywhere else
the component stays blocked with the install command. Without the opt-in, pull never
installs an OS package.

### The first converge

```bash
./bin/upkeeper doctor --limit agent-a
./bin/upkeeper pull --hosts agent-a --dry-run
./bin/upkeeper pull --hosts agent-a
```

The dry run reports a missing prerequisite or credential as the component state `blocked`
with the remedy, before anything changes. A blocked component changes nothing, and the
components after it still converge unless they depend on it (t3, worker-configuration and
steward-fleet-configuration depend on t3-steward); a blocked host exits 10, like drift.
Create what the message names and pull again.

On the first converge of a fresh host, expect:

- every component except the blocked ones to report `updated`; Huyang is cloned and built
  in a private stage, renamed into place without ever replacing something that is already
  there, and its unit is seeded and started;
- t3-steward to install its verified binary although no T3 exists yet: no T3 at all is a
  safe pair transition (only when npm answers that no global t3 package is installed, or its
  global prefix has no `lib` directory yet, and neither `lib/node_modules/t3` nor a linked
  `bin/t3` exists under that prefix; an npm that is missing, times out or cannot read its package tree refuses the transition), because nothing is running that the steward could be incompatible
  with and the candidate has proved that the release's T3 is in its range. When the host has
  no `t3-steward.service`, the binary is installed and verified without a restart and the
  action says so; install the service with `t3-steward install-service` once T3 serves;
- t3 to install the pinned T3 after the steward.

Every command the agent runs is looked up on its own explicit `PATH` (the mise shims,
`~/.local/bin`, `~/go/bin`, `/usr/local/go/bin`, then the session's), so a hand-written
`PATH` block in `~/.bashrc` is no longer needed on any host.

If the Huyang bootstrap is interrupted after the built checkout is in place, the next run
finishes it (builds the checkout again, seeds the unit if needed, enables and probes the
service) instead of mistaking the checkout for an installed Huyang. It does so only while the
checkout is on the pinned commit without local changes; otherwise it refuses and keeps the
pending marker. Both the checkout and the unit are checked again after the build: if either
changed while it ran, the checkout directory was replaced, or the unit is masked, unreadable or
no longer the seeded one, nothing is seeded or started and the marker stays. A service that is
already running is restarted, so it serves the binary recovery built. A service that does not answer the probe is disabled again.

If the agent-tooling stage fails part-way, the next run moves that incomplete stage to
`~/.local/share/upkeeper-tools/failed-stages/` (the newest three UpKeeper put there are kept
for diagnosis; anything else in that directory is left alone) and stages again. Only a stage carrying UpKeeper's own `.upkeeper-stage` marker for the same
release is moved; any other incomplete directory is preserved and refused until somebody
has looked at it. Stages left by an UpKeeper older than this marker are refused once and
have to be moved aside by hand.

## Records and recovery

Controller results are redacted under ~/.local/state/dev-fleet/runs/. Environment
prior bytes and modes remain target-local under
~/.local/state/dev-fleet/environments/<run-id>/; steward backups are under
~/.local/state/dev-fleet/binary-rollbacks/. They are evidence for deliberate recovery,
not an automatic process-crash replay mechanism.

Lost SSH responses/timeouts are reported and never automatically retried. Observe the
host and its private prior record before taking another action. Pull does not claim
the historical F03 PREPARED/STARTED/UNKNOWN journal semantics. Future durable
operations still require a qualified executor/journal/fence with independent acceptance.
Pull component compensation is not atomic fleet rollback. Operator rollback reads immutable release intent without moving the controller checkout:

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

Exit codes retain the existing interface: 0 converged/updated, 10 ahead/deferred/blocked,
20 partial/offline when no deployment-failure result takes precedence, 40 failed or
rolled-back, 50 rollback-failed, 60 lock contention. A dry-run update describes a planned
change and retains the existing exit-0 reporting convention. A transport failure
currently carries result failed and therefore returns 40; it is never success.
