# The compiled UpKeeper binary

UpKeeper is being migrated from a Python package to a single Go binary,
`cmd/upkeeper`. This document describes what the binary is, what it still needs
from a host, and how a host moves onto it and back off it again.

The Python implementation under `src/dev_fleet/` is unchanged and remains the
reference implementation and the rollback path. Nothing in the fleet runs the
Go binary until an operator installs it and switches a unit, and both of those
steps are reversible with one command.

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

The module has no third-party dependencies. The one place that would normally
reach for one, TOML editing for `.codex/config.toml`, is handled by a
span-preserving editor in `internal/agentenv/toml.go` that refuses anything it
cannot reproduce exactly rather than rewriting a file from a partial model.

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

Compiling the CLI does not remove these, and the migration does not claim to:

1. **A Git checkout per host.** `pull --self` and `rollback` read release intent
   out of Git history, and the inventory and desired state are files in that
   checkout.
2. **`gh`.** The self-refresh gate asks GitHub whether a candidate commit's CI
   actually passed before the checkout moves.
3. **`feed`.** The scheduled controller convergence posts its outcome to the
   fleet feed.
4. **The omarchy-setup bootstrap seam**, which lives in another repository and
   still installs python3, git and uv on a new host.

Git, OpenSSH and systemd are inside the product boundary by design.

## Moving a host onto the binary, and back

Each step is independently reversible and leaves the Python path installed.

1. Install the released binary at `~/.local/bin/upkeeper`. Nothing references it
   yet. Rollback: delete the file.
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

## Gates

`make go-gate` runs the format check, `go vet`, `go test`, `go test -race` and
the cross-builds for `linux/amd64` and `linux/arm64`. `make gate` still runs the
Python reference gate. Both run in CI; the Go gate is a release blocker on its
own.

`upkeeper version` prints the exact source commit the binary was built from.
