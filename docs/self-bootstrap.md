# Machine bootstrap and pull --self

Citadel U2 adds the target-side entry point:

```bash
~/.local/share/dev-fleet/scripts/upkeeper pull --self --dry-run
~/.local/share/dev-fleet/scripts/upkeeper pull --self
```

The CLI reads the checkout containing its own code. DEV_FLEET_CONFIG_ROOT does
not redirect --self. The checkout must be clean; HEAD freezes the release and any
legacy companion files. Exactly one inventory machine-id must match. Unknown or
ambiguous identities fail before deployment. --self is exclusive with --all and
--hosts. The transport is local, with no SSH fallback if identity changes.

This is the same pull engine, with the same locks, run records, component order,
T3 compatibility/idle checks, environment projection and uncertainty handling.
Dry run creates no target files, records or locks; the source launcher disables
bytecode writes. It reads the currently checked-out release and does not fetch.

omarchy-setup ends with modules/common/27-upkeeper.sh after every selected machine
module. A failed machine module prevents the handoff. The seam installs python3
(3.11+), git and uv using the OS package helper, clones main into
~/.local/share/dev-fleet, installs a command link if absent, and invokes that
checkout's upkeeper pull --self explicitly. An existing development command is
preserved. Existing bootstrap checkouts must be clean, on main, and have the
configured origin; they update by fast-forward only. A missing-checkout dry run
prints prerequisite/clone/handoff actions without creating it. An existing-checkout
dry run executes --self --dry-run without fetching. Git authentication and host
inventory admission must already be provisioned; no credentials are copied.

The seam does not implement OS installation, invent an inventory identity, enroll
a worker, create T3 authentication, or change the existing component bootstrap
prerequisites. A bare machine missing a component's required checkout/service
receives the component's explicit bootstrap error; that is not reported as
convergence. U2's live acceptance uses the plan's omarchy-pc alternative: deliberate
removal of two managed instruction files, full local dry run, detection without
activation, and exact restoration by scripts/validate-self-canary.py. This does
not claim a from-zero VM installation of every fleet service.

The post-update hook reaches this seam through the normal setup run. No new
timer or drift job is added here (U3); laptop rollout remains U4.
