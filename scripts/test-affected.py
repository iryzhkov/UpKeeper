#!/usr/bin/env python3
"""Run only the tests a change can affect, for the edit-test loop.

The change set is every path that differs from a base commit: committed since the
base, staged, unstaged and untracked (ignored files excluded). Each path is
classified, first match wins:

  a. docs/** or a top-level *.md: ignored;
  b. a path inside a Go package directory (the longest package directory that
     contains it, testdata included), or a *.go file whose own directory is a
     package: that package;
  c. go.mod or go.sum: every Go package;
  d. src/**, tests/*.py, scripts/*.py, pyproject.toml, uv.lock: pytest only;
  e. anything else, including a deleted .go file whose directory is no longer a
     package: every Go package and pytest, because both suites read files at the
     repository root.

The affected Go packages are the changed packages plus every module package whose
production or test build depends on one of them. They run under the race
detector, inside the policy fixture wrapper when one of them depends on
internal/policytest in its tests; pytest runs as the whole suite. This is a fast
loop, not the gate: `make ci-local` still runs once per chain.

The tool is development tooling and is not part of the production binary.
"""

from __future__ import annotations

import argparse
import os
import shlex
import subprocess
import sys
from collections.abc import Mapping
from pathlib import Path, PurePosixPath
from typing import NamedTuple

ROOT = Path(__file__).resolve().parents[1]
MODULE = "github.com/iryzhkov/UpKeeper"
POLICYTEST = f"{MODULE}/internal/policytest"
USAGE = "usage: make test-affected BASE=<commit> [LIST=1]"
GO_TIMEOUT = "-timeout=25m"
PYTEST_COMMAND = ["make", "test"]
POLICY_WRAPPER = ["bash", "scripts/with-policy-test-inputs.sh"]
PYTHON_ONLY_FILES = {"pyproject.toml", "uv.lock"}
MODULE_FILES = {"go.mod", "go.sum"}

RULE_TEXT = {
    "ignored": "ignored",
    "package": "package",
    "all-go": "every Go package",
    "pytest": "pytest only",
    "everything": "every Go package and pytest",
}


class UsageError(Exception):
    """The command line or the base commit cannot be used."""


class Runner:
    """Runs commands in the repository; tests replace it with canned output."""

    def __init__(self, root: Path) -> None:
        self.root = root

    def capture(self, argv: list[str]) -> tuple[int, str, str]:
        try:
            done = subprocess.run(argv, cwd=self.root, capture_output=True, text=True)
        except OSError as error:
            return 127, "", f"cannot run {argv[0]}: {error}\n"
        return done.returncode, done.stdout, done.stderr

    def call(self, argv: list[str]) -> int:
        sys.stdout.flush()
        return subprocess.call(argv, cwd=self.root)


class Classified(NamedTuple):
    path: str
    rule: str
    package: str | None
    reason: str

    def describe(self) -> str:
        if self.rule == "package":
            return f"{self.path}: package {self.package}"
        return f"{self.path}: {RULE_TEXT[self.rule]} ({self.reason})"


def capture_ok(runner, argv: list[str]) -> str:
    status, stdout, stderr = runner.capture(argv)
    if status != 0:
        raise RuntimeError(f"{shlex.join(argv)} exited {status}: {stderr.strip()}")
    return stdout


def resolve_base(runner, base: str) -> str:
    status, stdout, stderr = runner.capture(["git", "rev-parse", "--verify", f"{base}^{{commit}}"])
    if status != 0 or not stdout.strip():
        raise UsageError(f"test-affected: {base!r} is not a commit: {stderr.strip()}")
    return stdout.strip()


def changed_paths(runner, base: str) -> list[str]:
    """Committed since base, staged, unstaged and untracked paths, sorted."""
    tracked = capture_ok(runner, ["git", "diff", "--name-only", "--no-renames", base])
    untracked = capture_ok(runner, ["git", "ls-files", "--others", "--exclude-standard"])
    return sorted({line for line in (tracked + untracked).splitlines() if line})


def package_dirs(runner, root: Path) -> dict[str, str]:
    """Map each package directory, relative to the root, to its import path."""
    listing = capture_ok(runner, ["go", "list", "-e", "-f", "{{.ImportPath}}|{{.Dir}}", "./..."])
    dirs: dict[str, str] = {}
    for line in listing.splitlines():
        package, sep, directory = line.partition("|")
        if not sep:
            raise RuntimeError(f"go list printed an unexpected line: {line!r}")
        relative = PurePosixPath(os.path.relpath(directory, root)).as_posix()
        dirs[relative] = package
    return dirs


def classify(path: str, dirs: Mapping[str, str]) -> Classified:
    pure = PurePosixPath(path)
    parts = pure.parts
    if parts[0] == "docs" or (len(parts) == 1 and pure.suffix == ".md"):
        return Classified(path, "ignored", None, "documentation")
    if pure.suffix == ".go":
        # A Go file belongs to the package of its own directory, or to none.
        parent = pure.parent.as_posix()
        if parent in dirs:
            return Classified(path, "package", dirs[parent], "Go source")
    else:
        for depth in range(len(parts) - 1, 0, -1):
            prefix = PurePosixPath(*parts[:depth]).as_posix()
            if prefix in dirs:
                return Classified(path, "package", dirs[prefix], "package file")
    if path in MODULE_FILES:
        return Classified(path, "all-go", None, "module requirements")
    if (
        parts[0] == "src"
        or (len(parts) == 2 and parts[0] in {"tests", "scripts"} and pure.suffix == ".py")
        or path in PYTHON_ONLY_FILES
    ):
        return Classified(path, "pytest", None, "Python source")
    return Classified(path, "everything", None, f"no narrower rule for {path}")


