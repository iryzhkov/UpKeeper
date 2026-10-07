# The compiled UpKeeper binary

UpKeeper is being migrated from a Python package to a single Go binary,
`cmd/upkeeper`. This document describes what the binary is, what it still needs
from a host, and how a host moves onto it and back off it again.

The Go binary is the production path. The Python implementation under
`src/dev_fleet/` is no longer a rollback path: its systemd units, launchers and
canary drop-ins were removed on 2026-10-07, and its release reader already
accepted neither the current manifest nor controlled T3 artifacts. Rolling back
the binary is `upkeeper self rollback`, which switches to the previous Go
generation under `~/.local/lib/upkeeper/generations`. The package itself is
removed in a later, separately agreed step.

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

## Putting a host on the binary's timer

1. Install the released binary at `~/.local/bin/upkeeper`, for example with
   `upkeeper self install --expect-sha256 SHA256` or the standalone bootstrap.
2. From the release checkout, run `upkeeper install-self-timer`. It writes the
   `upkeeper-self.service` and `upkeeper-self.timer` text embedded in the
   binary to `~/.config/systemd/user`, reloads systemd and enables the timer.
   The controller timer on normandy is `upkeeper install-pull-timer`, run from
   `~/Work/dev-fleet`.
3. Roll the binary back with `upkeeper self rollback`. The unit never changes:
   it always starts `~/.local/bin/upkeeper`, and the generation behind that
   link is what moves.

A host may still carry the canary drop-in
`~/.config/systemd/user/upkeeper-self.service.d/10-upkeeper-binary.conf` (or
the `upkeeper-pull.service.d` one on normandy) from before the Go base units.
When that file is byte for byte the drop-in UpKeeper shipped, the installer
writes the base unit first, then removes the drop-in and the emptied `.d`
directory, then reloads systemd, and reports what it removed in its output and
in `--json`. An interrupted install leaves the unit running the binary through
the new unit, the drop-in or both, and running the installer again finishes
the job. Any other drop-in, including a modified copy of the legacy one, a
symbolic link under its name or a `.d` directory that is itself a symbolic
link, refuses the install before anything is written and names the file to
inspect and why. A failure after something changed (for example a failed
`daemon-reload`) exits 20 and still reports what was written and removed.

> Retired 2026-10-07: the Python canary rollback. Hosts first moved onto the
> binary by copying `scripts/upkeeper-self.service.d/10-upkeeper-binary.conf`
> next to the committed Python unit, and rolled back by deleting it. That
> procedure, the Python base units and timers under `scripts/`, their
> installers `scripts/install-self-timer` and `scripts/install-pull-timer`, the
> launchers `scripts/upkeeper-self-pull` and `scripts/upkeeper-scheduled-pull`
> and the manual `scripts/validate-self-canary.py` check were removed once
> every scheduled host ran the Go base unit. The history is in Git.

## What the binary does not carry over

Two things are deliberately absent, and a reader comparing the two
implementations will notice them.

The historical F03 Python API held a richer record store: frozen intent, a
hash-chained event journal, `expected_revision` fences and orphan recovery. No
CLI command reached it. Python 0.2.0 removes dev_fleet.upkeeper, ansible_backend,
foundation_observer and adoption, ending API support including unknown callers.
Its last-supported source is 40e29c6c4b0b1dcc258e42f4aeba6ecc023c2460 / package
0.1.0; existing published artifacts and installed hosts are unchanged.

The live fleet records remain `src/dev_fleet/records.py`, reproduced by
`internal/runstore` field for field and crash-safe by atomic rename. That store
does not provide the retired saga's journal, fences or reconciliation. Future S6
durable operations require their own qualified executor/journal/fence and
independent acceptance; this retirement does not port or waive those requirements.

`self-pull` no longer re-executes itself after a refresh. The Python launcher
*was* the checkout, so refreshing and re-executing guaranteed that the gated
code ran; a binary is not the checkout, and re-executing it restores nothing.
The invariant is checked instead: after a refresh, the binary compares its own
source commit against the refreshed checkout and refuses to converge when the
program differs, naming the commit whose artifact the host needs. A binary
older than the checkout is normal and allowed, because main advances on every
release capture without the Go program changing.

## Self-update from the release pin

A release may pin UpKeeper's own binary in an optional `upkeeper` component:

```json
"upkeeper": {
  "kind": "upkeeper-binary",
  "repository": "iryzhkov/UpKeeper",
  "tag": "v0.1.31",
  "source_commit": "<40 hex>",
  "toolchain": "go1.24.13",
  "assets": {
    "linux-amd64": {"asset": "upkeeper-linux-amd64", "sha256": "<64 hex>", "size": 9437184},
    "linux-arm64": {"asset": "upkeeper-linux-arm64", "sha256": "<64 hex>", "size": 9043968}
  }
}
```

Releases without it load, converge and roll back as before. The component is
not in the convergence order and `--components` cannot select it: the agent
never converges it.

