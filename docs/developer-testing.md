# Developer testing

This page is for whoever edits UpKeeper and wants a fast, honest test loop. The
gate is unchanged: `GOFLAGS=-timeout=25m make ci-local` still runs the whole
hosted job set once per chain, and nothing here replaces it.

## The edit-test loop: `make test-affected`

```
make test-affected BASE=<commit>          # plan, then run
make test-affected BASE=<commit> LIST=1   # plan and the exact commands; runs nothing
```

`BASE` is required; without it the target prints
`usage: make test-affected BASE=<commit> [LIST=1]` and exits 2, and a `BASE`
that is not a commit also exits 2. The change set is every path that differs
from `BASE`: committed since it, staged, unstaged and untracked (ignored files
excluded), that is `git diff --name-only --no-renames <base>` plus
`git ls-files --others --exclude-standard`. With no changes, or with only
documentation changes, it prints
`test-affected: no changes against <sha>; nothing to run` and exits 0.

Each path is classified; the first matching rule wins and is printed next to
the path.

| Rule | Paths | Runs |
| --- | --- | --- |
| a | `docs/**`, a top-level `*.md` | nothing |
| b | a path inside a Go package directory (the longest one that contains it, `testdata/` included), or a `*.go` file whose own directory is a package | that package and its reverse dependencies |
| c | `go.mod`, `go.sum` | every Go package |
| d | `src/**`, `tests/*.py`, `scripts/*.py`, `pyproject.toml`, `uv.lock` | pytest |
| e | anything else, for example `Makefile`, `tests/fixtures/**`, `desired/**`, `shared/**`, `testdata/**` at the root, a shell script, a deleted `.go` file whose directory is no longer a package | every Go package and pytest |

Rule e is deliberately broad: Go tests read `../../shared`, `../../testdata`,
`../../tests/fixtures` and `desired/`, and the Python tests read the Makefile,
the workflows, `scripts/`, `shared/` and `desired/`, so a narrower mapping would
miss tests.

The affected Go packages are the changed packages plus every package in the
module whose production build or test build depends on one of them, including
importers that only import it from a `_test.go` file. They come from
`go list -test -deps=false -f '{{.ImportPath}}|{{join .Deps ","}}' ./...`. They
run as

```
go test -race -count=1 -timeout=25m <packages>
```

If `GOFLAGS` already contains a `-timeout=` word, the flag is left out so that
the caller's value wins. When an affected package's tests depend on
`internal/policytest` (today agent, author, converge, fleet, publish and
release), the command runs under `scripts/with-policy-test-inputs.sh`, which
prepares the pinned policy fixtures and is a no-op when
`UPKEEPER_POLICY_INPUTS_READY=1` is already set. Preparing them costs about a
minute on a warm Go cache, so for many iterations prepare them once (see the
script) and export the variables it writes.

Pytest runs as `make test`, the whole suite, whenever a path matched rule d or
e; there is no per-file mapping. The Go run comes first, pytest second, both
run even if the first fails, and the target exits 0 only when every run passed.

## The race groups

`make go-race` and the CI matrix run the race detector as four exhaustive
groups, derived again from the real inventory on every run by
`scripts/go-race-shards.py`, which refuses a gap or an overlap:

- author-baseline: author tests named `TestCatalogBaseline*`;
- author-catalog: the remaining author tests named `TestCatalog*`;
- author-other: the remaining author tests, fuzz targets and examples;
- other-packages: every other package.

Run one group with `make ci-race GROUP=<group>`; the summary line
`go-race-shards: <group>: pass in <seconds>s ...` carries its wall time, and
`python3 scripts/go-race-shards.py --group plan` prints the inventory and the
exact commands without running them.

To find what is slow in a group, run its `go test -race -count=1 -json -run ...`
command from the plan and sort the `pass` events by `Elapsed`, or profile one
test with
`go test -race -count=1 -run '^TestName$' -cpuprofile cpu.out ./internal/author`
and `go tool pprof -top cpu.out`. The race detector multiplies CPU cost several
times, so profile under `-race`, which is what the gate runs.

The author-baseline and author-catalog groups run their independent tests and
subtests in parallel (`t.Parallel()`). A test may run in parallel only when its
state is its own: its own `t.TempDir()`, no `t.Setenv`, no change to a
package-level variable. Tests that count this process's open file descriptors
(`snapshotFDCount` reads `/proc/self/fd`) must never run in parallel with
anything, because descriptor counts are process-wide; Go runs parallel
top-level tests only after every sequential one has finished, so keeping those
tests sequential is enough. Expensive immutable fixtures are built once and
each test or subtest receives its own copy before it changes anything.

## Timing rules for tests

A test must not assert an elapsed wall-clock window or wait on a short
real-time budget. Instead:

- inject a clock, as internal/agent/component_fleet_reload_test.go does;
- lower an unexported budget: production keeps its value in a package-level
  variable that defaults to the literal, and the test lowers it for its own
  run (such a test then cannot run in parallel);
- assert bounded work or the relation between deadlines, for example that a
  child context's `Deadline()` is not later than its parent's, instead of how
  long something took.

A hang guard, a select or poll that fails only if something blocks, is fine,
but give it at least 10 seconds: its claim is "does not block indefinitely",
and a loaded CI host must not turn it into a flake. A negative wait, which
proves that something does not happen within a window, stays as it is.
Retry loops (for example around ETXTBSY) poll until a deadline of about 10
seconds instead of counting a fixed number of short sleeps.
