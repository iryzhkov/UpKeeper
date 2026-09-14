# H2 ADR: UpKeeper owns Steward fleet intent, the coordinator owns the effective catalog

Status: accepted. Freezes the contract before implementation.
Date: 2026-09-14
Authority: the dogfood failures of the UpKeeper Go-migration campaign, where model enablement
and the coordinator catalog were hand-edited live state with no durable owner, and where a
changed catalog invalidated enrollment with a diagnosis that said only "no route".

## Scenario

UpKeeper already distributes `worker-configuration` v1: one bootstrap document per host, with
worker identity, coordinator identity, transport, capabilities, provider routes and an opaque
credential reference. That component is deliberately narrow. It names provider *instances*, not
models, and it says nothing about the coordinator, the client, projects, repositories or
eligible workers.

Everything it does not cover was hand-edited on the coordinator. Enabling Opus on a host, adding
a project, changing which workers are eligible for it, and pointing a project at a repository
were all live edits with no reviewed source, no diff and no rollback. The campaign that exposed
this was accepted against a project whose repository (`ssh://git@git.ryzhkov.dev/igor/dev-fleet.git`)
was wrong, and the mistake surfaced only when a worker tried to prepare a workspace.

## Decision

Add a new versioned component, `steward-fleet-configuration`, that owns the non-secret desired
intent for the whole Steward fleet. It is a new component rather than a widening of
`worker-configuration` v1, because v1's field set is closed on purpose and existing releases
must stay loadable and rollback-able exactly as they are.

The component holds one normalized source covering coordinator identity and client endpoint;
worker identity, address and CPU class; executor-slot count; capabilities; provider instances;
explicit desired model allowlists; quota-pool references; projects with their canonical
repository, default ref, setup profile and eligible workers; credential references; reusable
host profiles; and explicit per-host exceptions.

UpKeeper renders three projections from that one source:

| Projection | Destination | Consumer |
| --- | --- | --- |
| worker | `~/.config/t3-steward/worker-bootstrap.json` | the worker runtime |
| coordinator | the coordinator's catalog input | the coordinator, through an explicit Steward operation |
| client | `backlog_v2.coordinator_client` | a non-coordinator CLI (H1) |

The worker projection must remain byte-compatible with `worker-configuration` v1 for any host
whose intent has not changed. That is the migration safety property: adding the new component
changes no bytes on a host until its intent is actually edited.

## Authorization is not evidence

A desired model allowlist authorizes a model. It never proves one is usable. The effective model
set is computed by Steward as the intersection of five things:

1. models allowed by authored fleet intent;
2. models actually installed or exposed by the provider on that host;
3. provider instances authenticated on that host;
4. models advertised in that host's current T3 cache;
5. any applicable quota-pool or host policy.

UpKeeper may report desired and, where it has an observation, observed. Only the coordinator
publishes effective, because only the coordinator holds the accepted snapshot the planner uses.
This preserves the boundary the worker-enrollment ADR already drew: the coordinator owns the
effective catalog, UpKeeper distributes bootstrap and releases.

A host whose desired set is empty is a host with no authorized models, which is different from a
host with no observed models. Both must be distinguishable in output, because collapsing them is
how "no route" became unexplainable.

## Enrollment stays explicit

UpKeeper may produce an enrollment plan. It may not enroll.

A plan renders the authorization-relevant diff, the previous and proposed catalog digests, and
the expected revision each change is fenced against. Applying it is a deliberate Steward admin
operation with its own idempotency key and revision fence. There is no flag on any UpKeeper
command that applies enrollment as a side effect of convergence, because an accidental
re-enrollment invalidates in-flight offers on a live fleet.

## Capture safety

`upkeeper push` captures the controller host's observed state. For this component, capture is
forbidden from overwriting authored intent with one host's observations. Capture may record
observations in a clearly separate observed field; the authored fleet model is edited by hand,
reviewed as a diff, and never inferred.

This rule exists because capture has already silently moved a reviewed pin to the controller's
newer local checkout once. Applying that failure mode to model authorization would let one
host's local state become fleet-wide authorization.

## Compatibility, staging and rollback

Releases without the new component load unchanged and converge unchanged. Releases with it are
refused by a binary too old to understand it, rather than partially applied: the component
carries its own `schema_version`, and an unknown version fails closed.

Unknown or newer fields inside a known version are preserved when they can be round-tripped
byte-for-byte, and refused when preservation cannot be guaranteed. Silently dropping a field
that a newer controller wrote is the one behaviour that turns a rollback into data loss.

Staging is deterministic: render, diff, stage to a temporary path, validate, then activate
atomically, with the previous document retained privately under the run's state directory the
way `worker-configuration` v1 already retains `prior.json`. Rollback re-applies the retained
document and is exercised as part of qualification, not assumed.

## Secrets

The component carries references only, in the existing `secretref:` form. Values are resolved on
the target host from its own 0600 secret store and are never read, copied, logged or captured by
the controller. Admin client credentials use the `f03-admin/` namespace and worker protocol
credentials keep `f02-protocol/`, so that distributing one never distributes the other.

## Alternatives rejected

Widening `worker-configuration` v1 was rejected: its closed field set is what makes
"catalogs and secrets are forbidden here" enforceable, and every older release would have to be
re-validated against a moving schema.

Letting UpKeeper write the coordinator catalog directly was rejected. The coordinator would then
have two authors for one document, and a reload that failed validation would leave the fleet's
authorization state ambiguous. UpKeeper proposes; Steward accepts, fences and publishes.

Deriving model allowlists from what each host currently reports was rejected: that is capture
inferring authorization from observation, which is precisely the drift this component exists to
remove.

## Explicitly not in this work

No quota policy redesign, no scheduler changes, no artifact system changes, no new inventory
format, and no automatic enrollment. Inventory keeps supplying host addresses, users and
verified host identity.
