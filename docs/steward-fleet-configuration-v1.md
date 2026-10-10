# Steward fleet configuration contract v1

This is the frozen shape of the `steward-fleet-configuration` component. The reasoning is in
`docs/architecture/adr-h2-steward-fleet-configuration.md`; this file is what the implementation
and its tests must match.

The component lives at `desired/release.json.components.steward-fleet-configuration`, is appended
to `ComponentOrder` after `worker-configuration`, and carries exactly these keys: `kind`,
`schema_version`, `coordinator`, `profiles`, `hosts`, `projects`, and optionally `clients` and `observed`.

## Authored fields

`coordinator`: `coordinator_id`, `host`, and `client_endpoint` with `address`, `connection`
(`ssh`), `remote_command`, `request_timeout`.

`remote_command` is a bare program name, `t3-steward`. The runtime passes it to `ssh` as a
single argv element and appends its own subcommand and operation after it, so a value with
spaces, arguments or a path is not a command it can run. The pattern forbids all three.

`request_timeout` is a duration string such as `"30s"`, not a number of seconds. The runtime
parses it with `time.ParseDuration` while it loads its configuration, so a number does not
degrade one request: it stops that host's worker daemon and both exchange commands from
starting at all. Authored values are bounded to 1s through 1h and normalized, so one intent
renders one spelling however it was written.

`profiles`: named reusable host profiles, at least `standard` and `build`. Each carries
`cpu_class`, `executor_slots`, `capabilities`, `provider_instances`, `models` (a map from
provider instance to its explicit desired model allowlist), `quota_pools` and, optionally,
`quota_bindings` (a map from provider instance to the one quota pool the coordinator charges
its work to; see "Quota bindings" below).

`hosts`: one entry per worker host, keyed by the inventory host name. Each carries `profile`,
`worker_id` (which must equal the key), `transport`, `credential_ref`, `admin_credential_ref` and
an optional `exceptions` object whose keys are a subset of the profile's keys and which override
them explicitly.

`projects`: one entry per Steward project. The legacy Git form carries `repository` (canonical remote),
`default_ref`, `setup_profile` and `eligible_workers`. An optional `type` may explicitly select `git` or
`fresh`; fresh projects omit `repository` and `default_ref`, and may omit `setup_profile` so Steward uses
its reserved empty-workspace setup profile.

`observed`: written only by capture, never authored, and never read as authorization.

## Rules

Credential references are opaque and follow the existing namespaces: `secretref:f02-protocol/<host>`
for the worker protocol and `secretref:f03-admin/<host>` for the coordinator-admin client. Values
are never present, never read by the controller and never captured.

Lists are sorted and unique. Host and worker identity must agree with each other and with the
inventory. A model allowlist is authorization only; it is never evidence that a model is installed,
authenticated or advertised.

An unknown `schema_version` fails closed, and so does an unknown field inside a known version.
Every key set in this component is closed, at the top level and in every nested object.
`quota_bindings` is the one optional profile key: a document authored before it existed
validates unchanged, and a document that carries it is validated as strictly as any other key.

## Quota bindings

`quota_bindings` maps a provider instance to the quota pool the coordinator may charge that
instance's work to. It is authorization, exactly as a model allowlist is: it says which pool an
instance is allowed to draw from. It never claims that the instance is installed on the host,
signed in, enabled, or advertising any model; those remain facts only the worker's inventory
and the coordinator's accepted snapshot can state, and the projection still never claims them.

Every instance a binding names must be in the profile's `provider_instances`, and every pool
it names must be in the profile's `quota_pools`; either mismatch is refused, because a binding
nobody can act on is dead authorization. One instance binds to one pool. A host exception may
override the binding for that host, and may introduce `quota_bindings` even when its profile
has none, because the exception is the reviewed place one worker differs from its class.

The coordinator projection renders `quota_bindings` in a worker's entry only when the worker's
resolved profile binds something. A worker that binds nothing projects the exact bytes it did
before the field existed, so a release that never authored a binding changes nothing on the
coordinator.

