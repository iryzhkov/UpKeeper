# dev-fleet

An audited Ansible deployment system for developer tooling on `laptop`, `omarchy-pc`
(current SSH alias `gaming-pc`), `normandy`, and `homelab`.

This repository is intentionally at the Phase 1 gate. It implements discovery, validation,
redaction, run-record primitives, and deployment-lock primitives. It contains no mutating
deployment role, reconciliation timer, remote bootstrap, or repair agent.

## Read-only commands

```bash
./scripts/fleet doctor
./scripts/fleet doctor --json
./scripts/fleet status
./scripts/fleet status --json
./scripts/fleet status --json --limit laptop,homelab
```

Both commands use local execution for the inventory entry whose verified machine identity
matches the controller and SSH for every other host. SSH always uses batch mode, strict
host-key checking, and a bounded timeout. Neither command installs packages, fetches or updates
checkouts, creates remote directories, changes services, or writes run records.

Exit codes are stable: `0` is healthy/converged, `10` is drift, `20` is partial/offline,
`30` is a doctor/preflight failure, `40` is a future deployment failure, `50` is a future
rollback failure, and `60` is lock contention.

## Environment

Python 3.11 is the supported floor because `homelab` runs Debian 12. The CLI itself is
standard-library-only so discovery works before Ansible is installed. Development and Ansible
are locked in `uv.lock`:

```bash
python3 -m pip install --user uv==0.12.12
uv sync --locked --all-extras
uv run ansible-galaxy collection install -r requirements.yml
make gate
```

Do not run bootstrap implicitly from `doctor` or `status`. Missing controller prerequisites
are reported as findings.

## Source of truth

- `inventory/hosts.yml`: verified identity, addressing, user, OS, architecture, and host keys.
- `desired/components.yml`: schema and immutable discovered component pins.
- `docs/discovery-2026-09-09.md`: redacted live evidence, decisions, and blockers.
- `docs/architecture.md`: command boundaries and JSON schema.
- `docs/threat-model.md`: security boundaries.
- `docs/recovery.md`: Forgejo-independent recovery design.

The full audited plan remains at `/home/igor/Work/plans/dev-fleet-plan.md`; Phase 2 must re-read it
and the committed repository state before implementing any deployment behavior.
