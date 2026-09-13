# Laptop self-convergence

Citadel U4 runs the laptop's pull locally from its own checkout. After the
omarchy-setup `27-upkeeper.sh` bootstrap handoff, install the timer from the clean
GitHub-gated checkout:

```sh
python3 ~/.local/share/dev-fleet/scripts/install-self-timer
systemctl --user start upkeeper-self.service
```

The daily timer runs at 09:00 local time with ten minutes of jitter, startup
activation after five minutes, and persistent catch-up. It requires the existing
GitHub CLI authentication and OS/toolchain prerequisites; credentials are never
copied into the repository or unit. The user session must be running. No inbound
SSH connection is needed to refresh or converge.

The runner requires the laptop's inventory machine-id, a clean `main` checkout
and canonical GitHub origin. It fetches `main`, verifies a fast-forward and the
exact candidate's latest successful GitHub `ci.yml` push run, then moves HEAD.
Pending, missing or failed CI, unavailable GitHub/authentication, a changed
checkout or a divergent branch fail closed without deployment. No release pin
is recaptured from the laptop.

After refresh it re-executes the new gated code. Local convergence is the same
`pull --self` engine: frozen release and companion files, exactly one local
inventory identity, no remote fallback, component safeguards, locks and records.
A durable scheduled hold survives failed or uncertain dispatch until observation.
The journal and `~/.local/state/dev-fleet/runs` contain outcomes.

```sh
python3 ~/.local/share/dev-fleet/scripts/upkeeper-self-pull --dry-run
journalctl --user -u upkeeper-self.service
systemctl --user disable --now upkeeper-self.timer
```

Dry run never fetches or advances HEAD and performs no activation. A host with
`self_managed: true` is skipped by controller pull/rollback, including reachability
probes, and is displayed as self-managed in the fleet report. Inspect its own
run record for deployment state; a controller skip is not evidence of convergence.
The existing homelab drift checker understands this intentional skip.

The laptop is not added to the worker set in this stage. S4's current worker
contract uses inbound coordinator SSH and is still being qualified. A future
worker decision must reconcile that transport with the laptop's no-inbound-SSH
deployment decision. No worker credentials or enrollment are created here.