**Until the corresponding t3-steward change lands, the coordinator does not read this
field.** It reads `coordinator-fleet.json` with unknown fields refused, so a projection that
carries `quota_bindings` is rejected by a coordinator that predates that change, its reload
receipt says so, and the `steward-fleet-configuration` convergence on the coordinator host
fails with that receipt rather than applying half of the intent. The refused projection is
put back to the document that run replaced, so that coordinator still starts from a
configuration it can read; the release edit, not the host, is what has to be fixed. Authoring a binding is
therefore safe only once the coordinator accepts the field; the intended contract on that
side is that the projected binding is used when `backlog_v2.workers.<w>.providers.<instance>`
carries no explicit `quota_pool`, that an explicit `quota_pool` in the coordinator's own
configuration still wins, and that an instance with desired models and no binding from either
source is dropped for that worker with one warning naming it rather than failing the whole
configuration. Once that lands, registering a provider is one release edit.

This amends the original wording, which said unknown fields were preserved when they could be
round-tripped. A field nothing validates is a field a credential value can be authored into,
and this component is published in the release manifest and distributed to every host, so
carrying one silently would ship a secret to the whole fleet one commit after somebody typed
it. Forward compatibility is `schema_version`'s job: a controller that needs a new field ships
a new version, and an older binary refuses that version outright rather than carrying fields it
cannot check. Refusals name the offending key and say that this component carries references
only.

### Client-only hosts

Optional `clients` maps an inventory host name to the closed entry
`{"admin_credential_ref":"secretref:f03-admin/<host>"}`. A client name must be in inventory,
must not also appear in `hosts`, and must not name the coordinator host. The reference is
host-bound and carries no credential value.

A client-only host receives only `coordinator-client.json`: it receives no worker bootstrap,
does not enter the coordinator worker catalog or enrolled-worker count, and convergence never
enrolls it. Adding or removing one is listed in the authorization plan without worker
re-enrollment or coordinator-reload advice. Removing the entry retires UpKeeper's owned client
projection under the same receipt and rollback rules as an existing worker client's projection.

## Projections

| Projection | Destination on the host | Rule |
| --- | --- | --- |
| worker | `~/.config/t3-steward/worker-bootstrap.json` | byte-identical to the `worker-configuration` v1 document whenever a host's intent is unchanged |
| client | `~/.config/t3-steward/coordinator-client.json`, mode 0600 | rendered from `coordinator.client_endpoint` plus the host's `admin_credential_ref`; an explicit `backlog_v2.coordinator_client` block in `config.yaml` overrides it |
| coordinator | `~/.config/t3-steward/coordinator-fleet.json`, mode 0600 | Steward consumes the authored catalog fields before validation; only the coordinator host receives it, including a coordinator that is not a worker |

Rendering is deterministic: the same intent produces the same bytes on every host and every run.

The coordinator projection owns the full worker and project membership and the fields it renders.
Removing an entry revokes that authorization; release review must therefore cover the full intended
catalog, including entries unrelated to the immediate model change. For retained entries Steward
preserves the base configuration's transport, credentials, explicit quota bindings, execution
metadata and resource sizes. A new worker needs an explicit matching base binding. A new
provider's quota binding may be authored in the projection (`quota_bindings`, above); a
coordinator that reads the field uses it where its own configuration binds nothing, and a
coordinator that does not yet read it refuses the projection. A quota mapping is never inferred
from an unordered list of pools.
A new project does not: Steward loads a project the projection names and `backlog_v2.projects`
does not bind with default local bindings (no credentials, resource locks or directory
resources), logs one warning naming it at startup, and reports `project-binding-defaulted` as an
informational detail in `campaign check`. This amends the original wording, under which one
unbound project failed the coordinator's whole configuration load and with it every admin query.
A project that needs host-local bindings still gets them from an explicit `backlog_v2.projects`
entry, which is preserved exactly as before.
CPU classes use Steward's `low`, `medium`, `high` vocabulary; profile names remain arbitrary.
The projection never claims that desired models are installed, authenticated or available.
A changed catalog leaves enrollment stale until the existing revision-fenced operation is applied.
The coordinator projection's return channel is the coordinator's reload receipt,
`~/.local/state/t3-steward/coordinator/reload-receipt.json`: after writing a changed projection
on an active coordinator host the component sends SIGHUP and reads the receipt whose
`requestedAt` is at or after the signal (`accepted` or `unchanged` converge with the digest,
`rejected` fails with the coordinator's error and blockers, no receipt within 30 s fails as
unverified). The coordinator owns that verdict; UpKeeper never decides it and never restarts
the unit to force one.
A `rejected` verdict also puts the projection back to the document the run replaced, or
removes it when the run created it, and the failure says which: the coordinator never loaded
the refused bytes, and a projection it has refused must not be what it reads at its next
start, which would turn a rejected reload into a coordinator that cannot start. An unverified
reload leaves the projection in place on purpose, because no verdict means no evidence about
what the coordinator is running, and reverting the file could undo an accepted catalog
silently.
Staging writes to a temporary path in the destination directory, validates, then renames
atomically, retaining the previous document under the run's private state directory the way
`worker-configuration` v1 already retains `prior.json`.

