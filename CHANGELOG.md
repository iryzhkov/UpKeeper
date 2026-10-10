# Changelog

Notable changes to UpKeeper are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are the
release tags (`v0.1.x`).

## [Unreleased]

### Added

- Operator holds. `upkeeper hold set --hosts HOST... --reason TEXT [--until TIME]`
  fences hosts before a release, and `hold clear --hosts HOST... --reason TEXT`
  releases them. A hold is recorded in
  `~/.local/state/dev-fleet/operator-holds/HOST.json` on this host and, through the
  new target-side `__hold` command over SSH, on each held host, so it survives reboot
  and every path honours it: a held host's self-pull refreshes nothing, updates no
  binary and converges nothing; scheduled and controller pulls report the host `held`
  and dispatch nothing; the agent refuses a payload for a host held in its own state.
  An explicit `pull --override-hold` passes through and is logged in `HOST.log` on the
  controller and the target; rollback, dry runs and status are never held. A host that
  cannot be reached keeps the controller's copy and the command exits 20. `hold show`
  lists operator holds with reason, setter and expiry beside the scheduled markers. A
  hold record that cannot be read holds. Before this, the only fence was stopping
  `upkeeper-self.timer` on every host, which a reboot re-armed.
- `upkeeper status --expect FILE` reads a JSON list of accepted non-converged states,
  each with a reason. A host whose every non-converged state is listed no longer fails
  status, so status can gate a rollout; the report lists what was accepted and the
  entries that matched nothing. Without `--expect` status is unchanged.
- `docs/authored-release.md`: the authored release procedure for a binary-only Steward
  bump (hold, author, review package, push, canary, fleet, rollback), linked from the
  README.

- A failed controlled T3 activation now journals its original cause and every
  failed installed, served, health, native and unit predicate sample before
  rollback, and reports the restoration result beside that cause instead of
  replacing it. Post-restart and restoration readiness are bounded probes that
  also require Steward units declared `PartOf=` the T3 unit to return under a
  new invocation; unknown dependent activity refuses before any effect, and
  dependents active before the activation remain restoration obligations, also
  when the T3 unit itself was inactive. Failed transaction command and health
  check output is redacted complete before it is bounded. Journal records are
  published only under the host's target lock and only over the record the
  transaction last wrote or read; a journal changed by any other writer is
  left untouched and the intended record is quarantined beside it.
  `upkeeper pull --hosts HOST --components t3
  --reconcile-t3-transaction NAME` explicitly closes one transaction left at
  `files-restored` once every retained tree, the unit and the serving identity
  prove unchanged; it does not perform legacy updater retirement. See
  `docs/t3-transaction-reconciliation.md`.

- Digest-verified self-update. A release may pin UpKeeper's own binary in an
  optional `upkeeper` component (tag, source commit, toolchain, and per
  architecture the asset, size and sha256). When `self-pull` finds that the
  running binary's program differs from the gated checkout's, it downloads the
  pinned asset, refuses any size or digest that differs from the manifest,
  probes its `version` with an empty `PATH`, installs it as a generation under
  `~/.local/lib/upkeeper`, switches `current` atomically and re-executes it as
  `self-pull --after-update COMMIT`, which converges. Failures leave `current`
  unchanged or restore the previous generation. `upkeeper self install
  --expect-sha256`, `self rollback` and `self status` manage the generations by
  hand. A pin whose program is not the gated checkout's is stale and is
  refused whole, before anything is fetched; code-repository CI does not hold
  a commit to its pin, and that invariant moves to the configuration
  repository's CI (M2b). `go.sum` now counts as program, remote agents are invoked as
  `__agent --expect-program COMMIT` and refuse a program they were not built
  from (an agent from before the fence rejects the option and does no
  work), and `status` and the
  probe report each host's binary version, commit and generation.

- `upkeeper author --steward VERSION --omarchy-setup COMMIT` writes an authored
  release into `desired/release.json` without capturing any host. It sets the
  Steward and omarchy-setup pins and refreshes the agent-environment skills and
  instructions that derive from that commit, running the commit's own
  instruction generator against a scratch home. It replaces the out-of-repository
  helper that had to copy a test file into the tree, and its output is
  byte-identical to that helper's for the same inputs.

