# Scheduled convergence and drift

The fleet now uses [independent local timers](fleet-self-convergence.md).
The Normandy timer below is the legacy central mode; disable it after migration.

Citadel U3 adds one daily systemd user timer on Normandy. Install from a clean,
GitHub-gated main checkout at `~/Work/dev-fleet` with `upkeeper install-pull-timer`.
It installs `upkeeper-pull.service` and `upkeeper-pull.timer` from the unit text
embedded in the binary, scheduled at 06:30 local time with up to ten minutes of
jitter and catch-up after downtime. A legacy canary drop-in UpKeeper shipped in
`upkeeper-pull.service.d` is removed and reported; any other drop-in refuses.

The timer calls `~/.local/bin/upkeeper scheduled-pull`. It requires Normandy's inventory
machine-id, refuses a dirty controller and freezes the release and companion files
from its own HEAD. It does not fetch, capture, promote pins, or follow tool releases.
New versions become eligible only through a committed release promotion.
Normal pulls preserve Huyang checkouts ahead of the pin rather than implicitly
rolling newer persistent state back. Explicit rollback remains an operator action.
If the Huyang checkout is absent, UpKeeper clones the declared source, verifies
pin ancestry, builds in staging, installs the checkout and seeds its missing user
service. Existing non-Git paths and units are preserved and reported for inspection.
Failed builds never activate; failed fresh service starts retain diagnostic files
and disable the newly created unit. Python, Git, Go and make remain OS/toolchain
prerequisites supplied by omarchy-setup.

The runner uses the same pull transaction with all inventory hosts. A strict SSH
`true` probe skips network-unreachable hosts before dispatch. Authentication and
host-key failures remain failures. A timeout after deployment dispatch is uncertainty,
never an offline skip. Each host uses the normal controller and target locks.

Before scheduled dispatch, a private durable marker is created at
`~/.local/state/dev-fleet/scheduled-holds/<host>.hold`. An understood completed
success, ahead, deferred or lock-contention response clears it. Process death,
lost responses and failed deployment leave it in place. Later scheduled runs refuse
that host until an operator observes its state and removes that exact marker.
Do not remove a hold just to retry. Manual pull remains an explicit operator action.
Other hosts can still converge independently.

The marker names the run that wrote it and the command that clears it.
`upkeeper hold show [--json]` lists every marker on the host that keeps it, with
its content, age and, when the local run store has that run, the run record and
its result. `upkeeper hold clear <host> --reason TEXT` removes exactly that marker
and appends who cleared it, when, why and what the marker said to
`<host>.hold.cleared` beside it. A clear without `--reason`, or for a host with no
marker, exits 2 and changes nothing. Markers live where the dispatch ran: a
self-converging host keeps its own, Normandy keeps the fleet timer's.

A scheduled run's stdout is one JSON document. Its failures are repeated on
stderr, one line per failed host or component with the untruncated first line of
the error, followed by the run record path and the exit code, so the journal
shows why the run failed without reading the document. `upkeeper status` prints a
reason beside each drifting component: a hold marker for the host, the host's
`upkeeper-self.timer` being disabled, or the last scheduled run (`self-pull` or
`scheduled-pull`; a manual pull is not scheduled evidence) recorded for that host
that selected the component (a component failure, a deferral, or a host-level
refusal such as a hold). For the controller itself the evidence is read locally.
For every other host it comes from the host's own observation: the target-side
agent and the probe both report, under `upkeeper`, the host's newest scheduled
runs (id, time, command, outcome, failed components, per-host component outcomes)
from its own run store, every hold marker it holds with the run that left it and
its age, and the state of `upkeeper-self.timer` and `upkeeper-pull.timer`
(enabled, active, next elapse). A marker the controller wrote for a host when it
dispatched there is kept in front of the host's own. The sentence "reason
unavailable from this host" is printed only for a host that reported no such
evidence at all, which is a target running an UpKeeper binary from before this
release. Run records written by `self-pull` and `scheduled-pull` carry that
command name; records from earlier releases say `pull` and are not counted as
scheduled evidence.

The T3 pair refusal reads the program behind `t3code.service` through the running
process (`/proc/<MainPID>/exe`, which the kernel marks deleted when a mise Node
upgrade removed it) or, when nothing is running, through `mise which t3`; the mise
shim itself is never taken as evidence that T3 exists. A missing program under a
server that still answers is a warning in the observation, not a refusal.

The runner posts once to the fleet feed only if a component/legacy updater changed
or the run needs attention. Quiet convergence and pre-dispatch offline skips produce
no post. Source is `upkeeper-pull`, tags are `citadel upkeeper`. Reports are redacted.
A failed feed request is not retried; the service fails and the journal retains the
result. Existing pull records remain under `~/.local/state/dev-fleet`.

Validate without mutation or feed posts using
`upkeeper scheduled-pull --dry-run`. The generic drift command is
`upkeeper pull --all --skip-unreachable --dry-run --json`; ordinary pull without
`--skip-unreachable` retains its original failure behavior.

The homelab-infra drift checker integrates this fleet dry run on Normandy when
`~/.config/homelab-infra/upkeeper-drift` exists. Its existing report includes
component changes even when the dry-run exit code is zero; broken observation is
checker failure. Other Docker hosts need no UpKeeper checkout. Deploy that seam
with homelab-infra's narrow `playbooks/upkeeper-drift.yml`.

Operations: `systemctl --user start upkeeper-pull.service` runs once;
`journalctl --user -u upkeeper-pull.service` shows evidence;
`systemctl --user disable --now upkeeper-pull.timer` stops future runs.
U4 owns the laptop's own timer. This stage neither enrolls workers nor changes pins
owned by concurrent Track S work.

Retired 2026-10-07: the Python `scripts/install-pull-timer`,
`scripts/upkeeper-scheduled-pull` and the committed Python units and canary
drop-ins. The Go installer and units above replace them; binary rollback is
`upkeeper self rollback`.
