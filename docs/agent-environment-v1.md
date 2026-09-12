# AgentEnvironmentSpec v1 (F05)

This contract is the deliberately narrow, single-worker environment seam used before R6.
UpKeeper owns install, activation, observation, and rollback. t3-steward continues to own
leases and assignments; T3 owns conversations; Huyang owns its journals; Git owns worktrees.

The immutable spec includes exact artifact provenance, digest, size, distinct staging and active
paths, activation order, fixed argument-vector probes, opaque secret references, and
project-instruction references pinned to a Git revision and digest. Project instructions are
referenced, not copied into a second authority. It also freezes the previous whole-environment
revision and one exact prior identity for every desired active path. Candidate staging may not
equal, contain, or be contained by any active path.

The only F05 adapter is `local-single-worker-v1`. It stages every artifact without changing an
active path. UpKeeper must include the canonical spec bytes and current active spec revision in its
existing hash-chained deployment intent. Each install, activate, probe, and rollback effect is
PREPARED and STARTED before execution. A lost response becomes UNKNOWN and must be observed before
retry. Activation is allowed only when the candidate supplies complete passing evidence for every
baseline safety claim. Rollback restores the complete prior environment revision, never selected
files.

Every phase observation uses the strict `agent_environment` evidence object. It names the frozen
host profile and worker, installed and active environment revisions, exact installed and active
asset maps, exact project-instruction references, baseline and candidate probe results, worker
readiness, unexplained paths, and paths proved preserved by rollback. Missing or malformed evidence
is ambiguous rather than success. Install requires desired bytes staged while prior active bytes
and the ready worker remain unchanged. Activate requires the worker to be unavailable until the
separate run/probe phase. Run and observe require the desired revision, exact bytes, and a ready
worker. Rollback requires the exact prior revision/assets and passing baseline probes.

Drift projection distinguishes MATCH, MISMATCH, MISSING, UNAVAILABLE, and UNEXPLAINED. Unexplained
paths are preserved and fence activation. Startup recovery is an attributed projection over four
owners: t3-steward leases, T3 threads, Huyang journals, and Git worktrees. UpKeeper never infers or
mutates their state from paths.

The fixed probe set must cover explicit effect authority, preservation of unexplained changes,
instruction safety, and skill safety. Candidate evidence must include every baseline probe and may
only retain or add safety claims. A weaker, missing, duplicated, reordered, or malformed result is
not eligible for activation.

## Prepared one-worker canary

Materialize an operator-approved spec whose asset hashes are observed from the target host. Freeze
the current environment as the rollback unit outside every managed path. Run baseline probes;
stage to a new immutable candidate directory; verify schema, paths, dependencies, hashes and
permissions; record and manually approve the exact UpKeeper plan hash and event revision; activate
as one unit; run the same probes; then roll back as one unit and repeat the probes. Preserve any
unexplained change and stop before activation. Service restarts, global instruction/configuration
changes, and worker activation require separate live authority.

## H03 deterministic qualification

The frozen H00 campaign identifies the `normandy` Codex worker as the intended profile; the
environment fixture names that profile `normandy-codex-worker`.
`tests/fixtures/h03-environment-faults-v1.json` is the non-executing H03 fault matrix.
`tests/test_h03_hardening.py` runs install-only, stale-revision activation refusal, activation,
run/probe, observation, rollback, and restoration. It injects a partial install, weakened safety
claims, an incorrect active environment revision, managed drift, and unexplained drift. The latter
remains UNKNOWN with its lock held until an explicit rollback proves both exact restoration and
preservation of the unrelated path. These tests do not authorize deployment or activation.