- UpKeeper is now the single publisher of repository-owned skills. Capture reads
  the complete skill trees from the pinned omarchy-setup checkout, ignores stale
  installed copies of those names, and projects the reviewed bytes to
  `~/.claude/skills` for Claude/OpenCode and `~/.agents/skills` for Codex.
  omarchy-setup remains the bootstrap and source repository; it does not publish
  or converge skills.

- Every invocation of `upkeeper` appends one record to the tool-feedback spool
  the fleet's MCP servers already write, at
  `~/.local/share/toolfeedback/upkeeper/<host>-<date>.jsonl`. The record holds
  the verb path, the option names, an outcome class, the exit code and the
  elapsed time, which is what turns a per-verb refusal rate into a measurement
  rather than an impression. It carries no argument values: a word extends the
  verb path only when it names a published command, an option is recorded only
  when the parser declares it and otherwise as `?`, and an error is recorded as
  the name of its exit code and never as its message, because these records are
  shipped to another host. `outcome` maps the stable exit codes onto the five
  classes an agent experiences: `ok` (0 and 10, because drift is a report's
  finding and not a refusal), `usage` (2 and 30), `state` (20, 40 and 50),
  `transport` (60) and `internal` (1). The spool is off unless the `enabled`
  marker exists in the spool directory or `UPKEEPER_FRICTION=1` or
  `TOOLFEEDBACK=1` is set, and a spool that cannot be written is skipped in
  silence: it never changes the exit code, standard output or standard error.
- A full capture refuses a host whose resident agent instructions have drifted
  from the omarchy-setup checkout that generates them. A capture photographs
  the pushing host's home directory, and the shared `CLAUDE.md`, the files
  under `~/.config/agents` and every skill under `~/.claude/skills` reach every
  other host as a whole-file overwrite, so a hand edit here would become a
  fleet fact that nothing downstream could notice. The capture now asks
  `agents-instructions-gen --root <checkout> --check` before it observes
  anything, and refuses at exit 30 with the diverged paths named. A check that
  could not run at all -- no checkout, no installed generator, a generator that
  predates `--check`, a run that failed or timed out, an unexpected exit status
  -- refuses in the same place with its own message: "could not check" is not
  "nothing to check". Nothing is captured, written, committed or pushed by
  either refusal, and `--components` still publishes work that carries no
  resident instructions (A-2).
- `upkeeper push --publish-diverged-instructions` is the operator's explicit
  permission to publish this host's resident instructions although they differ
  from their source. It must be typed in full, it prints every path it promotes
  to standard error before the capture proceeds, and it covers a detected
  divergence only: a check that could not run is still refused, because an
  override has to say what it is promoting and a check with no answer cannot
  name it.
- `upkeeper provider add INSTANCE --quota-pool POOL --hosts a,b [--models m1,m2]`
  and `upkeeper provider remove INSTANCE [--hosts a,b]` register or remove a
  provider instance in the `steward-fleet-configuration` component of
  `desired/release.json` the way `project add|remove` edits a project: one
  edit carries the instance, its models (`*` unless named), the quota pool and
  the instance-to-pool entry in the new `quota_bindings` profile key; the
  profile is edited when every host of it is named, otherwise each named
  host's exceptions are, and the output says which. An unknown host, an
  instance already bound to a pool, or an invalid name is refused with
  nothing written. The edited manifest is validated by `fleet.Parse`, written
  atomically in the canonical encoding, and the `fleet-plan` diff against
  `HEAD` is printed (F-17).
- The fleet contract's optional, closed `quota_bindings` profile key maps a
  provider instance to the one quota pool its work is charged to. A binding's
  instance must be in `provider_instances` and its pool in `quota_pools`; a
  host exception may override or introduce it. The coordinator projection
  renders it per worker only when a binding is authored, so a release that
  binds nothing projects the same bytes as before. The coordinator honors the
  field only once its own change lands; until then a coordinator refuses a
  projection that carries it, and the contract says so.
- The target-side probe and the observe-only agent report the host's own
  convergence evidence under `upkeeper`: its newest scheduled runs (id, time,
  command, outcome, failed components) from its run store, its hold markers
  with the run that left each and its age, and the state of
  `upkeeper-self.timer` and `upkeeper-pull.timer` (enabled, active, next
  elapse). `upkeeper status` explains a remote host's drift from that
  evidence exactly as it explains the controller's own, and prints "reason
  unavailable from this host" only for a target whose binary reported none
  (F-16).

### Changed

