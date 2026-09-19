# Changelog

Notable changes to UpKeeper are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are the
release tags (`v0.1.x`).

## [Unreleased]

### Added

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