## Enrollment planning

`upkeeper` may render an enrollment plan showing the authorization-relevant diff. It may not
enroll, and no convergence path may enroll as a side effect.

The plan carries no catalog digest. An enrollment is fenced against a catalog revision the
coordinator computes over data UpKeeper does not hold, so any digest rendered here would be a
number the coordinator will always reject, and printing one that merely looks like a fence
invites somebody to pass it and then to distrust the refusal. The plan reports the last
observed coordinator revision when there is one, and names where the real fence comes from:
read it with `t3-steward backlog workers --json`, which reports each worker's catalog
revision, and pass it to `t3-steward worker enroll --catalog-revision <revision>`, or run
`t3-steward worker enroll <worker> --current-catalog` on the coordinator host, which reads
the digest and the revision from the coordinator itself. This replaces the original wording,
which promised previous and proposed catalog digests.

For a project that the previous release did not carry, the plan says that the coordinator
will load it with default local bindings and lists, per eligible worker, the re-enrollment
command above. A worker that becomes or stops being eligible for an existing project, and
every worker of a removed project, is listed the same way, because each of those changes the
catalog digest that worker is enrolled against. So is every worker whose own coordinator entry
changes (models, quota pools, quota bindings, executor slots, capabilities, cpu class or
provider instances). A binding that appears, moves to another pool or disappears is its own
line: "worker W binds I to quota pool P", "rebinds I from quota pool P to Q", "unbinds I from
quota pool P".

## Adding and removing a project

`upkeeper project add NAME --repository URL --default-ref REF --workers a,b
[--type git] [--setup-profile P]`, `upkeeper project add NAME --type fresh --workers a,b
[--setup-profile P]`, and `upkeeper project remove NAME` are the one authored edit this
component has a command for. They load the release, edit the `projects` entry through the
release model, validate the edited manifest with `fleet.Parse` before a byte is written,
write `desired/release.json` atomically in its canonical encoding (a temporary file beside
it, renamed over it), and print the `fleet-plan` diff against the release committed at
`HEAD`. `--setup-profile` defaults to `git-status` for Git projects; when omitted for a fresh
project, Steward selects its reserved empty-workspace setup profile. A worker the fleet intent
does not declare is refused by name, and a refusal writes nothing.

The commands stay inside the authority this document draws: they commit, push, pull and
enroll nothing. The operator reviews `git diff desired/release.json`, commits, publishes with
`upkeeper push`, and, once the coordinator has converged, re-enrolls the eligible workers on
the coordinator host. The coordinator's own `backlog_v2.projects` is not written, and does
not need to be for a plain Git project (see the rule above).

## Adding and removing a provider

`upkeeper provider add INSTANCE --quota-pool POOL --hosts a,b [--models m1,m2]` and
`upkeeper provider remove INSTANCE [--hosts a,b]` are the same kind of authored edit for a
provider instance. `add` puts `INSTANCE` into `provider_instances`, its models into `models`
(`["*"]` for an instance the targets do not run yet; the authored allowlist is kept for one
they do, unless `--models` names new ones), `POOL` into `quota_pools` and `INSTANCE: POOL`
into `quota_bindings`, in one edit, on every named host. When every host of a profile is
named the profile itself is edited; otherwise each named host's `exceptions` receives all
four provider keys, built from the merged view of its profile and its existing exceptions, so
the host reads the same values whether or not the profile changes later. A host whose
exceptions already override a provider key is edited even when its profile is, because the
exception would otherwise mask the profile change. The output says which profiles and which
hosts were edited.

A host the fleet intent does not declare, an instance already bound to a pool on a named host
(the same pool: "already bound"; another pool: one instance may not draw from two pools) and
an invalid instance, pool or model name are refused, and a refusal writes nothing. `remove`
takes the instance, its models and its binding out of the named hosts, or out of every
profile and host exception when none is named; `quota_pools` is membership rather than a
binding and is left as authored. The edited manifest is validated with `fleet.Parse`, written
atomically in the canonical encoding, and the `fleet-plan` diff against `HEAD` is printed,
with the re-enrollment command for every worker whose entry changed. The commands commit,
push, pull and enroll nothing.

## Shared fixtures

