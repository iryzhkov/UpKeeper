# Threat model

## Protected assets

The protected assets are per-host SSH and Forgejo keys, feed tokens, user configuration,
t3-steward backlog/state, Neovim and agent99 state, source worktrees, run evidence, and future
rollback copies. Public SSH host keys and one-way machine-identity digests are inventory facts,
not credentials.

## Trust boundaries

- A canonical inventory name is not a machine identity. Hostname, user, architecture, OS, host
  key, and machine-ID digest must agree.
- `gaming-pc` is a connection alias only; `omarchy-pc` is canonical.
- LAN is primary and Nebula is an explicit fallback. Neither route may weaken host-key checking.
- Component sources are restricted to declared HTTPS repositories and SSH mirrors on
  `github.com` or `git.ryzhkov.dev`.
- Forgejo is inside the managed homelab failure domain. It is never the sole recovery copy.
- CI has no deployment keys, production secrets, or permission to target the live inventory.

## Controls

All selector/ref/path/source validation occurs before interpolation. Subprocesses use argument
arrays. SSH is non-interactive with strict known-host checks. Secret-shaped keys and strings are
redacted recursively before output or persistence. Tests include bearer/basic authorization,
PATs, API keys, JWTs, credentials in URLs, query tokens, and private-key blocks.

Discovery inspects only metadata for secret-bearing paths such as feed keys and t3-steward config.
It never reads their values. Git status records counts, not filenames, because filenames can
contain sensitive customer or task data. Journals and environment values are not collected.

Future mutation must acquire every selected target lock before preflight, use versioned immutable
installs and atomic activation, preserve at least two healthy versions, back up schema-sensitive
state, skip every component that depends on a failed one, and stop the host after a failed
rollback. Dirty or unmanaged paths require explicit adoption and a verified backup. Stale locks, active agent99 bridges, and schema incompatibility require explicit
operator decisions.

## Known residual risks at the Phase 1 gate

- The laptop IP is currently verified but not proven reserved, and its mDNS name does not resolve
  from homelab. Machine identity prevents a silent wrong-host operation but does not provide
  reachability after an address change.
- Three controllers lack the pinned Ansible environment.
- Desired agent99, Neovim configuration, homelab-cli, and agent-config baselines are unresolved.
- Existing dirty repositories and an unrelated failed Normandy user service must be reconciled by
  their owners, not overwritten by this project.
