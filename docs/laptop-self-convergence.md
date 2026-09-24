# Laptop self-convergence

This mechanism now also supports every other inventory host; see
[fleet self-convergence](fleet-self-convergence.md). The laptop constraints below
still apply to its deployment and worker transport.

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
copied into the repository or unit. The user session must be running. The timer
needs no inbound SSH connection; an explicit manual controller pull may use the
inventory's verified SSH identity when the laptop is reachable.

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
`self_managed: true` remains excluded from the legacy scheduled controller pull.
Explicit manual `upkeeper pull --hosts laptop` and `upkeeper rollback --hosts laptop`
include it, with the same target lock that serializes a local timer run. Preview a
manual deployment with `upkeeper pull --hosts laptop --dry-run --json`, then inspect
the actual host result and run record; a successful controller command or a scheduled
skip alone is not evidence of convergence. The homelab drift checker continues to
understand the scheduled skip.

The laptop is not added to the worker set in this stage. S4's current worker
contract uses inbound coordinator SSH and is still being qualified. Manual
UpKeeper SSH deployment does not enroll the laptop as a Steward worker or create
worker credentials.
