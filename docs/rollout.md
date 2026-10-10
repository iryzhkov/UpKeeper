# rc122 native rollout candidate

The rc122 command requires separate parent release review and publication gates.
Source validation does not establish installed UpKeeper or Steward readiness.
The historical rc121 configuration exception is revoked. No failed producer,
including a full preview returning exit 40, can supply an acceptance gate.

## Supported commands and order

Use an isolated release checkout and a private absolute StatePath. Preserve that
directory, its JSON checkpoint, permanent .attempts inventory, original backups
and review package. Configure the intended targets including laptop. Check and
plan are read-only:

```sh
upkeeper rollout check --config rollout.json
upkeeper rollout plan --config rollout.json --json
upkeeper rollout prepare --config rollout.json --yes
```

Preparation fences timers and stops after the unpublished candidate/package.
The parent reviews its exact head, canonical fleet-plan digest, full asset
disposition and PACKAGE digest. It supplies the external ReviewFile whose first
line is VERDICT: ACCEPT. Host execution checks that review and publishes the
authored candidate before its exact release CI gate. Every mutation requires
--yes. Parent release authorization is recorded in FleetApproval; Igor need
not attend the maintenance window.

```sh
upkeeper rollout run --config rollout.json --host agent-b --yes
upkeeper rollout run --config rollout.json --host normandy --yes
upkeeper rollout run --config rollout.json --host homelab --yes
upkeeper rollout run --config rollout.json --host agent-a --yes
upkeeper rollout run --config rollout.json --host omarchy-pc --yes
upkeeper rollout enroll --config rollout.json --host agent-b --yes
upkeeper rollout enroll --config rollout.json --host agent-a --yes
upkeeper rollout enroll --config rollout.json --host omarchy-pc --yes
upkeeper rollout verify --config rollout.json --yes
```

Each run mutates exactly its named rollout host, verifies its binary/process
generations, unchanged T3 generation, drains and epoch, and monitors readiness,
activity and collection. The canary interval is at least 30 minutes; each other
host requires 15 minutes. Samples must remain within 60 seconds of one another,
and the receipt binds host, configuration, release, epoch identity and elapsed
interval. Restarting an interrupted observation cannot count an unobserved gap.
A later host requires its predecessor's valid receipt. Completed hosts refuse
repetition. No hidden production loop advances to a subsequent host.

## Configuration contract

Config is strict JSON. Preserve the existing Version, PreviousRelease (full
immutable SHA), repositories, Checkout, SetupRepository, StatePath, transport
Targets, Remotes, TimerHosts, DrainedHosts, ActiveWorkers, QuietCommand, backup
age (1..3600 seconds), readiness (60..180 seconds), SoakSeconds (>=1800),
ReviewFile and FleetApproval fields. The fixed rollout order is agent-b,
normandy, homelab, agent-a, omarchy-pc, with normandy coordinator and omarchy-pc
last. The laptop is excluded from this Steward walk and enrollment, but is
required in full parent release preview/disposition and must remain masked.

Additional required rc122 inputs:

- CoordinatorConfig: BeforeSHA256, AfterSHA256, exact Content and
  ValidateCommand argv. At least one validator argument contains {config};
  it is replaced by the private staged path. The command must be a separately
  verified supported configuration validator; do not substitute true.
- ActivityCommand: reviewed argv adapter, given host and coordinator epoch as
  final arguments. Successful stdout is strict JSON containing host,
  coordinator_epoch, collection_deferred:false and
  running_without_activity:false. Unknown fields, missing booleans, wrong
  host/epoch, trailing JSON or nonzero producer status refuse.
- HaikuCheckCommand: reviewed argv adapter given worker and epoch, returning
  strict JSON worker_id, coordinator_epoch and
  model:"claudeAgent/claude-haiku-5-5". Status is checked before parsing.
- DropinTestCommand: the parent-pinned isolated Steward test command on each
  drop-in host. DropinAbsentHosts explicitly identifies an already removed
  rc121 drop-in; absence never invents an original backup.
- AcceptedEffects: exact Host, Component, State, ordered Actions and nonempty
  Reason for every full-preview effect, including converged components with
  actions. All intended Targets including laptop must be present, components
  uniquely inventoried, failure-free and every accepted entry matched.

The supplied historical evidence does not define a supported machine-readable
activity/Haiku adapter or a configuration validation CLI. Those exact adapter
commands and their source/contract verification remain parent readiness gates.
The candidate refuses without them. Historical configuration bytes are
review inputs, not installation authority. Keep secretrefs as references.

## D2 transaction

Immediately before a coordinator backup can stop/restart the old binary, its
live config must equal the reviewed before digest. Backup requires quiet,
both stopped database holders, a fail-closed same-uid /proc inventory comparing
device/inode (including hardlinks), verified snapshot and scratch integrity/FK,
and nonfuture age within the configured hour. Unreadable/non-dumpable inventory
or a vanished descriptor from a live process refuses.

A durable configuration intent precedes any remote installation. The original
config.yaml.bak-rc122-pre is created exclusively, hashed and never overwritten;
its owner/group/mode are preserved. The staged after content is hashed, fsynced,
validated with checked status, rechecks the live digest, then is atomically
installed and directory-synced. Retry only verifies a completed matching
transaction with the original before backup; an unfinished effect never
authorizes installation again. Corrupt/future/mismatched evidence refuses.
Coordinator recovery restores and verifies the original configuration before
dispatching immutable binary rollback.

## D5 and recovery

Named enrollment preserves drains; it never uses --all. Agent-b's D5 completion
precedes agent-a. Both must advertise Haiku 5.5 and have no running attempts.
Drop-in repair records an exclusive original backup/digest/metadata before
removal, reloads/restarts the worker, verifies readiness and runs the isolated
test command. Any failed or uncertain check restores the original and leaves
the attempt failed; it cannot authorize the next worker. Already absent rc121
drop-ins require explicit reviewed absence and do not create fictional backups.

A host failure rolls back only that host and leaves a terminal attempt. Final
enrollment/verification failures stop; they do not silently trigger a fleet
rollback policy. Interrupted/ambiguous work cannot resume as success. The
supported recovery command is limited to saved intent for the named host:

```sh
upkeeper rollout recover --config rollout.json --host HOST --yes
```

Recovery never erases an attempt fence, marks success, advances a host or
enrolls. A fresh rollout attempt requires a newly reviewed parent plan/state,
with prior evidence retained. For a database incompatible with the old binary,
the parent supervises stopped-holder proof, preservation of failed
DB/sidecars/artifacts, restore into absent targets, restart and verification.
Enrollment recovery is forward and per-worker, not a broad snapshot rollback.

The permanent invocation intent is durable before effects and independent of
JSON replacement. Completion is an unbuffered acknowledgement appended only
after all checked JSON writes/fsync/rename/directory sync and gates succeed.
Loss of that last acknowledgement on power failure refuses continuation.
Unknown inventory, missing acknowledgement or success-shaped bytes left by a
failed persistence operation do not authorize the next host.

Timers, schedules, operational holds and laptop mask retain their fences.
Timer release requires a separate full parent release disposition. Tests use
temporary HOME/state, injected clock and transport; they prove no deployment,
backup freshness on the real coordinator, or real elapsed soak.
