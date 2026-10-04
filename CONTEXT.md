# dev-fleet context

UpKeeper is the fleet deployment authority. Git owns release history; Huyang owns
repository reads and edits; the steward owns workflows and execution; T3 owns
conversations. The local mainline carries discovery and the live push/pull deployment
path. Python package 0.2.0 retires the separately advertised F03 saga API, including
unknown external callers. Last-supported source is
40e29c6c4b0b1dcc258e42f4aeba6ecc023c2460 (package 0.1.0); published artifacts,
installed hosts and persisted state remain unchanged.

Read docs/architecture/system-model.md first for state, ownership, failure semantics
and the limits of pull relative to the historical F03 API. The Citadel replan at
../citadel/docs/reference/active-plan/replan-2026-09-12.md controls stage order.

U0 adds the T3 pair, F05 agent environment and the S4 worker bootstrap distribution
contract. U1 removes the legacy self-update path and adds operator rollback from committed
release manifests, with historical companion files and current deployment safeguards.
U2 adds pull --self. U3 schedules Normandy convergence. U4 refreshes GitHub-gated
main and converges the self-managed laptop locally from its own timer, without
controller SSH. See docs/laptop-self-convergence.md. Do not enroll workers as
part of release distribution.

The subsequent user-authorized fleet decentralization extends the gated self timer
to every inventory host. Each host refreshes and converges independently; Normandy
is no longer a required deployment controller. See docs/fleet-self-convergence.md.


Use Huyang for repository reads, searches and edits. Run make gate before commits.
Land normal-prose commits on main and push both remotes. The user's U0 decision
selects GitHub CI as the gate; gitea remains a mirror.
Unknown external effects require observation, never automatic retries.
