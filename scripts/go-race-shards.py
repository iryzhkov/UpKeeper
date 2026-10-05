#!/usr/bin/env python3
"""Run the Go race detector over the whole module as three exhaustive, fixed groups.

The race gate is the same coverage as `go test -race ./...`: every package and every
runnable top-level test, fuzz target and example. It is divided only so that the
author package, whose single race binary outgrew the default ten-minute alarm on the
hosted runner, runs as two separately timed binaries:

* author-catalog: the author package's runnable names that start with TestCatalog;
* author-other: the author package's remaining runnable names, fuzz and examples
  included;
* other-packages: every other package that `go list -race ./...` reports.

Every invocation derives the inventory again from the real toolchain under the race
build, proves that the groups are disjoint and that their union is the inventory, and
refuses rather than guessing when the inventory is empty, duplicated, malformed or too
large to pass on a command line. A run then checks that exactly the planned tests and
packages reported a result, so a name that silently failed to run is a failure too.

No flag weakens the gate: the default race flags and the default per-binary timeout
are kept, nothing is skipped, and -count=1 only stops a cached result being reported
as a fresh measurement. The tool is development and CI tooling and is not part of
the production binary.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import signal
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MODULE = "github.com/iryzhkov/UpKeeper"
AUTHOR_PACKAGE = f"{MODULE}/internal/author"
CATALOG_PREFIX = "TestCatalog"
GROUPS = ("author-catalog", "author-other", "other-packages")
MODES = ("all", *GROUPS, "plan")

# The go command the runner invokes. Tests replace it with a finite fake program;
# it is deliberately not reachable from the command line or the environment.
_GO: tuple[str, ...] = ("go",)

# Linux refuses a single argument longer than MAX_ARG_STRLEN (128 KiB); the whole
# argv is held well under ARG_MAX. A larger inventory is refused, never truncated.
MAX_ARG_BYTES = 120_000
MAX_ARGV_BYTES = 1_000_000
# Inventory listings are captured in memory, so they are bounded.
MAX_LISTING_BYTES = 4 * 1024 * 1024
MAX_ERROR_EXCERPT = 4000

RUNNABLE = re.compile(r"(?:Test|Fuzz|Example)[A-Za-z0-9_]*")
BENCHMARK = re.compile(r"Benchmark[A-Za-z0-9_]*")
PACKAGE = re.compile(r"[A-Za-z0-9_.~/-]+")
LIST_SUMMARY = re.compile(r"ok  \t(?P<package>\S+)\t(?:[0-9]+(?:\.[0-9]+)?s|\(cached\))")
# Characters that carry meaning in Go's RE2 syntax.
REGEX_META = re.compile(r"([\\.+*?()|\[\]{}^$])")


class InventoryError(Exception):
    """The real inventory could not be derived or did not validate."""


class Cancelled(Exception):
    """The runner was asked to stop by SIGINT or SIGTERM."""


def excerpt(text: str) -> str:
    text = text.strip()
    if len(text) > MAX_ERROR_EXCERPT:
        return text[:MAX_ERROR_EXCERPT] + "... [truncated]"
    return text


class Runner:
    """Owns the one child process that may be running, so a signal can reach it."""

    def __init__(self) -> None:
        self.child: subprocess.Popen[bytes] | None = None
        self.cancelled_by: int | None = None

    def handle_signal(self, signum: int, _frame: object) -> None:
        self.cancelled_by = signum
        child = self.child
        if child is not None and child.poll() is None:
            # The child leads its own process group, so go test and the test
            # binaries it started all receive the signal, as from a terminal.
            try:
                os.killpg(child.pid, signum)
            except ProcessLookupError:
                pass

    def check_cancelled(self) -> None:
        if self.cancelled_by is not None:
            raise Cancelled(signal.Signals(self.cancelled_by).name)

    def capture(self, argv: list[str]) -> str:
        """Run a listing command to completion and return its bounded stdout."""
        self.check_cancelled()
        try:
            child = subprocess.Popen(
                argv,
                cwd=ROOT,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                start_new_session=True,
            )
        except OSError as error:
            raise InventoryError(f"cannot run {argv[0]}: {error}") from error
        self.child = child
        try:
            stdout, stderr = child.communicate()
        finally:
            child.wait()
            self.child = None
        self.check_cancelled()
        if len(stdout) > MAX_LISTING_BYTES:
            raise InventoryError(f"{' '.join(argv)} printed more than {MAX_LISTING_BYTES} bytes")
        text = stdout.decode("utf-8", "replace")
        if child.returncode != 0:
            raise InventoryError(
                f"{' '.join(argv)} exited {child.returncode}:\n"
                + excerpt(stderr.decode("utf-8", "replace") + "\n" + text)
            )
        return text

    def stream(self, argv: list[str], on_event) -> int:
        """Run go test -json, print every line of its output and report each event."""
        self.check_cancelled()
        child = subprocess.Popen(argv, cwd=ROOT, stdout=subprocess.PIPE, start_new_session=True)
        self.child = child
        out = sys.stdout.buffer
        try:
            for raw in child.stdout or ():
                try:
                    event = json.loads(raw)
                except ValueError:
                    event = None
                if not isinstance(event, dict):
                    # Anything that is not a test2json event is shown as it came.
                    out.write(raw)
                    out.flush()
                    continue
                output = event.get("Output")
                if isinstance(output, str):
                    out.write(output.encode("utf-8", "replace"))
                    out.flush()
                on_event(event)
        finally:
            # A cancelled run still waits for the child, so nothing is orphaned.
            child.wait()
            self.child = None
        return child.returncode


def go_list_packages(runner: Runner) -> list[str]:
    text = runner.capture([*_GO, "list", "-race", "./..."])
    packages = text.splitlines()
    if not packages:
        raise InventoryError("go list -race ./... reported no packages")
    seen: set[str] = set()
    for package in packages:
        if not PACKAGE.fullmatch(package) or not (
            package == MODULE or package.startswith(MODULE + "/")
        ):
            raise InventoryError(f"go list reported an unexpected package line: {package!r}")
        if package in seen:
            raise InventoryError(f"go list reported {package} twice")
        seen.add(package)
    if AUTHOR_PACKAGE not in seen:
        raise InventoryError(f"go list did not report {AUTHOR_PACKAGE}")
    return packages


def parse_test_list(text: str) -> tuple[list[str], list[str]]:
    """Split `go test -list .` output into runnable names and benchmark names.

    The output must be name lines followed by exactly one summary line for the
    author package. Benchmarks are reported but not run by go test without -bench,
    so they are returned separately and never claimed as race coverage.
    """
    lines = text.splitlines()
    if not lines:
        raise InventoryError("go test -list printed nothing")
    summary = LIST_SUMMARY.fullmatch(lines[-1])
    if summary is None or summary.group("package") != AUTHOR_PACKAGE:
        raise InventoryError(f"go test -list did not end with the author summary: {lines[-1]!r}")
    runnable: list[str] = []
    benchmarks: list[str] = []
    seen: set[str] = set()
    for line in lines[:-1]:
        if RUNNABLE.fullmatch(line):
            target = runnable
        elif BENCHMARK.fullmatch(line):
            target = benchmarks
        else:
            raise InventoryError(f"go test -list printed an unrecognised line: {line!r}")
        if line in seen:
            raise InventoryError(f"go test -list reported {line} twice")
        seen.add(line)
        target.append(line)
    if not runnable:
        raise InventoryError("the author package reported no runnable tests")
    return runnable, benchmarks


def anchored(names: list[str]) -> str:
    """Return a -run pattern that matches exactly these top-level names."""
    return "^(" + "|".join(REGEX_META.sub(r"\\\1", name) for name in names) + ")$"


def check_argv(group: str, argv: list[str]) -> None:
    longest = max(len(arg.encode()) for arg in argv)
    total = sum(len(arg.encode()) + 1 for arg in argv)
    if longest > MAX_ARG_BYTES or total > MAX_ARGV_BYTES:
        raise InventoryError(
            f"the {group} command is too long ({longest} bytes in one argument, {total} in all);"
            " refusing rather than dropping names"
        )


def partition(packages: list[str], runnable: list[str]) -> dict[str, dict]:
    catalog = [name for name in runnable if name.startswith(CATALOG_PREFIX)]
    other = [name for name in runnable if not name.startswith(CATALOG_PREFIX)]
    rest = [package for package in packages if package != AUTHOR_PACKAGE]
    if not catalog or not other:
        raise InventoryError(
            f"both author groups must be nonempty (catalog {len(catalog)}, other {len(other)})"
        )
    if set(catalog) & set(other) or set(catalog) | set(other) != set(runnable):
        raise InventoryError("the author groups are not an exact partition of the inventory")
    if len(catalog) + len(other) != len(runnable):
        raise InventoryError("the author groups do not account for every name once")
    if not rest or set(rest) | {AUTHOR_PACKAGE} != set(packages):
        raise InventoryError("the other-packages group is not the package set less author")
    base = [*_GO, "test", "-race", "-count=1", "-json"]
    groups = {
        "author-catalog": {
            "packages": [AUTHOR_PACKAGE],
            "tests": catalog,
            "argv": [*base, "-run", anchored(catalog), AUTHOR_PACKAGE],
        },
        "author-other": {
            "packages": [AUTHOR_PACKAGE],
            "tests": other,
            "argv": [*base, "-run", anchored(other), AUTHOR_PACKAGE],
        },
        "other-packages": {
            "packages": rest,
            "tests": None,
            "argv": [*base, *rest],
        },
    }
    for group, spec in groups.items():
        check_argv(group, spec["argv"])
    return groups


def identity(runner: Runner) -> dict:
    commit = runner.capture(["git", "rev-parse", "HEAD"]).strip()
    dirty = bool(runner.capture(["git", "status", "--porcelain"]).strip())
    version = runner.capture([*_GO, "version"]).strip()
    return {"source_commit": commit, "worktree_dirty": dirty, "go_version": version}


def inventory(runner: Runner) -> dict:
    packages = go_list_packages(runner)
    listing = runner.capture([*_GO, "test", "-race", "-list", ".", AUTHOR_PACKAGE])
    runnable, benchmarks = parse_test_list(listing)
    groups = partition(packages, runnable)
    return {
        **identity(runner),
        "packages": packages,
        "author_runnable": runnable,
        "author_benchmarks_not_run": benchmarks,
        "groups": groups,
        "counts": {
            "packages": len(packages),
            "author_runnable": len(runnable),
            "author-catalog": len(groups["author-catalog"]["tests"]),
            "author-other": len(groups["author-other"]["tests"]),
            "other-packages": len(groups["other-packages"]["packages"]),
        },
    }


def run_group(runner: Runner, group: str, spec: dict) -> dict:
    """Run one group and check that exactly the planned names and packages reported."""
    tests: dict[str, str] = {}
    packages: dict[str, dict] = {}
    terminal = {"pass", "fail", "skip"}

    def on_event(event: dict) -> None:
        action = event.get("Action")
        package = event.get("Package")
        name = event.get("Test")
        if action not in terminal or not isinstance(package, str):
            return
        if name is None:
            packages[package] = {"result": action, "elapsed_s": event.get("Elapsed")}
        elif isinstance(name, str) and "/" not in name and package == AUTHOR_PACKAGE:
            tests[name] = action

    print(f"go-race-shards: {group}: {' '.join(spec['argv'][:6])} ...", flush=True)
    started = time.monotonic()
    status = runner.stream(spec["argv"], on_event)
    elapsed = round(time.monotonic() - started, 3)

    problems: list[str] = []
    if status != 0:
        problems.append(f"go test exited {status}")
    planned_packages = set(spec["packages"])
    if set(packages) != planned_packages:
        missing = sorted(planned_packages - set(packages))
        extra = sorted(set(packages) - planned_packages)
        problems.append(f"package results differ from the plan: missing {missing}, extra {extra}")
    failed_packages = sorted(p for p, r in packages.items() if r["result"] == "fail")
    if failed_packages:
        problems.append(f"failed packages: {failed_packages}")
    receipt: dict = {
        "group": group,
        "exit_status": status,
        "wall_s": elapsed,
        "packages": dict(sorted(packages.items())),
    }
    if spec["tests"] is not None:
        planned = set(spec["tests"])
        if set(tests) != planned:
            problems.append(
                "test results differ from the plan: "
                f"missing {sorted(planned - set(tests))}, extra {sorted(set(tests) - planned)}"
            )
        receipt["tests"] = {
            result: sorted(name for name, r in tests.items() if r == result)
            for result in ("pass", "skip", "fail")
        }
    receipt["problems"] = problems
    receipt["result"] = "pass" if not problems else "fail"
    return receipt


def summary_line(receipt: dict) -> str:
    counts = ""
    if "tests" in receipt:
        counts = " " + ", ".join(f"{len(v)} {k}" for k, v in receipt["tests"].items())
    head = f"go-race-shards: {receipt['group']}: {receipt['result']}"
    return f"{head} in {receipt['wall_s']}s{counts}"


def write_receipt(path: Path | None, document: dict) -> None:
    text = json.dumps(document, indent=2, sort_keys=True) + "\n"
    print("go-race-shards receipt:", flush=True)
    print(text, end="", flush=True)
    if path is not None:
        # A receipt never replaces an existing file.
        with path.open("x", encoding="utf-8") as handle:
            handle.write(text)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Run the Go race detector as three exhaustive, fixed groups."
    )
    parser.add_argument("--group", required=True, choices=MODES)
    parser.add_argument("--receipt", type=Path, help="also write the receipt to this new file")
    args = parser.parse_args(argv)

    runner = Runner()
    previous = {
        signum: signal.signal(signum, runner.handle_signal)
        for signum in (signal.SIGINT, signal.SIGTERM)
    }
    document: dict = {"mode": args.group}
    status = 1
    try:
        plan = inventory(runner)
        document.update(plan)
        if args.group == "plan":
            document["result"] = "planned; no race workload was run"
            status = 0
        else:
            selected = list(GROUPS) if args.group == "all" else [args.group]
            document["runs"] = []
            started = time.monotonic()
            for group in selected:
                receipt = run_group(runner, group, plan["groups"][group])
                document["runs"].append(receipt)
                print(summary_line(receipt), flush=True)
                # A failed group does not stop the next one, so every failure is shown.
                runner.check_cancelled()
            document["wall_s"] = round(time.monotonic() - started, 3)
            ok = all(r["result"] == "pass" for r in document["runs"])
            document["result"] = "pass" if ok else "fail"
            status = 0 if ok else 1
    except InventoryError as error:
        document["result"] = "inventory refused"
        document["error"] = str(error)
        print(f"go-race-shards: {error}", file=sys.stderr, flush=True)
        status = 2
    except Cancelled as error:
        document["result"] = "cancelled"
        document["error"] = f"stopped by {error}"
        print(f"go-race-shards: stopped by {error}", file=sys.stderr, flush=True)
        status = 128 + (runner.cancelled_by or signal.SIGTERM)
    finally:
        for signum, handler in previous.items():
            signal.signal(signum, handler)
    if runner.cancelled_by is not None and status == 0:
        status = 128 + runner.cancelled_by
        document["result"] = "cancelled"
    write_receipt(args.receipt, document)
    return status


if __name__ == "__main__":
    sys.exit(main())
