# Authored release guide: a binary-only Steward bump

This is the procedure for the most common release: move the fleet to a new Steward
version and change nothing else. It is written against the commands as they behave
now, and every step says what it refuses and why. The background for each command
is in [the release guide](upkeeper-push-pull.md); this page is the order to run them in.

The procedure has six stages: fence the fleet, author and preview, assemble the review
package, publish, canary, and release to the fleet. Rollback is at the end.

Commands run from the operator's own release checkout (`~/Work/dev-fleet`), on the
controller you publish from, unless a step names a host.

## 0. Before you start

- The release checkout must be on `main`, clean, and level with every remote:

  ```bash
  git switch main
  git pull --ff-only origin main
  git fetch --all
  git log -1 --format=%H origin/main; git log -1 --format=%H gitea/main
  ```

  The two commits must be the same. `push` checks this again before it does
  anything (step 4), but finding out now costs nothing. If one remote lags, push the
  newer main to it, for example `git push gitea refs/remotes/origin/main:refs/heads/main`,
  and check again. If the histories have diverged, stop and reconcile them by hand;
  never force-push release intent.
- Note the release commit currently on `main`. It is the rollback target:

  ```bash
  PREVIOUS=$(git rev-parse HEAD)
  ```

- Decide the canary host. The examples below use `omarchy-pc` as the canary and
  `laptop`, `normandy` and `homelab` as the rest of the fleet.

## 1. Fence the fleet with an operator hold

Every host converges on its own `upkeeper-self.timer`, which also runs on startup. A
release published to `main` is therefore consumed by every host within a day, or at
its next boot, unless something stops it. Hold every host except the canary:

```bash
upkeeper hold set --hosts laptop normandy homelab \
  --reason "Steward 0.11.0-rc.120 canary on omarchy-pc" --until 48h
upkeeper hold show
```

`hold set` records each hold in this host's state and in each held host's own state
(`~/.local/state/dev-fleet/operator-holds/HOST.json`), so it survives reboot and every
path honours it:

- the held host's own `self-pull` refreshes nothing, updates no UpKeeper binary and
  converges nothing; the run is recorded with the host reported `held`;
- `scheduled-pull` and every other controller's pull report the host `held` and
  dispatch nothing to it;
- an explicit `upkeeper pull` naming a held host is refused whole unless it is given
  `--override-hold`, and every override is logged in `HOST.log` beside the hold, on
  the controller and on the target.

Dry runs and `status` are never held. `rollback` is not held either: it is recovery,
not consumption of a new release.

`--until` takes an RFC 3339 time or a duration; without it the hold lasts until it is
cleared. An expired hold stays listed by `hold show` and stops nothing.

If a host cannot be reached, `hold set` keeps this host's copy, prints `held here
only`, and exits 20. This host's pulls honour that copy, but the unreachable host's
own timer does not, so run the command again once the host is up. A host whose
UpKeeper binary predates operator holds is reported as such; until its binary is
updated, stop its timer by hand (`systemctl --user stop upkeeper-self.timer`) and
remember that a reboot re-arms an enabled timer.

## 2. Author and preview the release

`author` moves the Steward pin, and the omarchy-setup pin when you give a new commit,
without capturing any host. For a binary-only bump, pass the omarchy-setup commit the
release already pins:

```bash
SETUP=$(jq -r '.components["omarchy-setup"].commit' desired/release.json)
./bin/upkeeper author --steward 0.11.0-rc.120 --omarchy-setup "$SETUP" --dry-run
./bin/upkeeper author --steward 0.11.0-rc.120 --omarchy-setup "$SETUP"
```

The dry run writes nothing. The real run rewrites `desired/release.json` only, and
prints the new release digest and the exact `push --authored` command to use. A
binary-only bump should report one pin change (`t3-steward`) and no file changes; if
it reports file changes, the omarchy-setup commit is not the one the release pins.
`author` does not move the T3 version or its tested range: a Steward release that
changes them still needs a capture.

Commit the reviewed bytes on `main`:

```bash
git commit -m "Pin Steward 0.11.0-rc.120" -- desired/release.json
HEAD_COMMIT=$(git rev-parse HEAD)
DIGEST=<release digest printed by author>
```

Do not author on a side branch. A publication always updates `main` on every remote,
and `push` refuses a checkout on any other branch rather than publishing the release
under that branch's name.

## 3. Assemble the review package

What a reviewer needs to accept the release, all of it read-only:

```bash
git show --stat HEAD
git diff "$PREVIOUS" HEAD -- desired/release.json
./bin/upkeeper fleet-plan --previous "$PREVIOUS"
./bin/upkeeper pull --hosts omarchy-pc --dry-run
./bin/upkeeper status --json > status-before.json
```

- the diff shows that only `t3-steward.version` and `t3.compatibility.steward_version`
  moved;
- `fleet-plan` shows the authorization diff, which must be empty for a binary-only bump;
- the dry-run pull shows what the canary would do, and is not held;
- `status` records the starting point. It exits 10 or 20 when anything anywhere is
  not converged. For a gate, list the states you already know about and accept in an
  expectations file and pass it with `--expect` (see [Status as a gate](#status-as-a-gate)).

## 4. Publish

```bash
./bin/upkeeper push --authored --expect-head "$HEAD_COMMIT" --expect-release-sha256 "$DIGEST"
```

No remote names: a publication goes to `main` on every configured remote, and naming
only some of them is refused. Before anything is staged or committed, `push`:

- refuses a checkout that is not on `main`, or a detached `HEAD`;
- reads `main` on every remote and refuses when they differ, naming each remote and
  its commit. When one remote's `main` contains all the others, the refusal names the
  `git push` that brings the others level; otherwise it says the histories diverged;
- refuses when the remotes' `main` is not an ancestor of `HEAD`, naming the
  `git pull --ff-only` that fixes it;
- checks both fences: `HEAD` must be `--expect-head` and the release digest must be
  `--expect-release-sha256`.

It then pushes the release commit to `refs/heads/main` on each remote in turn and
prints a receipt per remote. A push that fails part way reports `partial-failure`
with the remotes that were and were not updated; resolve it before going on, because
the remotes now disagree and the next publication will refuse until they do not.

Wait for the release commit's CI push gate on GitHub to pass. Hosts consume only a
`main` whose exact commit passed it.

## 5. Canary

Converge the canary now rather than waiting for its timer:

```bash
ssh omarchy-pc systemctl --user start upkeeper-self.service     # or, on the canary itself
upkeeper status --limit omarchy-pc
```

The canary's self-pull fetches the gated `main`, updates its own binary when the
release pins a new one, and converges. Read its journal and its run record under
`~/.local/state/dev-fleet/runs`, and check the Steward version it reports. A pushed
commit or a skipped host is not proof of convergence.

If the canary is wrong, go to [Rollback](#rollback) while the rest of the fleet is
still held.

## 6. Release to the fleet

Release the holds and let the hosts converge:

```bash
upkeeper hold clear --hosts laptop normandy homelab --reason "rc.120 canary green on omarchy-pc"
```

Each host converges on its next timer run. To converge now, start
`upkeeper-self.service` on each host as for the canary. Then gate on status with the
expectations file:

```bash
upkeeper status --expect release-expectations.json
```

It exits 0 only when every non-converged state on every host is one the file
accepts.

## Rollback

After a failed canary, retain the entire fleet fence until a corrective release is
verified. First replace any expiring holds with indefinite holds on every host,
including the canary, before rolling it back:

```bash
upkeeper hold set --hosts omarchy-pc laptop normandy homelab \
  --reason "canary rejected rc.120; awaiting verified corrective release"
