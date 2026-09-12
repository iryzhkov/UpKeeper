# AgentEnvironmentSpec v1 (F05)

This contract is the deliberately narrow, single-worker environment seam used before R6.
UpKeeper owns install, activation, observation, and rollback. t3-steward continues to own
leases and assignments; T3 owns conversations; Huyang owns its journals; Git owns worktrees.

The immutable spec includes exact artifact provenance, digest, size, distinct staging and active
paths, activation order, fixed argument-vector probes, opaque secret references, and
project-instruction references pinned to a Git revision and digest. Project instructions are
referenced, not copied into a second authority.

The only F05 adapter is `local-single-worker-v1`. It stages every artifact without changing an
active path. UpKeeper must include the canonical spec bytes and current active spec revision in its
existing hash-chained deployment intent. Each install, activate, probe, and rollback effect is
PREPARED and STARTED before execution. A lost response becomes UNKNOWN and must be observed before
retry. Activation is allowed only when the candidate supplies complete passing evidence for every
baseline safety claim. Rollback restores the complete prior environment revision, never selected
files.

Drift projection distinguishes MATCH, MISMATCH, MISSING, UNAVAILABLE, and UNEXPLAINED. Unexplained
paths are preserved and fence activation. Startup recovery is an attributed projection over four
owners: t3-steward leases, T3 threads, Huyang journals, and Git worktrees. UpKeeper never infers or
mutates their state from paths.

## Prepared one-worker canary

Materialize an operator-approved spec whose asset hashes are observed from the target host. Freeze
the current environment as the rollback unit outside every managed path. Run baseline probes;
stage to a new immutable candidate directory; verify schema, paths, dependencies, hashes and
permissions; record and manually approve the exact UpKeeper plan hash and event revision; activate
as one unit; run the same probes; then roll back as one unit and repeat the probes. Preserve any
unexplained change and stop before activation. Service restarts, global instruction/configuration
changes, and worker activation require separate live authority.
