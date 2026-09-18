# Changelog

Notable changes to UpKeeper are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are the
release tags (`v0.1.x`).

## [Unreleased]

### Changed

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
  unchanged. `fleet-plan` says, under a coordinator catalog change, that the
  coordinator refuses the reload while a worker whose catalog changes holds a
  retained assignment and that the receipt names each one (F-3).