upkeeper hold show
```

Confirm delivery to every host; a `held here only` result is not a fleet fence (see
step 1). Do not clear any hold while `main` still pins the rejected release. Holding
only the rolled-back canary does not protect the other hosts from timers or reboot.

While the fleet is held, only the canary has moved. Roll the canary back to the
previous release:

```bash
upkeeper rollback --to "$PREVIOUS" --hosts omarchy-pc --components t3-steward --dry-run
upkeeper rollback --to "$PREVIOUS" --hosts omarchy-pc --components t3-steward
```

`rollback` reads the release at `$PREVIOUS` from Git history and passes through
operator holds, logging the pass. It does not move `main`: the canary's own timer
would take the rejected release again without its hold. Keep all hosts held while
authoring a corrective release as in step 2 with the previous Steward version.
Review its full release diff and fleet preview as in step 3, then publish it to
`main` on every configured remote as in step 4. Never rewrite `main`.

Before clearing any hold, confirm that every remote's `main` points to the reviewed
corrective release restoring the safe pin and that GitHub CI passed for that exact
corrective release commit. If correction is deferred, leave the entire fleet held
indefinitely; completing the local rollback alone never authorizes clearing holds.
Verify the corrective release on the held canary with an explicit override:

```bash
upkeeper pull --hosts omarchy-pc --override-hold
upkeeper status --limit omarchy-pc
```

Read the canary's journal and run record and verify the restored Steward version.
If publication, CI or canary verification fails, retain every hold.

If the UpKeeper binary itself is the problem, `upkeeper self rollback` on the host
makes the previous binary generation current again; see [the binary guide](go-binary.md).

Only after the corrective publication, exact-commit CI and canary verification
above succeed, clear the holds so the fleet resumes following the corrected `main`:

```bash
upkeeper hold clear --hosts omarchy-pc laptop normandy homelab \
  --reason "corrective release reviewed, exact-commit CI passed, canary verified"
```

Then verify the remaining hosts as in step 6.

## Status as a gate

`upkeeper status` exits 0 when every host is converged, 10 when something drifts and
20 when a host failed or could not be observed. On a real fleet some of that is known
and accepted: a host that is asleep, a component deliberately left behind. An
expectations file names exactly those states, each with the reason it is accepted:

```json
{
  "schema_version": 1,
  "accepted": [
    {"host": "laptop", "state": "unavailable", "reason": "laptop is asleep during the rollout"},
    {"host": "homelab", "component": "t3", "state": "drift",
     "reason": "T3 0.0.40 stays on homelab until its threads drain"}
  ]
}
```

```bash
upkeeper status --expect release-expectations.json
```

- An entry without `component` accepts the host's own result (`unavailable`, `failed`
  or `drift`).
- An entry with `component` accepts that component in exactly that state. A host is
  accepted only when every component that is not converged or ahead is accepted; one
  unexpected component fails the host.
- Every host must be in the inventory, every component must be a release component,
  and every entry needs a reason; a malformed file is refused before any host is
  reached.
- The report lists what was accepted (`accepted`) and the entries that matched
  nothing (`expectations_unused`), which usually means the accepted state has gone
  and the entry should be removed.

Without `--expect`, `status` behaves exactly as before.
