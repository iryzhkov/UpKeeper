# The compiled UpKeeper binary

UpKeeper is being migrated from a Python package to a single Go binary,
`cmd/upkeeper`. This document describes what the binary is, what it still needs
from a host, and how a host moves onto it and back off it again.

The Python implementation under `src/dev_fleet/` is unchanged and remains the
reference implementation and the rollback path. Nothing in the fleet runs the
Go binary until an operator installs it and switches a unit, and both of those
steps are reversible with one command.

For an explicitly invoked local-only standalone installer, see
[standalone-bootstrap.md](standalone-bootstrap.md). Debian 13/Arch amd64
compatibility remains provisional until actual distro execution is qualified.

For operator-pinned, read-only candidate tool declarations and content verification,
see [offline-tool-catalog.md](offline-tool-catalog.md).

## Why a binary

The deployed program needed a Python runtime on every host, and the supported
build and test path needed `uv`. That put a language toolchain in the trust
boundary of the thing whose job is to install trustworthy tooling, and it made
bootstrapping a new host a chicken-and-egg problem. A compiled binary removes
both: a host needs the binary, Git, OpenSSH, systemd and ordinary operating
system facilities, and nothing else.

## Package boundaries

Packages follow durable responsibilities rather than the Python file layout.

| Package | Owns |
| --- | --- |
| `cmd/upkeeper`, `internal/cli` | argument parsing, exit codes, text and JSON reports |
| `internal/safety` | every validator a value must pass before it reaches a path or a command |
| `internal/canonjson` | the three Python-identical JSON encodings that form durable identities |
| `internal/redact` | secret removal before anything is printed or persisted |
| `internal/fsatomic`, `internal/locks`, `internal/runstore` | atomic writes, the deployment lock, append-only run records |
| `internal/inventory` | `inventory/hosts.yml` and `desired/components.yml` |
| `internal/release`, `internal/publish` | the release manifest, its capture, and its publication |
| `internal/agentenv` | the owned agent environment: capture, projection and compensation |
| `internal/probe`, `internal/report` | `doctor` and `status` |
| `internal/transport` | local versus strict-SSH dispatch |
| `internal/agent` | the target-side convergence and its component adapters |
| `internal/converge` | convergence order, locking, run records and exit aggregation |
| `internal/selfconv`, `internal/units` | the scheduled entry points and their systemd units |

The binary uses `github.com/klauspost/compress/zstd` to verify and extract
controlled runtime bundles without requiring a system zstd executable. TOML
editing for `.codex/config.toml` uses the span-preserving editor in
`internal/agentenv/toml.go`, which refuses anything it cannot reproduce exactly
rather than rewriting a file from a partial model.

## How a target is reached

The Python controller streamed a Python program to `python3 -` on the target.
A Go controller cannot do that, so the transport changed:

- when the inventory host is this machine, identified by the SHA-256 of
  `/etc/machine-id`, the agent runs **in this process**. There is no subprocess,
  no network and no interpreter. This is the path every fleet host uses daily;
- every other host is reached over strict SSH and runs **its own copy of this
  binary**, `~/.local/bin/upkeeper __agent`, with the same single-JSON-payload
  wire format as before. A host that does not have the binary yet is diagnosed
  by name before anything is mutated, rather than failing obscurely.

`doctor` and `status` work the same way through `__probe`.

This is the one behavioural change to the deployment contract, and it is the
reason the controller no longer needs an interpreter on the far side. Until
every host has the binary, remote convergence from a Go controller requires
installing it there first; self-convergence does not.

## What a host still needs

Compiling the CLI removes the interpreter from UpKeeper itself. It does not
make a host empty, and the migration does not claim to. This is the complete
set of programs the binary executes, established by reading every process it
starts rather than by recalling the design.

On a controller, for `doctor`, `status`, `push`, `pull`, `rollback` and the
timers:

1. **A Git checkout per host**, and `git`. `pull --self` and `rollback` read
   release intent out of Git history, and the inventory and desired state are
   files in that checkout.
2. **`ssh`**, for every host that is not this one.
3. **`npm`**, for the T3 component of a capture. A `push` fails without it.
4. **The installed `t3-steward` binary**, which a capture asks for its version
   and its compatibility range.
5. **`gh`**, for `self-pull` only: the refresh gate asks GitHub whether a
   candidate commit's CI actually passed before the checkout moves.
6. **`feed`**, for `scheduled-pull` only, which posts its outcome to the fleet
   feed.
7. **`systemctl`, `loginctl`, `uname`**, for observation and for the timers.

On a target, additionally, for the component adapters:

8. **`bash`**, to run the omarchy-setup installer, which is the seam that still
   installs python3, git and uv on a new host.
9. **`go`, `make` and `git`**, to build Huyang from source; **`bin/huyang`**
   itself, which the probe starts as an MCP client.
