# Redacted fleet discovery, 2026-09-09 to 2026-09-10

This report is based on live, read-only probes from `normandy`. Secret-bearing files were
checked by metadata only. No managed component, user service, configuration, plugin checkout,
or persistent database was changed.

## Verified identities and controller prerequisites

| Canonical host | Live hostname | LAN / mesh | User | OS | Arch | Python | Ansible | sudo -n | user systemd / linger |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| laptop | omarchy-macbook-air-igor | 192.168.90.163 / 10.42.0.5 | igor (uid 1001) | archarm | aarch64 | 3.14.7 | core 2.21.3 | no | running / yes |
| omarchy-pc | omarchy-pc | 192.168.90.212 / 10.42.0.6 | igor | Omarchy 4.0.3 | x86_64 | 3.14.7 | absent | yes | running / yes |
| normandy | omarchy-normandy | 192.168.70.234 / 10.42.0.7 | igor | Omarchy 4.0.1 | x86_64 | 3.14.7 | absent | yes | degraded / yes |
| homelab | homelab | 192.168.70.223 / 10.42.0.2 | iryzhkov | Debian 12 | x86_64 | 3.11.2 | absent | yes | running / yes |

The machine-ID digests are checked into inventory. The verified Ed25519 host-key fingerprints are:

- laptop: `SHA256:uHgPXAkcH1+xxdTKkLeeQMg+TtDo9teG4R9VRG4q9qo`
- omarchy-pc: `SHA256:Nd09F3OkNGDaGq4sLT1735uL+svBo6F7wsedunZ54UM`
- normandy: `SHA256:KtRGckedXhKRdOs7roVyrtfX1SHSObd2c8+jtjc/nNE`
- homelab: `SHA256:cwDZ4KLgTlYS0UTzSP4+kzcvaOPscvu4bopftUtn71U`

The live public key on each target matched the already trusted key. No key was auto-accepted.

Normandy's user manager is degraded because `docker-update-weekly.service` is failed; the
t3-steward service itself is healthy. This project did not reset or restart the unrelated unit.

## SSH matrix

Every off-diagonal direction succeeded with `BatchMode=yes`,
`StrictHostKeyChecking=yes`, and identity verification. A controller uses a local connection
for itself, so failed self-SSH was not treated as a target requirement.

| From / to | laptop | omarchy-pc | normandy | homelab |
| --- | --- | --- | --- | --- |
| laptop | local | pass | pass | pass |
| omarchy-pc | pass | local | pass | pass |
| normandy | pass | pass | local | pass |
| homelab | pass | pass | pass | local |

The laptop key is already pinned against `192.168.90.163` on all three other controllers.
The laptop's mDNS name `omarchy-macbook-air-igor.local` resolves on laptop, omarchy-pc, and
normandy but not homelab. The current IP is therefore recorded with
`stable_address_verified: false` until a DHCP reservation or universally resolvable DNS name
is confirmed. The machine-ID check prevents an address reuse from becoming a false identity.

## Repository and source access

All four hosts authenticate to Forgejo over SSH. Before repository creation,
`igor/dev-fleet` did not resolve through `git ls-remote`. Normandy, omarchy-pc, and homelab
authenticate to GitHub over SSH and can resolve all four component sources. The laptop's GitHub
SSH authentication is unavailable, but its existing HTTPS origins for nvim-configuration and
homelab-cli are reachable; public HTTPS fallback is available for the other sources. No
credential value was read or printed.

The working tree for this session is `/home/igor/Work/dev-fleet` on Normandy. It was empty and
not a git repository at kickoff.

## Installed component state

All four hosts have `t3-steward`, `t3-quota-watchdog`, `t3-backlog`, `feed`, Neovim,
Python, and git on PATH. All t3-steward units are loaded, enabled, active, and running, with
config mode 0600 and state-database mode 0600.

| Host | t3-steward | nvim-configuration | agent99 plugin | active agent99 bridges |
| --- | --- | --- | --- | --- |
| laptop | 0.10.1-dirty, commit 81f1965 | 824359e | 229ad18 | 1 |
| omarchy-pc | 0.10.1, commit 81f1965 | 2e94d7c | f9a372e | 0 |
| normandy | 0.10.1, commit 81f1965 | 2e94d7c | f9a372e | 2 during audit |
| homelab | 0.10.1, commit 81f1965 | 2e94d7c | f9a372e | 0 |