- Every publication (`push` and `push --authored`) updates `main` on every configured
  remote. Before capturing or committing anything it refuses a remote list that omits
  a configured remote, a checkout on any branch but `main` (an authored push from a
  clone on another branch used to publish under that branch's name), remotes whose
  `main` differ (naming each remote, its commit and the `git push` that resyncs them,
  after Gitea's main lagged GitHub's for a week unnoticed), and a remote `main` that
  `HEAD` does not contain. `--no-push` is unaffected.

- `fleet-plan` lists a quota binding that appears, moves or disappears
  ("worker W binds I to quota pool P", "rebinds", "unbinds"), and names the
  re-enrollment command for every worker whose own coordinator entry changed,
  not only for the workers a project change touched.

- The `steward-fleet-configuration` component reads the coordinator's reload
  receipt (`~/.local/state/t3-steward/coordinator/reload-receipt.json`)
  after it sends SIGHUP for a changed coordinator catalog, instead of
  reporting the signal as done. It waits up to 30 s (polling every 500 ms)
  for a receipt whose `requestedAt` is at or after the signal: `accepted` or
  `unchanged` converge with the digest in `coordinator_reload` and the action
  "coordinator reloaded its configuration (digest ...)"; `rejected` fails the
  component with the coordinator's error and every blocking assignment, one
  per line; no receipt in time fails it as unverified, naming
  `t3-steward coordinator reload --json` to run on the host and saying
  whether a receipt file existed at all. A dry run and an inactive unit are
  unchanged. `fleet-plan` says, under any change that alters a worker's
  catalog revision (a project it is eligible for, or its own models, quota
  pools, executor slots, capabilities, cpu class or provider instances), that
  the coordinator refuses the reload while that worker holds a retained
  assignment and that the receipt names each one (F-3).

### Fixed

- A rejected coordinator reload no longer leaves the refused projection on the
  host. The `steward-fleet-configuration` component puts back the
  `coordinator-fleet.json` the run replaced, or removes the file when the run
  created it, and says which in the failure beside the coordinator's error and
  blockers. The component still fails, and the coordinator's own configuration
  is untouched either way: it never loaded the refused document. Before this,
  the refused bytes stayed on disk and were what the coordinator read the next
  time it started, so a reload that was merely rejected became a coordinator
  that could not load its configuration at all, with `t3-steward check` failing
  and the steward pair gate that would repair the host blocked behind that
  check. A reload whose verdict never arrived is unchanged: the projection
  stays, because an unverified reload does not say whether the coordinator
  loaded it, and reverting it could silently undo a catalog the coordinator is
  running (R7).
- The Huyang user unit is no longer ordered `After=default.target`. It is
  wanted by `default.target`, so that line put every unit ordered after
  `huyang.service` in a cycle with `default.target` (a target is implicitly
  ordered after the units it wants, so the unit alone already closes one), and
  systemd broke the cycle at boot by deleting `t3-steward-worker.service/start`
  (and at shutdown by deleting `huyang.service/stop`), leaving the worker down
  after every reboot. Both bootstrap renderers drop the line. Because the
  bootstrap never replaces an existing unit, the `huyang` component now removes
  `default.target` from the `After=` lines of an installed unit's `[Unit]`
  section, whether or not the checkout exists. Lines it does not rewrite stay
  byte-exact, a rewritten `After=` line is normalised, the file mode and the
  drop-ins are kept, and nothing inside a value continued with a backslash
  (comment lines within it included) is rewritten. It
  runs `systemctl --user daemon-reload` and does not restart Huyang for it; a
  marker under `~/.local/state/dev-fleet/unit-migrations/` makes a failed
  reload retried by the next run. The component then checks every installed
  user unit under `~/.config/systemd/user`: wanted by `default.target` through
  `WantedBy=` in the unit or a drop-in or an entry in `default.target.wants/`,
  and ordered after it through `After=` in the unit or a drop-in, continuation
  lines joined. A cycle left in Huyang's own unit fails the component; a cycle
  in any other unit is a warning naming the unit and the fix, and `status`
  reports it as drift (`user_unit_ordering_cycles`, and `unit_ordering_cycle`
  for Huyang's unit), without stopping later components. `status` also
  reports drift while a reload is pending (`unit_reload_pending`) and when
  the ordering could not be observed.
  `systemd-analyze --user verify` does not report such a cycle (P0.1).