10. **`npm`**, and `mise` when present, for T3.
11. **Outbound HTTPS to github.com**, for Steward release artifacts.
12. **A running systemd user session with lingering.**

Git, OpenSSH and systemd are inside the product boundary by design; the rest
are the components' own requirements, not UpKeeper's.

**`python3` and `ansible` are still observed, and `doctor` still fails a host
that has neither.** Those two checks are part of `doctor`'s frozen report and
were ported unchanged, so a host cannot actually drop its Python until they are
retired. Convergence does not need them: a host without Python reports nulls in
`status` and converges normally. Retiring the checks is a report-contract
change and is deliberately not part of this migration.

## Moving a host onto the binary, and back

Each step is independently reversible and leaves the Python path installed.

1. Install the released binary at `~/.local/bin/upkeeper`.

   On a host that has been managed for a while this path is not empty: it is
   already a symlink to `scripts/upkeeper` inside the checkout, so installing
   the binary here is what makes the interactive `upkeeper` command the Go one.
   Nothing scheduled goes through this path — the timers name their
   `ExecStart` in full — but a person typing `upkeeper status` does.

   Record what the path holds before replacing it, for example with
   `readlink -f ~/.local/bin/upkeeper`, because rollback is restoring that
   symlink and not merely deleting the file. Deleting it on a host where the
   symlink was the Python entry point removes the Python entry point too.
2. Run `upkeeper doctor` and `upkeeper status` by hand and compare them with the
   Python output. Both commands write nothing, so a difference costs nothing.
3. Run `upkeeper push --no-commit` on a scratch clone and compare
   `desired/release.json` byte for byte. Never on the live checkout: writing the
   tracked manifest makes the tree dirty, and the next capture refuses a dirty
   tree until the operator commits or reverts it.
4. Run `upkeeper pull --self --dry-run`.
5. Switch the timer with a systemd drop-in, not by editing the committed unit:
   copy `scripts/upkeeper-self.service.d/10-upkeeper-binary.conf` into
   `~/.config/systemd/user/upkeeper-self.service.d/` and run
   `systemctl --user daemon-reload`. Rollback is `rm` of that one file plus
   another `daemon-reload`; the committed unit and `scripts/upkeeper-self-pull`
   are never touched.
6. Only after a bounded real convergence and a proven rollback on the first
   host, repeat elsewhere.
7. Retire the Python launchers last. That is the first irreversible step in the
   whole migration, because no step before it rewrites a persistent format.

A host that has the binary and its checkout can install its own timer with
`upkeeper install-self-timer`, which writes the unit text embedded in the
binary. The controller timer on normandy is `upkeeper install-pull-timer`.

The two routes are alternatives, not layers. The drop-in overrides the
committed unit and is rolled back by deleting one file; the installer writes
the base unit with the binary's `ExecStart` already in it, so deleting a
drop-in on a host that has also run the installer would roll back nothing. The
installer refuses when a drop-in is present, so that "delete one file" stays
true wherever it was ever true.

## What the binary does not carry over

Two things are deliberately absent, and a reader comparing the two
implementations will notice them.

`src/dev_fleet/upkeeper.py` holds a second, richer record store: frozen intent,
a hash-chained event journal, `expected_revision` fences and orphan recovery.
No command in the CLI ever reached it — it belongs to the Ansible backend and
the foundation observer — so it was not ported. The records the fleet actually
writes are `src/dev_fleet/records.py`, which `internal/runstore` reproduces
field for field; they are crash-safe by atomic rename and have no fence to
preserve. If the Ansible backend is ever revived on the Go path, that store has
to be ported with it.

`self-pull` no longer re-executes itself after a refresh. The Python launcher
*was* the checkout, so refreshing and re-executing guaranteed that the gated
code ran; a binary is not the checkout, and re-executing it restores nothing.
The invariant is checked instead: after a refresh, the binary compares its own
source commit against the refreshed checkout and refuses to converge when the
program differs, naming the commit whose artifact the host needs. A binary
older than the checkout is normal and allowed, because main advances on every
release capture without the Go program changing.

## Gates

`make go-gate` runs the format check, `go vet`, `go test`, `go test -race` and
the cross-builds for `linux/amd64` and `linux/arm64`. `make gate` still runs the
Python reference gate. Both run in CI; the Go gate is a release blocker on its
own.

`upkeeper version` prints the exact source commit the binary was built from.

Release artifacts are built with `-trimpath`, which keeps the absolute path of
the checkout out of the binary. Without it, two clean checkouts of the same
commit produce different bytes and a published checksum can only be reproduced
by someone who happened to build in the same directory. With it, any clean
checkout of the commit reproduces the published checksum exactly, which is what
makes the checksum evidence rather than decoration.