Code-repository CI does not require the pin's `source_commit` to hold the
program of the commit under test. While release data lives in the code
repository that check is circular: a commit that changes the program can only
be pinned by a later release commit, after its binary is tagged and built, so
the commit itself would always fail. The plan's CI invariant (plan section 4,
"New CI invariant") therefore moves to the configuration repository's CI in
milestone M2b, where a configuration commit may only pin a released UpKeeper
whose program it declares compatible; `release.CheckProgramPin` implements the
comparison for it. Until M2b, a pin can be stale: it names a binary whose
program (every build input between `<source_commit>` and `<checkout>`,
`release.ProgramDiffers`) is not the gated checkout's. The comparison covers
the whole repository, because a production package can import a package
outside `cmd` and `internal`, and that package can embed its own files. Tests,
test fixtures and the release intent are not build inputs, so a commit that
changes only them keeps the program's identity. A changed path is ignored only
when it is proven not to be compiled at either commit:

- it is not a non-test Go file, a `go.mod`, `go.sum`, `go.work` or
  `go.work.sum`, or a file under a `vendor` directory;
- under `go.mod`, `go.sum`, `cmd` and `internal`, it is a `*_test.go` file or
  a non-Go file under a `testdata` directory;
- unless it is a `*_test.go` file, its directory holds no non-test Go file;
- no `//go:embed` directive of a non-test Go file can match it;
- neither commit has a cgo file (one that imports `"C"`), an assembly, C or
  other non-Go source the go command compiles, a symbolic link or a
  submodule, because an `#include` can reach any file, even a `*_test.go`
  one, and a link or a submodule can make any path compiled.

A non-test Go file under `testdata` counts, because a production package can
import its package explicitly. A host refuses a stale pin whole, as
it refused a changed program before self-update: exit 30, a message that says
the pin is stale and the host keeps its binary, and nothing downloaded,
adopted or switched, with any hold left as it was. The next release that pins
a binary built from the current program lets hosts update themselves.

When `self-pull`'s program check finds an identified, clean binary whose
program differs from the refreshed checkout, and the release at that checkout
pins a binary, the host updates itself instead of exiting 30. It also updates
when the program is the same but the binary was built from another commit than
the pin, because the SSH program fence below compares commits; a failure of
that update is reported on stderr and convergence continues. No self-update
runs while the host has a scheduled hold marker.

