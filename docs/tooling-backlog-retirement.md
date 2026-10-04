# Owned t3-backlog retirement

When the tooling manifest omits the exact source entry `homelab-cli/bin/t3-backlog`, UpKeeper may unlink only `.local/bin/t3-backlog` whose lstat type is symlink and whose readlink text exactly equals `<tooling-base>/current/homelab-cli/bin/t3-backlog`. Relative/foreign symlinks, regular files, directories and other unknown entries are preserved. A dangling exact owned link is eligible. Absence is a no-op. Omission does not establish ownership of any other command.

The bin parent uses existing agentenv.TargetPath confinement; the command leaf is inspected without following its target. Escaping parent paths are refused. Observation reports owned stale links as drift, even after current selects the new bundle; foreign preservation is explicitly reported in t3_backlog_binding rather than represented as deletion. Dry-run performs no staging, commands or retirement.

Activation includes an eligible retirement in the existing private 0600 bindings.json snapshot before any binding mutation. The snapshot must still be the exact owned link; each mutation has the existing concurrent snapshot precondition. Failed active health/binding verification compensates in reverse order, restoring current and the retired link. Compensation restores a retired path only if it is still absent; concurrent recreation is preserved with an actionable rollback failure and snapshot location. This retains the existing snapshot-check/mutation concurrency boundary; it does not introduce a filesystem-wide lock against arbitrary outside writers.

Immutable release files and old bundle data are never retired. Selecting a historical release containing the source entry probes the wrapper and restores its desired link through ordinary activation. Repeated omitted-source application converges when the exact link is absent; preserved foreign bindings stay preserved.

## Lead release checklist

- Obtain independent Sol/Astra medium reviews and required GitHub CI at the published exact source head.
- Build and validate controller artifacts first. Upgrade controllers/agents implementing tooling activation before delivering a bundle without t3-backlog: old controllers always probe that wrapper and cannot retire its dangling binding.
- Verify the lead's new Steward task/campaign binary support and full authored release compatibility; this source unit performs no provider, secret, fleet or release integration calls.
- Inspect full release drift/holds and preserve historical source bundles and the previous release throughout the rollout/rollback observation window.
- Review foreign-binding evidence honestly; arrange operator action separately when needed.
- Publish/deploy only under lead authority and verify omitted-source convergence plus historical rollback on approved targets. This PR does not complete M15 or waive later gates.
