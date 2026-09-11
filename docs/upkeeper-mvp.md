# UpKeeper mutation MVP

F03 adds the first bounded mutation seam to dev-fleet. It is a library API, not a
second workflow coordinator: t3-steward owns task and attempt state, while an
approved caller supplies an immutable `DeploymentSpec`, exact run revision, and
the frozen plan hash.

## Lifecycle

`UpKeeper.plan` freezes intent and creates a self-hashed event journal.
`UpKeeper.check` proves the previous healthy revision without acquiring a
mutation lock. `UpKeeper.apply` acquires the existing per-host locks and runs
install, activate, run, and observe. Each backend effect has a deterministic
identity and durable PREPARED and STARTED records before execution. Success
releases locks only after a terminal COMPLETED or ROLLED_BACK event.

Ambiguous effects stay UNKNOWN and retain their locks. The caller must invoke
`UpKeeper.reconcile`, which uses an independent backend observation before
apply may resume. No UNKNOWN or STARTED effect is blindly retried. Failed health
causes the rollback phase to restore and independently prove the previous
healthy revision.

## Ansible adapter

`AnsibleBackend` is the provisional single-controller
`homelab-ansible-v1` adapter. Before every command it requires:

- a clean local checkout at the exact full Git revision;
- the exact configured remote identity;
- repository-relative inventory and playbook files;
- an explicit sorted host limit and a fixed phase tag.

Only hashes and bounded metadata enter receipts. A timeout is ambiguous; a known
non-zero exit is a backend failure. This repository does not own homelab desired
state: the separately supplied homelab-infra Git checkout and its Ansible remain
the deployment authority.

There is no live rollout, reconciliation timer, remote bootstrap, repair agent,
portability layer, or multi-controller protocol in F03. The local disposable
service fixture is the canary for success, lost-response reconciliation,
health-gate rollback, stale revisions, journal tampering, and lock retention.