1. The pin's own program is compared with the checkout again.
2. The asset for this architecture is downloaded from
   `https://github.com/iryzhkov/UpKeeper/releases/download/<tag>/`, and its
   size and sha256 must equal the pin. The `SHA256SUMS` file next to it is
   not consulted. The download is anonymous first. When github.com answers
   it with 404, 401 or 403, as it does for a private repository, the asset is
   resolved through the GitHub REST API instead (`GET
   /repos/<repository>/releases/tags/<tag>`, then `GET
   /repos/<repository>/releases/assets/<id>` with `Accept:
   application/octet-stream`) with a credential found on the host, in this
   order: `GH_TOKEN`, `GITHUB_TOKEN`, then what `gh auth token --hostname
   github.com` prints (bounded to ten seconds, its stderr discarded). The
   credential is sent only in the Authorization header to api.github.com,
   removed from any redirect that leaves that host (the asset redirects to
   GitHub's storage), and never written to a URL, an error, a report or a
   file. The authenticated body is held to the same size and sha256. A host
   with no credential refuses with `cannot download <asset>: the release is
   not public (HTTP 404) and no GitHub credential is available on this host;
   run gh auth login on this host or set GH_TOKEN`; a variable that is set
   but is not a token is refused by name, never by value. A host whose
   scheduled self-pull runs under systemd needs gh on the unit's `PATH`, or
   a token in its environment, for this to work unattended.
3. The staged file runs `version` with an empty `PATH` and a disposable `HOME`,
   and must print exactly `upkeeper <tag> (commit <source_commit>)`.
4. It is installed read-only as
   `~/.local/lib/upkeeper/generations/<sha256>/upkeeper`, `current` is switched
   to it by rename and fsync, and `~/.local/bin/upkeeper` becomes a link to
   `../lib/upkeeper/current/upkeeper`. The binary, its `identity.json` and
   the staged generation directory are each flushed before the directory is
   renamed into `generations/`, and that directory is flushed before
   `current` can name it; a failure of any flush stops the install with
   `current` unchanged. Only after the flush of `generations/` succeeds is
   the generation recorded as durable, by a flushed `.durable` marker inside
   it. A generation that exists without the marker, such as one an earlier
   attempt named before that flush failed, is never trusted for its valid
   digest: an apply, `self install`, adoption, rollback or recovery that
   would reuse or switch to it makes every flush again and refuses, with
   `current` unchanged, for as long as one fails. The first time, whatever that path held
   is adopted as a generation (a copied file, or the recorded link target), so
   a rollback can return to it. Three generations are kept; the current and
   previous ones are never removed. Every switch is appended to
   `~/.local/lib/upkeeper/journal.jsonl`.
5. The process execs the new generation as
   `self-pull --after-update <commit> --update-transaction <id>`, where the
   transaction names the switch just made; every switch replaces
   `~/.local/lib/upkeeper/switch.json` with a fresh one before moving a link.
   That run locks the checkout (an `flock` on its root directory, which every
   refresh also takes, so HEAD cannot move under it), refuses a checkout that
   moved, checks its own program against the checkout, and converges exactly
   the commit it checked: convergence is given that commit and refuses before
   loading anything if HEAD is not it. The lock is held until convergence
   ends. The report and the run record carry a `self_update` entry.

Any refusal before the switch leaves `current` unchanged and the host on the
binary it was running, with the same exit 30 the refusal had before. If the
exec fails, or the new binary's program check fails, the previous generation
is restored before any component work, but only if that update's switch is
still the last change to the installation: the installation lock is released
before the exec, and an operator's `self rollback` or `self install` in that
moment is kept rather than swapped back, with the refusal saying so. An interrupted switch is repaired on
the next run: stray temporary links and staging are removed, and a `current`
that names no complete generation is pointed back at `previous`.

An unidentified or `-dirty` binary, a binary built from a commit the checkout
does not have, and a release with no pin all still refuse. The unit's
`ExecStart` is unchanged. `install-self-timer` removes UpKeeper's own legacy
`10-upkeeper-binary.conf` drop-in and refuses any other drop-in on its units.

By hand:

- `upkeeper self install --expect-sha256 SHA256` installs the running binary
  through the same generation code, after checking its digest and that it
  names a clean source commit. This is the one-time bootstrap for a host whose
  binary predates self-update: stage the verified artifact anywhere, then run
  it with this command.
- `upkeeper self rollback` makes the previous generation current again; a
  second rollback undoes the first. It refuses when `~/.local/bin/upkeeper` is
  not the managed link. Every rollback, by hand or after a failed update,
  writes `~/.local/lib/upkeeper/held`, and while it exists self-pull installs
  nothing (a changed program exits 30; the same program converges). `self
  install` releases it, or the operator removes the file after observing.
- `upkeeper self status [--json]` reports the binary path, the generations and
  which is current and previous.

On the SSH path, `pull` and `scheduled-pull` fence the agent on the pin's
`source_commit`: the target runs `__agent --expect-program <commit>`, and the
payload carries the same commit as `expected_program_commit`. A target built
from another commit refuses before it takes its lock, naming both commits.
An agent from before the fence ignores the payload field, but rejects the
unknown option as a usage error before it reads the payload, so it does no
work; the controller reports that the host's upkeeper predates the fence and
must be updated to the pinned binary. A rollback sends no fence and invokes
plain `__agent`, so restoring older intent never requires the older binary;
the in-process local path is checked by `self-pull` instead. `status` and the probe report each host's
binary version, commit and active generation.

## Gates

`make go-gate` runs the format check, `go vet`, `go test`, `go test -race` and
the cross-builds for `linux/amd64` and `linux/arm64`. `make gate` still runs the
Python reference gate. Both run in CI; the Go gate is a release blocker on its
own.

Local gates and GitHub release builds use exactly Go 1.26.8, pinned by the
`go.mod` toolchain directive and forced by Make through `GOTOOLCHAIN`. The
language directive stays `go 1.24` to preserve its GODEBUG defaults. Run
`make ci-local` for the hosted job set and `make go-reproducible` to compare
two clean clones with separate empty build caches. Both commands should run
from clean committed source, with `TMPDIR` on an executable filesystem.

`upkeeper version` prints the exact source commit the binary was built from.

Release artifacts are built with `-trimpath`, which keeps the absolute path of
the checkout out of the binary. Without it, two clean checkouts of the same
commit produce different bytes and a published checksum can only be reproduced
by someone who happened to build in the same directory. With it, any clean
checkout of the commit reproduces the published checksum exactly, which is what
makes the checksum evidence rather than decoration.

The release workflow requires a successful push run of `ci.yml` for the exact
tagged commit before building either architecture. It then builds, attests
(public repositories only), records toolchain and checksums, writes release
notes, and publishes. A failed, missing or timed-out CI gate blocks publication
whether attestation runs or is skipped.

Build-provenance attestation is audit-only. The trust root is the manifest-pinned
artifact digest plus a reproducible rebuild from the pinned source and toolchain;
an adjacent checksum or attestation does not replace that trust root. GitHub does
not support these attestations for user-owned private repositories, so the
SHA-pinned attestation action runs only when the repository is public. Private
releases explicitly state in their notes that provenance attestation was skipped
because the repository is private. All other release gates and ordering remain
the same; an attestation failure on a public repository still blocks publication.
