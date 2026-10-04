# Private canonical policy generations

The shared source descriptor now names version 2, copied from Omarchy
b32c0b6f48e12b7d74a60f8a010f84bab73d299f (comparison tree
fc0e7ad2bad7a7df96030f71696125b5a7d5bd52). It is candidate-inert.
No production caller or default author selection uses this preparation.

Call author.PrepareSharedWithPolicy with explicit absolute physical repository
and release-manifest paths, a full lowercase source commit and a lowercase
SHA256 pin of the canonical release declaration. Store.StageWithPolicy owns
preparation and cleanup. The manifest is bounded, no-follow regular-only,
single-link and bound against replacement. Its JSON envelope is duplicate-safe,
bounded to depth 64 and 8192 entries per container and exactly canonical.
The owning release.Validate and FleetIntent run with normal configured consumer
context. Unavailable or rejected consumer/catalog validation refuses.
This validates the pinned declaration, not its companion assets.

Only generator derivation receives an empty private HOME/TMPDIR and explicit
policy file. It reuses author.derivePolicy, its generation and check invocation,
source binding and exact mandatory eighteen instruction files. Both native
provider families are required for all five roles. No model inference occurs.
The helper is the exact upstream blob; its copied executable mode is 0755,
where upstream Git recorded 0644. The generator adds only shared-root transport
and shared-source attribution, retaining the candidate's agent99 option exclusion.
CLAUDE retains the same exclusion. Current task/campaign/wait guidance preserves
M15 public submission, permanently retired executable local/coordinator/file-forwarding
Markdown intake, true-flag refusal/false compatibility parsing, authenticated
marker-only cleanup without retry, and fenced operator boundaries.
Other copied skills remain unchanged.

Receipt v2 extends the eight v1 fields with a mandatory policy object.
That object contains input_manifest_sha256, input_contract,
canonical_policy_sha256, policy_format, derivation, upstream_repository,
upstream_commit, upstream_tree, interpretation, configured_catalog_validation,
current_authorization and readiness. The frozen values are
canonical-release-manifest-v1, route-policy/v1-canonical-json,
author-derive-policy-v1, github.com/iryzhkov/omarchy-setup,
the current commit/tree above, steward-parser-and-pinned-generator,
completed-at-preparation, unknown and unassessed respectively.
Digests are lowercase SHA256. No manifest content is retained.

The sole extra payload is inputs/route-policy.json, kind policy-input,
logical destination route-policy, mode 0600, canonical bytes from FleetIntent,
size 1..1MiB and digest matching the policy record. Instructions are the
eighteen exact author paths prefixed with instructions/ and retain supported
0600/0644/0700/0755 modes. Closed skills retain 0644 and source hashes.
Receipt/descriptor limits remain 256KiB/depth12/1024 entries; payload limits
remain 16MiB per file/64MiB aggregate with the tighter policy bound.
Canonical receipt bytes determine generation identity.

Inspection/select/rollback do not call Steward, validate a current catalog,
or render policy again. Retained provenance records derivation only:
current authorization is unknown and readiness unassessed. A completed
preparation claim is never permission to activate. Existing immutable v1
generations and old APIs remain supported; v2 without explicit policy refuses.
Offline v2 reading also retains the exact prior33 commit
33cb805b5482d511071be7b344140e2ccb8c87da/tree
3439bd874573cb88bbad852c5b66771f2ab80eeb pair. Mixed pairs and arbitrary
provenance refuse. New preparation accepts only current b32; rollback never
rewrites receipts or reestablishes grants. Store identity, locking, durability,
rollback and fault semantics remain.

Verification keeps two distinct generator pins. Original author regression
uses Omarchy88d9b534a6648e2615037dc3a655e6a8d0e9bebc and its original tree.
Shared parity uses current b32 above. The exact consumer fixture is Steward
d2327563e90e9b66adaf80c5a7a03746fce16763 built in a disposable checkout.
Synthetic configured-catalog transport exercises that actual parser and
validator; it establishes no operator authorization or live readiness.

Independent candidate and current-source publication-safety reviews, exact
source branch/main CI and all full S1 writer/fence/current-authority/distro/
no-SSH/plugin qualification remain required. This slice publishes no source,
release or fleet changes and performs no ownership transfer.
