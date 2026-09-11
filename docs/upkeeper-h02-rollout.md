# UpKeeper H02 rollout contract

Status: source-qualified only. No live deployment, activation, restart, secret change, public
exposure, push, or pull request was performed in H02.

## Authority and transaction rules

t3-steward remains the workflow/effect-ledger authority. UpKeeper owns deployment admission,
phase progression, observation, reconciliation, and rollback. The pinned homelab-infra Git
revision and its Ansible inventory/playbook remain the only desired-state and application
authority.

Every phase requires the exact run revision and frozen plan hash. The target lock is acquired
before the locked preflight and retained until COMPLETED or ROLLED_BACK. Install, activate,
run, observe, reconcile, and rollback are independently callable. Every external effect records
PREPARED and STARTED first. Timeout, a lost reply, a nonzero result after target invocation, or
missing required observations records UNKNOWN. UNKNOWN permits reconciliation only; retry is
allowed only after an independent observation proves the effect applied or proves an exact safe
pre-effect state. An unexplained revision, artifact digest, or configuration digest remains
UNKNOWN and is not overwritten.

A local journal append writes an fsynced transition record before the event. Recovery may remove
that record only with the expected revision, a valid matching record, and proof that its process is
dead. If the event exists, kind and data digest must match. A live or unobservable owner, corrupt
record, unexpected revision, or contradiction remains recovery-required.

## Candidate rollout path

RolloutPlan freezes a sorted fleet, nonempty canary set, positive soak duration, and a batch size
bounded to 1–100. Its SHA-256 identifies the exact plan. steps() deterministically yields:

1. install-only on the frozen fleet; no activation;
2. activate/run/observe on the canary set;
3. a separate canary soak observation after the frozen duration;
4. activate/run/observe for each deterministic bounded batch;
5. a fleet-wide observation, then settlement.

Each step needs a new revision-fenced admission decision and complete required health. Process
liveness is not health. Missing or unhealthy health stops progression. Rollback is a distinct
effect and must use the external executor plus the exact prior artifact/configuration identities
recorded before activation. Candidate staging is cleanup-eligible only after terminal evidence.

## Install-only prerequisites

Before any separately authorized live install:

- create a reviewed homelab-infra commit that installs both the candidate artifact and the
  read-only configuration snapshot at ~/.config/dev-fleet/inventory/hosts.yml and
  ~/.config/dev-fleet/desired/components.yml;
- verify candidate, external rollback executor, runbook, prior artifact, and prior configuration
  path/size/SHA-256 outside all candidate-managed paths;
- run candidate status --json --limit normandy and require all revision axes plus no missing
  coverage;
- freeze the rollout plan hash, homelab-infra revision, inventory, playbook, policy evaluation,
  rollback identities, owners, stop conditions, and cleanup;
- obtain separate manual effect authority.

DEV_FLEET_CONFIG_ROOT is an explicit absolute-path test/operator override. Installed UpKeeper
otherwise discovers only the conventional ~/.config/dev-fleet location. Source checkouts keep
using their repository-root inventory and desired state.
