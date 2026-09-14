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

An unknown `schema_version` fails closed. Unknown fields inside a known version are preserved when
they round-trip byte-for-byte and refused when they cannot.

## Projections

| Projection | Destination on the host | Rule |
| --- | --- | --- |
| worker | `~/.config/t3-steward/worker-bootstrap.json` | byte-identical to the `worker-configuration` v1 document whenever a host's intent is unchanged |
| client | `~/.config/t3-steward/config.yaml` key `backlog_v2.coordinator_client` | rendered from `coordinator.client_endpoint` plus the host's `admin_credential_ref` |
| coordinator | proposed catalog input for the coordinator host | never applied by UpKeeper; it is input to an explicit Steward enrollment operation |

Rendering is deterministic: the same intent produces the same bytes on every host and every run.
Staging writes to a temporary path in the destination directory, validates, then renames
atomically, retaining the previous document under the run's private state directory the way
`worker-configuration` v1 already retains `prior.json`.

## Enrollment planning

`upkeeper` may render an enrollment plan showing the authorization-relevant diff, the previous and
proposed catalog digests and the expected revisions. It may not enroll, and no convergence path
may enroll as a side effect.

## Shared fixtures

`testdata/fleet/` holds the fixture set that both repositories carry with identical bytes:
`fleet-intent.json`, `projection-worker-<host>.json`, `projection-coordinator.json` and
`projection-client-<host>.yaml`. UpKeeper renders them; Steward parses them.
