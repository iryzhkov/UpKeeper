# Controlled T3 transaction custody and reconciliation

A controlled T3 activation runs as a retained transaction under
`<npm prefix>/lib/node_modules/.upkeeper-t3-*`, with its record in
`transaction.json`. This page describes what that record keeps when an
activation fails and how to close one that rollback left at `files-restored`.

## What a failed transaction retains

- `cause` is the original failure, redacted as a whole and then bounded, so a
  secret straddling the bound is still removed. The output of a failed
  transaction command, such as the T3 restart, is likewise redacted complete
  before it is cut to its tail, so no fragment of a long secret value survives
  without the label redaction needs. It is journaled at
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
- `dependents` is the census of `PartOf=` dependents taken before any effect:
  the `InvocationID` of each active unit, and an empty value for each known
  inactive one. The active ones are restoration obligations.

After a restart, readiness is a bounded series of complete samples (six, five
seconds apart), not one early sample. Unit binding drift and installed identity
drift end the probe at once. Units bound to the T3 unit with `PartOf=` (systemd
reports them as `ConsistsOf=`), such as the Steward service, restart with T3:
each that was active must come back active under a new `InvocationID`. A
dependent whose activity or identity cannot be determined (for example, the
user bus is unreachable) is unknown, not inactive, and refuses the activation
before any effect. Rollback keeps every dependent that was active when the
transaction began as an obligation, even one that stopped after the failed
restart, and does not report the service restored until each is active again.
This holds when the T3 unit was inactive before the activation too: `PartOf=`
does not stop a dependent from running on its own, rollback adds no restart to
bring one back, and an original dependent not active again leaves the
transaction at `files-restored`.

Each journal record is published by atomically exchanging it with the record
it supersedes (`renameat2` `RENAME_EXCHANGE`) and checking that the displaced
record is the one expected: the same inode with the same bytes. If another
writer replaced or changed the record meanwhile, the exchange is undone and the
write refuses. If the directory sync after publication fails, the new record is
withdrawn only while the journal is still exactly that record; a newer record
from another writer is kept, together with the superseded record (named
`.journal-*-previous` in the transaction directory) that recovery needs.

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
nothing, moves no file and skips the legacy updater retirement every other pull
performs; it only rewrites `transaction.json` to phase
`restored`, adding a `reconciliation` record and embedding the previous record
and its SHA-256 under `reconciled_from`. It proceeds only when all of these
hold, and otherwise refuses with the journal and every retained tree unchanged:

- the transaction directory is an owner-only directory beside the current
  package, and its record is at `files-restored` and names the current prefix,
  package, launcher, provenance path and unit;
- the transaction directory and `transaction.json` are the same directory and
  file, with the same bytes, as when reconciliation read them, at every fence
  and again immediately before the new record is published; a record changed
  or replaced by another writer is refused and left as that writer left it;
- the live package tree, launcher and provenance equal the transaction's
  original digests, and no `current-previous` recovery backup exists;
- the retained failed tree (`failed`) and failed provenance and receipts
  (`failed-.upkeeper-t3-NNNN`) equal the transaction's new digests, and the
  original backup was restored;
- the unit binding selects the owned launcher and, where the record has one,
  equals `binding_sha256`;
- the service serves the prior identity healthily within the bounded probe.
  For a record without `prior_served` the prior version is the one the
  unchanged original tree installs;
- every dependent the record's `dependents` census found active is active now.
  A record without `dependents` needs a known current census, which is recorded
  in the reconciliation but not required to be active, because the original
  state was not retained.

Repeating the command on a reconciled transaction reports `converged` and
writes nothing. Nothing is deleted: the failed tree, failed provenance and the
transaction directory stay for operator-reviewed cleanup.
