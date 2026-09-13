# dev-fleet

An audited Ansible deployment system for developer tooling on `laptop`, `omarchy-pc`
(current SSH alias `gaming-pc`), `normandy`, and `homelab`.

UpKeeper owns fleet deployment through release capture and pull. The F03 saga library
remains a separate mutation API with revision-fenced approval, durable effects and
reconciliation. Neither interface is a workflow coordinator. Start with
[the system model](docs/architecture/system-model.md) and [CONTEXT.md](CONTEXT.md).

## Release commands

```bash
./scripts/upkeeper push                      # capture this machine, commit, push to all remotes
./scripts/upkeeper push gitea --no-push      # capture and commit only
./scripts/upkeeper pull --hosts homelab omarchy-pc --dry-run
./scripts/upkeeper pull --all
```

`push` captures clean reachable checkout commits, the installed T3/steward pair and
compatibility range, and the complete owned agent environment into `desired/release.json`.
Worker bootstrap entries are host-scoped and carry only secret references.
`pull` carries the validated payload over strict SSH, observes before changing,
preserves unowned settings and compensates failed environment/Huyang/steward activation.
See [the release guide](docs/upkeeper-push-pull.md) and
[the S4 distribution contract](docs/worker-distribution-v1.md).

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

The Citadel replan at `../citadel/docs/reference/active-plan/replan-2026-09-12.md`
controls current stages. The earlier audited dev-fleet plan is historical context.