All four nvim and lazy agent99 checkouts were clean when inspected. Active bridges were recorded
without command-line arguments, environment, or journals; counts are point-in-time observations.

`t3-backlog --help` and `feed --help` succeeded everywhere. On laptop they resolve into the
clean `~/Work/homelab-cli` checkout at `2ebf13b`. On the other hosts they resolve under the
same nominal path but no git metadata was discoverable, so their installed source version is
unknown rather than inferred.

Dirty/unexplained user state was preserved:

- laptop `~/Work/t3-steward`: main at `81f1965`, four dirty entries;
- laptop `~/Work/homelab-infra`: main at `527ad92`, five dirty entries;
- Normandy `~/Work/t3-steward` changed during the audit from a dirty feature checkout to a clean
  feature checkout under unrelated activity. No reset, stash, clean, or edit was performed.

Install/config/state roots were verified as existing and writable where appropriate:
`~/.local/bin`, `~/.local/lib`, `~/.config/t3-steward`,
`~/.local/state/t3-steward/state.db`, `~/.config/nvim`,
`~/.local/share/nvim/lazy/agent99`, `~/.config/agents`, and installed skill directories.
Feed-key files were inspected by metadata only and never read.

## Existing Ansible conventions adopted

A temporary clean clone of `iryzhkov/homelab-infra` was studied without touching the laptop's
dirty checkout. Useful conventions retained here are strict host-key checking, one inventory
address per host, explicit per-host users and paths, no-op check mode before adoption, controller-
side secret handling, and preserving unmanaged state. This project remains a separate inventory
and blast radius.

The existing environment has a known Ansible startup failure when agent-harness stdout/stderr are
non-blocking. Agent-run gates redirect both streams to a file, then read a bounded result. This
project additionally pins Python packages and collection versions rather than installing latest.

## Initial component contracts

- t3-steward: preserve config/backlog/database and WAL sidecars; declare schema compatibility;
  stop only for activation; back up and integrity-check state first; atomically switch the binary;
  verify version, service, backlog, and waits; retain compatibility symlink.
- agent99: nvim-configuration owns the pin; refuse dirty/conflicted plugin or lockfile state;
  preserve theme drift; use pinned Lazy restore; build and probe both architectures; never kill
  active bridges without an explicit restart flag.
- nvim-configuration: validate JSON lockfiles, prerequisites, bounded Mason retries, and
  host-specific additions before atomic adoption.
- homelab-cli: replace mutable checkout links with versioned installs only after exact adoption;
  smoke-test every command and preserve feed configuration.
- agent-config: generate from its source of truth, compare before replacement, and preserve
  unrelated local additions.

## Decisions

- Canonical name is `omarchy-pc`; `gaming-pc` remains an SSH alias only.
- LAN is primary, Nebula explicit fallback.
- Default Phase 2 canary is the invoking controller when selected; otherwise Normandy.
- No unattended reboot is allowed. A prerequisite requiring reboot stops with a handoff.
- Forgejo access is per-host SSH identity. No shared private key or committed credential.
- GitHub is the off-homelab recovery mirror, never the implicit deployment source.
- Deployment remains disabled until every component baseline with divergent/unknown pins is
  explicitly selected and committed.

## Blockers at this report revision

1. Prove a stable laptop address (DHCP reservation or DNS reachable from homelab).
2. Bootstrap the locked Ansible environment on omarchy-pc, Normandy, and homelab through an
   explicit operation; doctor/status themselves must never install it.
3. Choose desired pins for agent99, nvim-configuration, homelab-cli, and agent-config.
4. Reconcile or explicitly accept the dirty laptop repositories and Normandy's unrelated failed
   user unit.
5. Verify the private Forgejo repository and off-homelab mirror after their first push.

Because these require evidence or user decisions, Phase 2 must not be queued unless they are all
resolved and every Phase 1 gate is green.
