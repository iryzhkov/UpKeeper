# Independent fleet convergence

Each inventory host runs upkeeper-self.timer from a dedicated clean
~/.local/share/dev-fleet checkout. Every host fetches GitHub main, verifies the
exact commit's successful ci.yml push gate, fast-forwards, re-executes the new
code, and runs the local-only pull --self transaction. No host depends on Normandy
being online. GitHub and its CI remain the shared publication authority; Gitea
remains the release mirror.

A release can be published from any validated fleet host with Git push access.
Before capture, update the publishing checkout to the current gated main.
Review the complete capture: push records local observed state, not just the
component being edited. Preserve unrelated pins and worker secret references.
Concurrent publishers use ordinary fast-forward Git pushes: on rejection, review
and reconcile the new release before trying again; never force-push release intent.

## Installation and migration

Prerequisites on each host: Python 3.11+, Git, authenticated gh with access to
iryzhkov/UpKeeper, and the existing component toolchains. Credentials stay local;
no credential is embedded in the manifest, checkout, or systemd unit.

Clone the canonical GitHub repository on main to ~/.local/share/dev-fleet, after
checking that the exact main commit passed CI. Use SSH Git transport when that
host already has a GitHub SSH key; the runner accepts the canonical HTTPS or SSH
origin. Do not replace existing checkouts or their local work.

Run:
```sh
python3 ~/.local/share/dev-fleet/scripts/install-self-timer
systemctl --user start upkeeper-self.service
journalctl --user -u upkeeper-self.service
```

The timer runs daily at 09:00 with ten minutes of jitter, on startup, and with
persistent catch-up while the user's systemd manager is running. The installer
requires exactly one inventory machine-id match. Identity is checked again by
the scheduler and local deployment transport. The scheduler never falls back to SSH.

After proving each host's self run, disable Normandy's old upkeeper-pull.timer.
Keep its deployment checkout for development and manual commands. The old central
scheduler and its documentation remain available for explicit legacy operation;
they are not required by the independent timers.

## Development safety

The timer uses ~/.local/share/dev-fleet, separate from ~/Work/dev-fleet. It refuses
local edits or divergent history in its own checkout. Huyang keeps its existing
checkout safeguards: dirty work refuses activation, a checkout ahead of the pin
is preserved, and normal pull also refuses a divergent downgrade. Immediately
before activation it rechecks Huyang HEAD and dirty state to preserve edits made
during preparation. Only explicit rollback permits moving backward. Avoid editing
the live checkout during the activation itself; these observations are not an
editor lock. A refused scheduled deployment retains a hold for inspection.

## Urgent updates and evidence

After publishing and passing CI, start upkeeper-self.service on each reachable
host to refresh and converge now. Triggering a service over an administrative SSH
session does not make the deployment depend on that session: systemd owns the run.
An explicit manual controller pull can include the laptop over SSH. Its local
timer remains available; the target lock serializes concurrent convergence.
Offline hosts catch up later. Urgency never bypasses CI, local locks,
busy-service deferrals, dirty-checkout guards or retained scheduled holds.

Manual controller pulls and rollbacks may include every selected inventory host,
including the laptop. The laptop's self_managed flag excludes it only from the
legacy scheduled controller pull; the local timer remains its automatic path.
Preview all intended hosts before a manual fleet deployment and check each receipt.

Inspect each host's journal and ~/.local/state/dev-fleet/runs. A successful Git
push or a controller skip is not proof of convergence. After an uncertain or failed
dispatch, observe the host before clearing its local scheduled hold.
A dry run through scripts/upkeeper-self-pull --dry-run never fetches or advances HEAD.

When the gated release pins an UpKeeper binary whose program differs from the
one a host runs, that host's self-pull installs the pinned binary, verified by
size and sha256 against the manifest, and re-executes it before converging.
No hand-installed binary is needed after the first self-update-capable one.
See "Self-update from the release pin" in docs/go-binary.md for the refusals,
the generations under ~/.local/lib/upkeeper and `upkeeper self rollback`.

## Rollout evidence

Implementation 8548a9b145170fb4879999cbeec4d39ad801a88f passed GitHub CI
34739139497. Local gates passed 231 tests, Ruff, Ansible lint, syntax/check mode,
redaction and systemd unit verification.

All four hosts run the gated checkout from ~/.local/share/dev-fleet. Their self
timers are enabled. The laptop refreshed and returned systemd Result=success,
exit 0. Omarchy-pc run 20260913T050022Z-d4ec5d31 and homelab run
20260913T050025Z-4c372773 updated successfully with self=true, transport=local.
Normandy run 20260913T050024Z-c3a25376 used the same gated revision and local
transport. It returned exit 10 only because Huyang is ahead of the pin; the clean
development checkout stayed at 3f64e94e6b4bf426543ff5af17a7274904895464 and all
other components converged. Systemd displays this deliberate drift exit as failed;
no deployment error or retained scheduled hold was reported.

The user authorized copying Normandy's GitHub credential directly over SSH to
homelab's local gh store. No credential was printed or committed. Homelab uses
gh 2.100.0 because Debian's packaged gh lacks the timer's --event filter; the
mise download verified GitHub artifact attestations. Its private CI query passed.
Normandy's legacy upkeeper-pull.timer was disabled after verifying all four local
runs. The development checkout under ~/Work is separate and remains untouched by
scheduled refresh. This removes Normandy from automatic deployment dependencies.

This change does not enroll workers, change the T3 coordinator, or move secrets.
Normandy may still own those independent roles; fleet software refresh does not.
