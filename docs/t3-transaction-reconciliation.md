# Controlled T3 transaction custody and reconciliation

A controlled T3 activation runs as a retained transaction under
`<npm prefix>/lib/node_modules/.upkeeper-t3-*`, with its record in
`transaction.json`. This page describes what that record keeps when an
activation fails and how to close one that rollback left at `files-restored`.

## What a failed transaction retains

- `cause` is the original failure, redacted and bounded. It is journaled at
  `restoration-required`, before any rollback effect, and is never replaced by
  the restoration result.
- `failed_samples` holds each failed readiness sample: the installed, served,
  health, artifact-installed, artifact-served (native descendant), unit binding
  and dependent-unit predicates, which of them failed, the stage
  (`post-restart`, `restoration`) and the time. At most twelve are kept; the
  first failure of each stage always survives.
- `restoration` is the result of the rollback (`files restored; previous service
  health not verified`, and so on). Every error returned after a failure names
  both the restoration result and the original cause.
- `service`, `binding_sha256`, `prior_active` and `prior_served` record the unit,
  its launcher binding and the serving state the transaction found.

After a restart, readiness is a bounded series of complete samples (six, five
seconds apart), not one early sample. Unit binding drift and installed identity
drift end the probe at once. Units bound to the T3 unit with `PartOf=` (systemd
reports them as `ConsistsOf=`), such as the Steward service, restart with T3:
each that was active must come back active under a new `InvocationID`, and an
unknown dependent identity refuses the restart.

A record written before this change has none of these fields. Its original
cause was not retained and cannot be reconstructed.

## Reconciling a files-restored transaction

`files-restored` means the original files are back but the previous service
was not verified. Once the service is healthy again, close the transaction
explicitly, from the controller, for one host:

    upkeeper pull --hosts HOST --components t3 \
        --reconcile-t3-transaction .upkeeper-t3-NNNN --dry-run
    upkeeper pull --hosts HOST --components t3 \
        --reconcile-t3-transaction .upkeeper-t3-NNNN

The dry run performs every check and writes nothing. The real run restarts
nothing and moves no file; it only rewrites `transaction.json` to phase
`restored`, adding a `reconciliation` record and embedding the previous record
and its SHA-256 under `reconciled_from`. It proceeds only when all of these
hold, and otherwise refuses with the journal and every retained tree unchanged:

- the transaction directory is an owner-only directory beside the current
  package, and its record is at `files-restored` and names the current prefix,
  package, launcher, provenance path and unit;
- the live package tree, launcher and provenance equal the transaction's
  original digests, and no `current-previous` recovery backup exists;
- the retained failed tree (`failed`) and failed provenance and receipts
  (`failed-.upkeeper-t3-NNNN`) equal the transaction's new digests, and the
  original backup was restored;
- the unit binding selects the owned launcher and, where the record has one,
  equals `binding_sha256`;
- the service serves the prior identity healthily within the bounded probe.
  For a record without `prior_served` the prior version is the one the
  unchanged original tree installs.

Repeating the command on a reconciled transaction reports `converged` and
writes nothing. Nothing is deleted: the failed tree, failed provenance and the
transaction directory stay for operator-reviewed cleanup.
