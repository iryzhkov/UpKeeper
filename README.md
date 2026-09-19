# dev-fleet

An audited Ansible deployment system for developer tooling on `laptop`, `omarchy-pc`
(current SSH alias `gaming-pc`), `normandy`, and `homelab`.

UpKeeper owns fleet deployment through release capture and pull. The F03 saga library
remains a separate mutation API with revision-fenced approval, durable effects and
reconciliation. Neither interface is a workflow coordinator. Start with
[the system model](docs/architecture/system-model.md) and [CONTEXT.md](CONTEXT.md).

Every host can publish a validated release. Each host independently fetches the
GitHub-gated release and converges through its own local timer. See
[the fleet self-convergence guide](docs/fleet-self-convergence.md).

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
Because that environment is read from this machine's home directory and overwrites the
same paths on every other host, `push` first proves that this host still holds what its
omarchy-setup checkout generates, and refuses when it has drifted or when the check
cannot be run at all.
`pull` carries the validated payload over strict SSH, observes before changing,
preserves unowned settings and compensates failed environment/Huyang/steward activation.
See [the release guide](docs/upkeeper-push-pull.md) and
[the S4 distribution contract](docs/worker-distribution-v1.md).

## Steward fleet intent

```bash
./bin/upkeeper fleet-plan                                # report what the fleet authorizes
./bin/upkeeper fleet-plan --previous 6bcd2f8 --json      # and what enrolling it would change
./bin/upkeeper project add NAME --repository URL --default-ref REF --workers a,b [--setup-profile P]
./bin/upkeeper project remove NAME                       # both edit desired/release.json and print the plan
./bin/upkeeper provider add INSTANCE --quota-pool POOL --hosts a,b [--models m1,m2]
./bin/upkeeper provider remove INSTANCE [--hosts a,b]    # same, for a provider instance and its binding
```

`project add` and `project remove` edit the `projects` entry of the fleet intent through the
release model, validate the manifest with the parser `fleet-plan` uses, write it back in its
canonical encoding, and print the `fleet-plan` diff against the release committed at `HEAD`.
An unknown worker is refused and nothing is written. They commit, push, pull and enroll
nothing: review `git diff desired/release.json`, commit, and publish with `push`. An added
project is loaded by the coordinator with default local bindings, and each worker it is
eligible for needs `t3-steward worker enroll <worker> --current-catalog` on the coordinator
host once the release has converged there.

`provider add` and `provider remove` do the same for a provider instance: one edit carries the
instance, its models (`*` unless `--models` names them), the quota pool and the
`quota_bindings` entry that binds the instance to that pool, on every named host (the profile
when every host of it is named, otherwise each host's exceptions; the output says which). An
unknown host, an instance already bound to a pool, or an invalid name is refused and nothing is
written. The binding is authorization to charge the pool; it never claims the instance is
installed or signed in, and the coordinator honors it only once it reads the field (see the
contract). Every worker whose catalog entry changed is told to re-enroll in the printed plan.

The `steward-fleet-configuration` component holds the authored, non-secret intent for the
whole Steward fleet: the coordinator and its client endpoint, reusable host profiles, one
entry per worker host with its explicit exceptions, and the projects a worker may be offered.
Three projections are rendered from that one source: the worker bootstrap
(`~/.config/t3-steward/worker-bootstrap.json`), the coordinator client
(`~/.config/t3-steward/coordinator-client.json`, which an explicit
`backlog_v2.coordinator_client` block in `config.yaml` overrides, and which the coordinator's
own host is never given) and the fleet's authorization as one reviewable document, which no
binary consumes and UpKeeper never applies.

`fleet-plan` only reads. It reaches no host, writes no file and never enrolls; exit code `0`
means it reported, `2` is a malformed command line and `30` is a refusal. Applying a catalog
is a deliberate Steward admin operation with its own idempotency key and revision fence.

The component is optional, so a release captured before it existed loads and converges
unchanged, and for a host whose intent has not changed the worker projection is byte-identical
to the `worker-configuration` v1 document. Capture never writes the authored document: one
host's observations may never become fleet-wide authorization. See
[the frozen contract](docs/steward-fleet-configuration-v1.md), its
[ADR](docs/architecture/adr-h2-steward-fleet-configuration.md) and the shared fixtures under
`testdata/fleet/`.

## Read-only commands

```bash
./scripts/fleet doctor
./scripts/fleet doctor --json
./scripts/fleet status
./scripts/fleet status --json
./scripts/fleet status --json --limit laptop,homelab
./bin/upkeeper hold show --json                         # scheduled hold markers on this host
```

`status` prints a reason beside each drifting component: a hold marker for the host, the
host's self timer being disabled, or the outcome of the last scheduled run that selected the
component. For the controller the evidence is local; for every other host it is what the host's
own agent reported about its run store, hold markers and timers, so a remote host's drift is
explained the same way. "Reason unavailable" is printed only for a target whose binary predates
that evidence.

## Scheduled holds

```bash
./bin/upkeeper hold show
./bin/upkeeper hold clear laptop --reason "T3 reinstalled after the mise Node upgrade"
```

A scheduled run leaves `~/.local/state/dev-fleet/scheduled-holds/<host>.hold` when it fails
or loses its output, and later runs refuse that host until the marker is cleared. `hold clear`
removes exactly one marker and records the reason in `<host>.hold.cleared`. See
[scheduled convergence](docs/scheduled-convergence.md) and [recovery](docs/recovery.md).

Both commands use local execution for the inventory entry whose verified machine identity
matches the controller and SSH for every other host. SSH always uses batch mode, strict
host-key checking, and a bounded timeout. Neither command installs packages, fetches or updates
checkouts, creates remote directories, changes services, or writes run records.

Exit codes are stable: `0` is healthy/converged, `10` is drift, `20` is partial/offline,
`30` is a doctor/preflight failure, `40` is a future deployment failure, `50` is a future
rollback failure, and `60` is lock contention.

## The compiled binary

The same command surface is being migrated to a single Go binary, `cmd/upkeeper`,
so that a supported host needs no Python, no `uv` and no Ansible. The Python
package stays as the reference implementation and the rollback path until a host
has been canaried onto the binary. See [the binary guide](docs/go-binary.md) for
the package boundaries, the transport change, what a host still needs, and how
to move a host onto the binary and back.

```bash
make go-gate                                 # format, vet, tests, race, cross-builds
make go-build && ./bin/upkeeper version      # the binary names its own source commit
```

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
- `docs/go-binary.md`: the compiled binary, its transport, and the cutover.
- `docs/threat-model.md`: security boundaries.
- `docs/recovery.md`: Forgejo-independent recovery design.

The Citadel replan at `../citadel/docs/reference/active-plan/replan-2026-09-12.md`
controls current stages. The earlier audited dev-fleet plan is historical context.