`testdata/fleet/` holds the fixture set that both repositories carry with identical bytes:
`fleet-intent.json`, `projection-worker-<host>.json`, `projection-coordinator.json` and
`projection-client-<host>.json`. UpKeeper renders them; Steward parses them.

The model identifiers in the fixture are illustrative. The fixture's worker projections are
not: they are the exact bytes the current release publishes for `homelab` and `omarchy-pc`,
which is what makes the compatibility property testable.

Both repositories must feed these bytes to their own code. The first version of this component
shipped a client projection with a number where the runtime required a duration string and a
shell-shaped `remote_command`, and nothing caught it, because "both repositories carry
identical bytes" was true of the bytes and false of the tests: only the producing side ever
parsed them. A fixture that is not parsed by its consumer is a fixture that agrees with
itself.



## Optional route policy (M14 foundation)

`route_policy` is an optional authored object under the fleet component. Its
value is the complete closed `route-policy/v1` document, with `schema` and an
ordered `roles` list. Each role has `name`, ordered `candidates`
(`route`, required `effort`, `tier`) and optional `constraints`
(`provider_families`, `exclude_provider_families`, `tiers`). The schema owner
is t3-steward, contract commit
`8f65b474132707290add6cfb145f40fabf83bed7`, `docs/route-policy.md`.
UpKeeper runs the installed rc107-or-newer `policy show` parser instead of
maintaining another policy schema. Null, paths, unknown fields/versions,
duplicate names/routes/keys, missing effort, max effort, contradictory constraints
and policies exceeding 1 MiB refuse. The release JSON loader rejects duplicate
keys before conversion to maps can discard evidence of them.

By default, every declared worker, client-only host and coordinator receives a
standalone `~/.config/t3-steward/route-policy.yaml`, mode 0644. A worker host may
set the optional boolean `hosts.HOST.route_policy` to `false` to omit this
projection and its configured-catalog validation. Omission or `true` preserves
the default. Coordinators cannot opt out; client entries do not accept this host
field. Workers receive concrete routes and do not read the policy. An opt-out
releases any existing policy ownership receipt through the normal retirement
path, restoring the pre-ownership bytes or removing the owned file. Without a
receipt, it leaves any unmanaged file untouched. Observe and apply use the same
decision. UpKeeper does not merge
policy into steward's config or client JSON. Rendering is canonical JSON
(a YAML subset), with sorted object keys, two-space indentation and a final
newline; list order, exact route pins, effort, tiers and constraints are preserved.
The raw rendered bytes' SHA-256 is the runtime provenance. The conventional YAML
in `testdata/fleet/route-policy.yaml` is an inert consumer fixture; formatting it
as fleet intent produces a different raw digest without changing its semantics.
Neither the fixture nor this foundation activates the shared agent-instruction
tree or changes its source-owner inventory or legacy commit.

An absent key leaves legacy projection bytes unchanged. Without a policy
ownership receipt, absence does not read, claim or delete an existing policy.
Removing an owned key or retiring the component/host restores the original
pre-ownership bytes, or removes the file if originally absent. A private
`policy-ownership.json` receipt in the fleet state directory records original
bytes and durable active/pending digests. A restored tombstone relinquishes
ownership; subsequent operator edits remain unowned. Re-adoption starts a new
ownership boundary. Unexpected drift, malformed receipts, symlinks and nonregular
or oversized policy files refuse rather than overwriting operator work.

Release validation/loading (including authored publication and preserved-intent
capture validation) invokes `t3-steward policy validate --file PATH --json`
on the rendered reviewed bytes before publication effects. Host projection
preflights the same configured-catalog check before ownership, retention or live
destination writes, and repeats validation on the exact staged bytes before
rename. The CLI uses the invoking controller/host's configured coordinator
transport and the agent's HOME/XDG environment. A first-time host needs a working
read-only coordinator transport before policy installation. No worker readiness,
quota headroom or advertised model list substitutes for configured authorization.
Missing authority or transport fails with a bounded 30-second command and a
configuration/upgrade remedy; it never becomes an indefinite wait.

Schema parsing requires t3-steward rc107 or newer. Configured sole-wildcard
model catalogs additionally require Steward commit
`ced8e299561920ad1b46059f354316467f10f655` or a release containing that fix;
rc107 does not support them in policy validation. The corrected shared worker
routing authorization rule treats a sole `models: ["*"]` as authorization for
any concrete model on that provider instance. Explicit lists authorize only
named models; mixed lists and partial patterns grant no wildcard authority.
Neither advertisements nor readiness establish configured authorization, and
`*` itself is never a runnable candidate. Use the corrected consumer before
validating wildcard catalogs; never install a refused candidate.

