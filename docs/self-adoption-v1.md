# Foundation self-adoption v1 (historical)

This is the historical F04 specification implemented by the F03 Python saga
library in package 0.1.0, last supported at source commit
`40e29c6c4b0b1dcc258e42f4aeba6ecc023c2460`. Python package 0.2.0 removes that
implementation, including the adoption helper; the protocol below is retained
as historical design evidence, not a currently callable API. Existing published
artifacts and installed hosts are unaffected by this source retirement.

The Go pull path does not implement this saga protocol. Future durable
infrastructure operations still require independently accepted journal, fence,
observation, reconciliation and recovery contracts before use.

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
