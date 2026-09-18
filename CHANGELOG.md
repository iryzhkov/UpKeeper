# Changelog

Notable changes to UpKeeper are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are the
release tags (`v0.1.x`).

## [Unreleased]

### Added

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
