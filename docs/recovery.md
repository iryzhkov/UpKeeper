# Recovery and off-homelab copy

Forgejo at `git.ryzhkov.dev` is the primary collaboration remote and must be named `gitea`.
It runs on `homelab`, so it cannot be the only recovery copy.

The Phase 1 recovery design is implemented as a private recovery repository at
`git@github.com:iryzhkov/UpKeeper.git`, named `github` locally. It was created and seeded from
Normandy. GitHub is not a deployment source unless it is explicitly listed and its repository
identity is verified. Normal work pushes to `gitea` first; the recovery remote receives the same
reviewed commit after that succeeds. Local `main` tracks `gitea/main`.

A recovery drill is:

```bash
git clone --origin github git@github.com:iryzhkov/UpKeeper.git dev-fleet-recovery
cd dev-fleet-recovery
git fsck --full
git verify-commit HEAD  # required once signing is enabled; until then record that it is unsigned
git remote add gitea git@git.ryzhkov.dev:igor/UpKeeper.git
git remote -v
```

The drill must run outside homelab and compare the recovered commit to the last known mirrored
commit. It may not silently promote the mirror to primary or deploy an unreviewed mirror-only
commit. No scheduled mirror job is installed in Phase 1; that would add credentials and an
unattended service outside this session's scope.

If GitHub mirroring cannot be created safely, create an encrypted git bundle in a later explicit
operation. Its age private key must not live only in the house, and restore testing is part of the
design rather than merely producing a bundle.

## Recovering a host from a failed scheduled run

A scheduled run that failed, died or lost its output leaves
`~/.local/state/dev-fleet/scheduled-holds/<host>.hold` on the host that dispatched it, and
every later scheduled run refuses that host until the marker is cleared. The steps are:

1. Read the failure. The journal of `upkeeper-self.service` (or `upkeeper-pull.service` on
   Normandy) carries one stderr line per failed component, the run record path and the exit
   code; the run record under `~/.local/state/dev-fleet/runs/` carries the full report.
2. Read the hold with `upkeeper hold show`. It names the run that left it.
3. Fix what the failure names. The T3 pair refusals say which of three things happened: the
   running T3 is outside the candidate steward's tested range (install a T3 in the range or
   publish a steward tested with the running one), the T3 version could not be observed (T3 is
   not serving or the steward cannot reach it), or the program the T3 unit starts is missing
   from disk, which is what a Node upgrade under mise leaves behind (reinstall T3, restart
   `t3code.service`).
4. Clear the marker with `upkeeper hold clear <host> --reason "what you observed and fixed"`.
   The reason, your user and the time are appended to `<host>.hold.cleared`.
5. Run `upkeeper self-pull` (or wait for the timer) and confirm with `upkeeper status`, which
   prints a reason beside any component still drifting.

A release commit for `rollback --to` or `fleet-plan --previous` is any revision git resolves
to a commit in the checkout's history: a hash, a tag such as `v0.1.8`, a branch or `HEAD`.

## Coordinator reload rejected

A pull that changes the coordinator's catalog input fails its `steward-fleet-configuration`
component with "coordinator rejected the reload" when the coordinator refused to activate the
new catalog. The coordinator keeps serving the previous configuration, and the projection
file is put back to the one the run found, or removed when the run created it, so the file
and the running coordinator agree and a restart of the unit reads a configuration it can
load. The failure says which of the two happened, and quotes the coordinator's receipt: the
error, then one line per blocking assignment with its worker, assignment, attempt, the
attempt's state and the action that unblocks it. Typically a worker whose catalog changes
still holds a retained assignment, paused or parked. The steps are:

1. Read the blockers in the run record, or on the coordinator host with
   `t3-steward coordinator identity --json` (field `lastReloadReceipt`).
2. Unblock each one as the receipt says: wait for a paused attempt's pause to lift or a
   parked attempt's wait to settle, or cancel its task with
   `t3-steward backlog cancel <run>/<task> --reason TEXT`.
3. Re-run the pull for that host, `upkeeper pull --hosts <coordinator>`. The rejected reload
   rolled the projection back, so the release's catalog is no longer on the host at all: asking
   the coordinator again with `t3-steward coordinator reload --json` would re-read the *old*
   projection and answer `accepted` or `unchanged` with exit 0, which says nothing about the
   release and leaves its catalog unapplied. The pull writes the projection again, the file
   therefore changes, and the component signals the coordinator and reports the new verdict.
   Use the reload verb only to get a verdict on the file that is already on the host.
4. Once that reload is accepted, re-enroll the touched workers as `fleet-plan` listed them.

If the failure instead begins with "rollback failed", the refused projection could not be
replaced: it is still the file the coordinator would read at its next start. Put the retained
document under `~/.local/state/dev-fleet/fleet-config/<run id>/prior.json` back by hand, or re-run the
pull once the cause (a full or read-only filesystem, usually) is gone, before that unit is
restarted.

A component that failed as "coordinator reload unverified" got no receipt within 30 seconds:
run `t3-steward coordinator reload --json` on the host to see the verdict. If the message
says no receipt file exists, the coordinator predates the receipt (t3-steward before rc.69)
and its journal (`journalctl --user -u t3-steward.service`) is the only record of the reload.
