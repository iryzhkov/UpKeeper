# UpKeeper push and pull

`upkeeper push` and `upkeeper pull` are the first UpKeeper commands that change a
machine. They deliberately do less than the F03 library saga: no Ansible, no
hash-chained event journal, no multi-phase approval. What they keep from that design
is the shape that matters operationally: a frozen, validated manifest; observe before
mutate; verify after; roll back what can be rolled back; locks on both sides; and a
redacted run record on the controller.

## The release manifest

`desired/release.json` is the fleet's desired state for the four things this
repository now deploys. It is captured, never typed:

| Component | Kind | Captured from | Applied by |
| --- | --- | --- | --- |
| `omarchy-setup` | git checkout | `~/.local/share/omarchy-setup` HEAD, must be on `origin/main` | `git pull --ff-only`, then the `28-scripts` and `45-agents` modules |
| `agent-config` | files | `~/.codex/AGENTS.md`, stored under `desired/agent-config/` | atomic write with a `.upkeeper-<run>.bak` backup |
| `huyang` | git checkout | `~/.local/share/huyang` HEAD, must be on `origin/feature/huyang` | fetch, detach at the commit, `make build`, restart, MCP probe |
| `t3-steward` | GitHub release | `t3-steward version`, must be a published release | `t3-update.service` (the fleet's existing installer) |

Every value is validated before it is written and again before it is sent: full 40
character commits, safe ref names, home-relative paths below `.config`, `.local`,
`.claude` or `.codex`, module names of the form `NN-name`, and a steward version that
is a release rather than a `-N-gHASH` or `-dirty` local build. A dirty checkout or an
unpushed commit refuses to become a release, because a target could not fetch it.

Claude Code's own files are not listed: omarchy-setup owns them, and running its
`45-agents` module is how they converge. Codex and OpenCode MCP registrations are
checked read-only and reported, not rewritten, because those files carry per-host
provider settings.

## `upkeeper push [REMOTE ...]`

1. Capture this machine into the manifest and store the agent files.
2. Commit `desired/release.json` and `desired/agent-config/` (only those paths) when
   they changed.
3. Push the current branch to the named remotes, or to every configured remote.

`--no-commit` writes the manifest only; `--no-push` commits without pushing.

Publishing is for durability and review. Targets never fetch this repository; the
controller carries the manifest to them.

## `upkeeper pull --hosts HOST ... | --all`

For each host, in the order given:

1. Acquire the controller-side lock (`~/.local/state/dev-fleet/locks/<host>.lock`).
2. Send the fixed standard-library agent program over the same strict SSH transport
   `fleet doctor` uses (`BatchMode=yes`, `StrictHostKeyChecking=yes`, argument
   vector only), with the validated manifest embedded as a JSON literal. The
   inventory host that matches this machine's `/etc/machine-id` runs it locally.
3. The agent takes the target-side lock, then handles components in a fixed order:
   `omarchy-setup`, `agent-config`, `huyang`, `t3-steward`. Each is observed first and
   left alone when it already matches. The first failure stops the host; later
   components are reported as `skipped`.
4. Results come back as one JSON document per host and are written, redacted, to
   `~/.local/state/dev-fleet/runs/<run-id>.json` on the controller.

`--dry-run` observes and prints what would change without taking locks or writing a
run record. `--components` limits the set. `--timeout` is per host (default 1800 s,
because a Huyang build plus an omarchy-setup module run can take minutes).

### Per-component behaviour

- **omarchy-setup**: refuses a dirty checkout. Pulls fast-forward only. A checkout
  that ends ahead of the release is reported as `ahead`, not rewound. Modules run
  when the commit changed or when any managed file (`config/claude/CLAUDE.md`,
  `config/claude/skills/*/*`, `config/bin/*`) differs from its installed copy. On a
  host with Omarchy, `run.sh --only ...` runs them; on Debian the modules run directly
  with the environment `bin/t3-update` uses.
- **agent-config**: compares digests, backs up, writes atomically, verifies the digest.
- **huyang**: refuses a dirty checkout. If the pinned commit is already checked out,
  the service is active and the MCP probe returns the expected tool count, nothing
  happens. Otherwise: fetch the ref, confirm the commit is present, detach at it,
  build, restart, wait for the control socket, probe. Any failure rebuilds and
  restarts the commit that was found before, probes again, and the component ends
  `rolled-back` (or `rollback-failed` when that also fails).
- **t3-steward**: runs `t3-update.service`, which installs the newest compatible
  release. A result equal to the manifest is `updated`; a newer one is `ahead`; an
  older one or an inactive service is a failure. There is no rollback here because
  the installer, not UpKeeper, owns that path (edit `config/t3.conf` and rerun).

### Exit codes

The stable codes from `docs/architecture.md`, worst outcome first: `50` rollback
failure, `40` a failed or rolled-back component, `60` lock contention, `20` a host
that could not be reached, `10` a host ahead of the release, `0` converged or updated.

## What this is not

It is not the R3/R4 mutation plane of the Citadel roadmap and does not replace the
library in `src/dev_fleet/upkeeper.py`: there is no approval step, no revision-fenced
event journal, no reconciliation of an ambiguous SSH loss (a lost response is reported
as offline and the host must be observed with `fleet status` or a dry run before
retrying), and no host bootstrap. A missing checkout is a failure, never a clone.
`desired/components.yml` and its `deployment_enabled: false` are untouched; the
release manifest is a separate, narrower authority.