New bytes are staged privately beside the destination, read back, checked,
changed to mode 0644 and atomically renamed. Every touched document's previous
bytes are retained in the run's `prior.json`. Validation, write, activation or
receipt-commit failure restores retained projections, with explicit rollback
failure reporting if restoration cannot finish. Manual `FleetRestorePrior`
also reconciles policy ownership. If a simultaneous coordinator-catalog reload
fails or remains unverified, the previous policy is restored and its receipt
reconciled; existing catalog-reload uncertainty handling remains authoritative
for the catalog. Policy-only changes require no service signal: rc107 reads
policy on each CLI invocation. Projection reports include the policy destination,
raw digest and retirement status.

The semantic tests invoke the actual installed steward CLI while a test-only
local socket returns configured authorizations for an offline, stale worker.
These are isolated fixtures, with no enrollments, quota/config changes or model
execution. Consumer tests fail when the CLI is missing. Verification must
provision the corrected consumer described above and run the fixtures, including
sole-wildcard authorization and concrete unauthorized candidate refusal.

The following omarchy generator campaign should consume `policy show --file
PATH --json` from the reviewed rendered file, retain its raw digest, preserve
role/candidate order and constraints, and choose each provider's candidates
without broadening pins or expanding specialty roles. The initial roles are
plan, execute, review, critical-review, read and 3d. Execute/review use medium;
plan uses high; critical-review/read/3d use medium. Explicit callers with a
model shared between high and medium roles need an explicit effort; session
calls remain explicit Codex medium. Premium/economy tier constraints do not
grant review authority or replace provider-diversity requirements. Generated
skills switch to roles only after coordinated publication and deployment.

## Implementation notes

These record where the implementation is stricter than the contract, and where the contract
left something open. They are notes about the implementation; the contract above is what was
frozen.

**The client projection is UpKeeper's own file.** Its destination was originally a key inside
`~/.config/t3-steward/config.yaml`, which t3-steward also authors; writing one key of that
file would have given one document two authors, the failure the coordinator half of this
component exists to avoid. The destination is therefore
`~/.config/t3-steward/coordinator-client.json`, mode 0600, written the same way the worker
bootstrap is and read the same way the runtime reads the worker bootstrap. An explicit
`backlog_v2.coordinator_client` block in `config.yaml` overrides it, so a host can still be
pinned by hand without UpKeeper and the hand-written value wins.

Its field set is closed: `schema_version`, `coordinator_id`, `address`, `connection`,
`remote_command`, `credential_ref`, `request_timeout` and `message_limits`. `credential_ref`
is the host's `secretref:f03-admin/<host>`, a reference and never a value.

**The coordinator's own host is never given this document.** It administers itself over its
own local socket, and its runtime refuses to start while a remote client endpoint is
configured, with advice to remove the block. If UpKeeper owned that file the advice would be
impossible to follow, so the coordinator host is skipped: it gets its worker bootstrap like
any other host, it is not required to hold an `f03-admin` credential for itself, and a client
document left over from when it was only a worker is removed and retained like any other
change. A coordinator that is also a worker is a supported configuration, not a refusal.

`message_limits` carries `max_bytes` and `max_artifact_bytes`, with defaults `4194304` and
`1073741824`. It may be authored under `coordinator.client_endpoint`, and is optional there so
that a document written against the frozen four-key endpoint shape still validates; an absent
one takes those defaults rather than meaning unbounded, because an unbounded client is one
malformed response away from exhausting the memory of whatever host ran the command. An
authored value is bounded to 1 byte through 1 GiB.

**These names are t3-steward's, not UpKeeper's to change.** The document is consumed as that
runtime's `V2MessageLimits`, whose vocabulary is `max_bytes`, `max_files` and
`max_artifact_bytes`. Renaming them here would give one concept two names across the two
repositories, which is the drift this whole component exists to remove. UpKeeper does not
author `max_files`, so the runtime's own default of `1000` applies to it; because the key set
is closed, authoring `max_files` in fleet intent is refused rather than quietly ignored, and
adding it means teaching this validator about it first.

**A disagreement between the two components is refused.** Both `worker-configuration` v1 and
this component project `~/.config/t3-steward/worker-bootstrap.json`, and this one converges
second. When a release carries an entry for the same host in both, and the two documents are
not byte-identical, convergence refuses instead of letting the later component silently
rewrite the reviewed v1 entry. The adoption hazard that follows from this is described below.

