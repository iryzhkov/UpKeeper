# Phase 1 architecture

This document describes the read-only discovery interface. Deployment now exists
through UpKeeper; see [the current system model](architecture/system-model.md) for
state ownership, push/pull transactions and the historical F03 saga API, retired
from new Python 0.2.0 installs.

## Boundary

Phase 1 is a read-only control plane. `fleet doctor` verifies whether a controller is safe
to use; `fleet status` reports desired, installed, activated, and running state without
changing it. The repository deliberately has no deploy, promote, unlock, repair, resume, or
reconcile command yet.

A host is local only when the SHA-256 digest of `/etc/machine-id` matches its inventory
entry. Every other host is reached with an argument-vector SSH invocation using
`BatchMode=yes`, `StrictHostKeyChecking=yes`, and bounded connection and probe timeouts.
The remote probe is a fixed standard-library Python program supplied on stdin. Inventory
values never become shell fragments.

The probes read OS identity, command versions, git HEAD/status, systemd user state, file
metadata, disk capacity, source reachability, lock names, and agent99 process paths. They do
not invoke package managers, create directories, fetch/update a checkout, read secret file
contents, call `systemctl start/stop/restart/reload`, or run a mutating Ansible task.

## Sources of truth

`inventory/hosts.yml` and `desired/components.yml` are JSON-compatible YAML. This is
intentional: Ansible accepts them as YAML, while the pre-bootstrap CLI can parse them with
Python's standard library. Full validation occurs before any network process starts.

Deployment remains disabled while a component pin or infrastructure decision is unresolved.
The CLI rejects unknown host selectors, wildcard/expression selectors, unapproved source
hosts, non-HTTPS canonical URLs, credential-bearing URLs, abbreviated or symbolic git refs,
absolute paths, traversal, control characters, and paths outside `.config` or `.local`.

## Status schema

Both JSON commands use `schema_version: 1` and include:

- command, run ID, UTC generation time, controller, manifest commit, result, and stable exit code;
- one entry per selected canonical host, in selector order;
- explicit reachability and transport;
- doctor checks with actual and expected values;
- status components with separate `desired`, `installed`, `activated`, and `running` fields;
- lock names, never arbitrary lock-file content.

Unknown running versions are `null`; they are not inferred from the current file on disk.
An offline host is `partial`, never success. An unpinned or mismatched component is drift.

## Runs and records

Run IDs are sortable UTC timestamps plus 32 bits of random entropy. Doctor and status hold
their run IDs in memory and write nothing. `RunRecordStore` is a future mutation primitive:
when explicitly called, it redacts structured and free-text secrets, writes mode 0600 through
an fsynced temporary file, and atomically replaces the record. Its default location is
`~/.local/state/dev-fleet/runs/`.

## Deployment locks

The lock design is implemented and tested but unused by Phase 1 commands:

1. Validate and sort canonical target names.
2. Atomically create each host lock in deterministic order.
3. Write an owner record containing run ID, controller, PID, manifest commit, component set,
   and start time.
4. On contention, release only locks acquired by the same run.
5. Release requires an exact run-ID match.
6. Age alone never makes a lock stale. It is stale only after the age bound and a positive
   confirmation that the recorded controller/PID is dead. Unreachable or uncertain remains held.
7. Future remote acquisition must implement the same protocol on each target and release earlier
   targets if any acquisition fails.

No command may automatically delete an invalid or apparently live lock.

## Ansible

`playbooks/status.yml` contains only read-only command/stat/debug tasks and is exercised in
check mode against localhost in CI. Real-fleet production CI is forbidden. Agent harnesses on
this fleet expose non-blocking stdout/stderr, which Ansible rejects; local agent-run gates
redirect both streams to a file before reading the bounded result.
