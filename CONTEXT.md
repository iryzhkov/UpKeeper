# dev-fleet context

UpKeeper is the fleet deployment authority. Git owns release history; Huyang owns
repository reads and edits; the steward owns workflows and execution; T3 owns
conversations. The local mainline carries discovery, the F03 saga library and the
live push/pull deployment path. They are separate interfaces in one repository.

Read docs/architecture/system-model.md first for state, ownership, failure semantics
and the relationship between both mutation paths. The Citadel replan at
../citadel/docs/reference/active-plan/replan-2026-09-12.md controls stage order.

U0 adds the T3 pair, F05 agent environment and the S4 worker bootstrap distribution
contract. U1 removes the legacy self-update path and adds operator rollback.
U2 adds pull --self; the laptop converges through that path. Do not enroll workers
as part of release distribution.

Use Huyang for repository reads, searches and edits. Run make gate before commits.
Land normal-prose commits on main and verify both configured remote CI workflows.
Unknown external effects require observation, never automatic retries.