**The component is optional in the accepted component set.** A release carries either the
legacy component set exactly, or the current set with any optional component absent. That is
what lets every release published before this component existed keep loading, converging and
rolling back unchanged. Any other component set is still refused by name.

**Capture writes nothing into this component, not even `observed`.** The authored document is
taken aside before capture observes anything and put back byte-for-byte at the end, and a
release that never carried the component does not gain one. Recording observations is
permitted by the contract but is not implemented, because the controller has no observation
of another host's models that it did not ask that host for.

`push --components steward-fleet-configuration` is refused rather than accepted as a no-op.
The selection reads as "capture the fleet intent", nothing can capture it, and a run that
exits 0 having done nothing is read as success by whoever ran it. The refusal says the
component is authored, not captured, and names the file to edit.

**Retention lives in its own directory.** The documents a run replaces are kept under
`~/.local/state/dev-fleet/fleet-config/<run-id>/prior.json`, alongside but not inside
`worker-config/<run-id>/`, so that a run in which both components act cannot collide on one
exclusively created directory. Every owned document for that host is retained even when only one of them
changes, so restoring a run puts the whole of what it found back rather than half of it. The
retained record names each destination, and restoring it puts the host back to exactly what
the run found, including having no document at all. A retry of the same run ID reuses the
record the first attempt took rather than refusing or overwriting it; a directory that exists
without a record is diagnosed as a collision.

**What the retained document is for, and what it is not.** It is the within-run repair: when
one of the documents cannot be activated, every document that run touched is put back
before the host reports, so a failed convergence never leaves a host half applied. There is no
operator command that replays it, and there should not be one — an operator who wants a host
back on an earlier bootstrap converges an earlier release, which is reviewed, committed and
the same mechanism every other component uses. The retained record is evidence and a repair
buffer, not a rollback interface. An earlier draft of this document called rollback
"re-applying the retained document"; that was never the operator path.

**Secret references are checked the way the runtime opens them.** The reference is resolved
with `Lstat`, a symlink is refused rather than followed, the file is opened with `O_NOFOLLOW`
and the opened file is compared against what was checked. A check that followed links would
approve a symlink whose target has the right owner and mode while the runtime, which does not
follow links, refuses it: the host would converge clean and fail at first use. The file is
opened to prove the path checked is the path that would be used, and its content is never
read.

**An exception must produce a valid profile.** Overriding a key the profile does not have is
refused, and the merged result is validated as if it had been authored directly, so an
exception cannot smuggle in a value the profile schema would have rejected.

## Adoption hazard: two components, one bootstrap

Adding `steward-fleet-configuration` for a host that already has a `worker-configuration` v1
entry means that one file on that host now has two sources in one release. The two must agree
byte-for-byte, and convergence refuses the host when they do not, with
`fleet intent and worker-configuration disagree about this host's bootstrap`.

The refusal is deliberate, and it is safe: the comparison happens before anything on the
target is written, so a refused host keeps exactly the bootstrap it had. It is also visible in
a dry run, so the disagreement can be found without changing a live host at all.

What an operator does about it, in order:

1. Add the host to fleet intent so that its rendered worker projection is byte-identical to
   the `worker-configuration` entry it already has. Run `upkeeper fleet-plan` to see what the
   intent authorizes, then
   `upkeeper pull --hosts <host> --components steward-fleet-configuration --dry-run` to confirm
   the host reports `converged` rather than a refusal. Commit that as one reviewed change; it
   rewrites no worker bootstrap on the host.
2. Change the host's bootstrap only after step 1 has landed, and change **both** components in
   the same commit. A commit that edits one and not the other is the refusal above, and it is
   better caught in review than at convergence.
3. Remove the host from `worker-configuration` once fleet intent is the reviewed source for
   it. From that commit on there is one author again and step 2 no longer applies. Do this as
   its own commit, so that a rollback to the commit before it still finds a complete v1 entry.

Do not resolve a refusal by deleting the fleet entry for the host mid-adoption, unless the
`worker-configuration` entry is the one you want. The refusal says the two sources disagree,
not which of them is right.

The client projection has no such hazard: `coordinator-client.json` has one author, and a host
that wants a different endpoint pins it with an explicit `backlog_v2.coordinator_client` block
in `config.yaml`, which the runtime prefers over the file UpKeeper writes.
