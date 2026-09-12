# Foundation self-adoption v1

F04 extends the UpKeeper MVP with a frozen, revision-fenced identity for adopting
t3-steward and UpKeeper itself. UpKeeper remains a deployment mutation engine;
t3-steward remains the sole workflow, task, attempt, and assignment coordinator.
The homelab-infra Git revision and Ansible playbook remain desired-state and
application authority.

## Frozen intent

An adoption intent records the source repository and exact Git commit, staged
artifact path/hash/size, installed path, optional configuration identity, an
argument-vector health probe, state-schema identity, and an external rollback
unit. The rollback executor, runbook, artifact, and configuration backup must
live outside every managed release/configuration path.

The modes are:

- adopt-running: prove that running revision and bytes already equal the desired
  identity, then bring that deployment under UpKeeper without changing it.
- bootstrap-absent: prove absence before first install. The external rollback
  executor must restore absence; a prior artifact is not required.
- upgrade: prove the exact previous revision and rollback bytes before mutation.

The adoption object is included in immutable intent and therefore in the plan
hash. Exact artifact/configuration hashes participate in preflight, observation,
settlement, and rollback verification. A missing or ambiguous observation fails
closed. PREPARED and STARTED effect records precede every Ansible phase, and
unknown effects must be reconciled rather than retried.

## Authority boundary

This code prepares and verifies a deployment operation but does not authorize one.
A live adoption still requires an approved exact desired-state commit, artifact
identities taken from the target host, maintenance timing, recovery ownership, and
rollback invocation authority. F04 does not add portability, a second scheduler,
remote bootstrap, public exposure, or multi-controller behavior.
