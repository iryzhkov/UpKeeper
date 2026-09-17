# Steward fleet configuration contract v1

This is the frozen shape of the `steward-fleet-configuration` component. The reasoning is in
`docs/architecture/adr-h2-steward-fleet-configuration.md`; this file is what the implementation
and its tests must match.

The component lives at `desired/release.json.components.steward-fleet-configuration`, is appended
to `ComponentOrder` after `worker-configuration`, and carries exactly these keys: `kind`,
`schema_version`, `coordinator`, `profiles`, `hosts`, `projects`, and optionally `observed`.

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
provider instance to its explicit desired model allowlist) and `quota_pools`.

`hosts`: one entry per worker host, keyed by the inventory host name. Each carries `profile`,
`worker_id` (which must equal the key), `transport`, `credential_ref`, `admin_credential_ref` and
an optional `exceptions` object whose keys are a subset of the profile's keys and which override
them explicitly.

`projects`: one entry per Steward project, carrying `repository` (canonical remote), `default_ref`,
`setup_profile` and `eligible_workers`.

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

This amends the original wording, which said unknown fields were preserved when they could be
round-tripped. A field nothing validates is a field a credential value can be authored into,
and this component is published in the release manifest and distributed to every host, so
carrying one silently would ship a secret to the whole fleet one commit after somebody typed
it. Forward compatibility is `schema_version`'s job: a controller that needs a new field ships
a new version, and an older binary refuses that version outright rather than carrying fields it
cannot check. Refusals name the offending key and say that this component carries references
only.

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
preserves the base configuration's transport, credentials, quota bindings, execution metadata and
resource sizes. A new worker or provider needs an explicit matching base binding; a missing
or unauthorized quota mapping is refused rather than inferred from an unordered list of pools.
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
catalog digest that worker is enrolled against.

## Adding and removing a project

`upkeeper project add NAME --repository URL --default-ref REF --workers a,b
[--setup-profile P]` and `upkeeper project remove NAME` are the one authored edit this
component has a command for. They load the release, edit the `projects` entry through the
release model, validate the edited manifest with `fleet.Parse` before a byte is written,
write `desired/release.json` atomically in its canonical encoding (a temporary file beside
it, renamed over it), and print the `fleet-plan` diff against the release committed at
`HEAD`. `--setup-profile` defaults to `git-status`. A worker the fleet intent does not
declare is refused by name, and a refusal writes nothing.

The commands stay inside the authority this document draws: they commit, push, pull and
enroll nothing. The operator reviews `git diff desired/release.json`, commits, publishes with
`upkeeper push`, and, once the coordinator has converged, re-enrolls the eligible workers on
the coordinator host. The coordinator's own `backlog_v2.projects` is not written, and does
not need to be for a plain Git project (see the rule above).

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