def base_package(entry: str) -> str:
    """The package a go list -test entry builds: P, P [P.test], P_test or P.test."""
    name = entry.split(" [", 1)[0]
    if name.endswith(".test"):
        return name[: -len(".test")]
    if name.endswith("_test"):
        return name[: -len("_test")]
    return name


def parse_test_deps(listing: str) -> dict[str, set[str]]:
    """Map each package to everything its production and test builds depend on."""
    graph: dict[str, set[str]] = {}
    for line in listing.splitlines():
        if not line:
            continue
        entry, sep, deps = line.partition("|")
        if not sep:
            raise RuntimeError(f"go list printed an unexpected line: {line!r}")
        package = base_package(entry)
        found = graph.setdefault(package, set())
        for dep in deps.split(","):
            if dep:
                found.add(dep.split(" [", 1)[0])
    return graph


def reverse_closure(
    changed: set[str], graph: Mapping[str, set[str]], modules: set[str]
) -> list[str]:
    """Changed packages plus every module package whose builds depend on one."""
    hit = {package for package in changed if package in modules}
    hit |= {package for package in modules if graph.get(package, set()) & changed}
    return sorted(hit)


def needs_policy_inputs(packages: list[str], graph: Mapping[str, set[str]]) -> bool:
    return any(
        package == POLICYTEST or POLICYTEST in graph.get(package, set()) for package in packages
    )


def caller_sets_timeout(environ: Mapping[str, str]) -> bool:
    return any(
        word.startswith(("-timeout=", "--timeout="))
        for word in environ.get("GOFLAGS", "").split()
    )


def go_command(
    packages: list[str], *, policy_inputs: bool, environ: Mapping[str, str]
) -> list[str]:
    argv = ["go", "test", "-race", "-count=1"]
    if not caller_sets_timeout(environ):
        argv.append(GO_TIMEOUT)
    argv += packages
    return [*POLICY_WRAPPER, *argv] if policy_inputs else argv


def plan(runner, root: Path, base: str, environ: Mapping[str, str]) -> list[list[str]] | None:
    """Print the plan and return its commands, or None when nothing is affected."""
    sha = resolve_base(runner, base)
    paths = changed_paths(runner, base)
    print(f"test-affected: base {sha}")
    if not paths:
        print(f"test-affected: no changes against {sha}; nothing to run")
        return None
    dirs = package_dirs(runner, root)
    classified = [classify(path, dirs) for path in paths]
    print("changed paths:")
    for item in classified:
        print(f"  {item.describe()}")
    rules = {item.rule for item in classified}
    if rules == {"ignored"}:
        print(f"test-affected: no changes against {sha}; nothing to run")
        return None

    modules = set(dirs.values())
    graph = parse_test_deps(
        capture_ok(
            runner,
            ["go", "list", "-e", "-test", "-deps=false", "-f",
             "{{.ImportPath}}|{{join .Deps \",\"}}", "./..."],
        )
    )
    if rules & {"all-go", "everything"}:
        packages = sorted(modules)
    else:
        changed = {item.package for item in classified if item.package is not None}
        packages = reverse_closure(changed, graph, modules)
    policy = needs_policy_inputs(packages, graph)
    pytest = bool(rules & {"pytest", "everything"})

    print("affected Go packages:" if packages else "affected Go packages: none")
    for package in packages:
        print(f"  {package}")
    if packages:
        state = "needed" if policy else "not needed"
        print(f"policy fixtures: {state} (internal/policytest in the affected test dependencies)")
    print("pytest: runs" if pytest else "pytest: skipped (no Python or repository file changed)")
    commands = []
    if packages:
        commands.append(go_command(packages, policy_inputs=policy, environ=environ))
    if pytest:
        commands.append(list(PYTEST_COMMAND))
    print("commands:")
    for argv in commands:
        print(f"  {shlex.join(argv)}")
    return commands


def main(
    argv: list[str] | None = None,
    *,
    runner=None,
    root: Path = ROOT,
    environ: Mapping[str, str] | None = None,
) -> int:
    parser = argparse.ArgumentParser(description="Run the tests a change can affect.")
    parser.add_argument("--base", default="", help="commit the change set is measured from")
    parser.add_argument("--list", action="store_true", help="print the plan and run nothing")
    args = parser.parse_args(argv)
    if not args.base:
        print(USAGE, file=sys.stderr)
        return 2
    runner = runner if runner is not None else Runner(root)
    environ = os.environ if environ is None else environ
    try:
        commands = plan(runner, root, args.base, environ)
    except UsageError as error:
        print(error, file=sys.stderr)
        print(USAGE, file=sys.stderr)
        return 2
    except RuntimeError as error:
        print(f"test-affected: {error}", file=sys.stderr)
        return 2
    if not commands:
        return 0
    if args.list:
        print("test-affected: list mode; nothing was run")
        return 0
    # Every command runs even after a failure, so one run shows every failure.
    failed = []
    for command in commands:
        print(f"test-affected: running {shlex.join(command)}", flush=True)
        status = runner.call(command)
        if status != 0:
            failed.append(f"{command[0] if command[0] != 'bash' else 'go'} exited {status}")
    if failed:
        print(f"test-affected: failed: {'; '.join(failed)}", file=sys.stderr)
        return 1
    print("test-affected: pass")
    return 0


if __name__ == "__main__":
    sys.exit(main())
