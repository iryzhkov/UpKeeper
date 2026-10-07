"""Behaviour of scripts/test-affected.py over canned git and go list output.

The planner is pure: every git and go command goes through a runner that these
tests replace, so nothing here runs the real toolchain or make. Only the change
set test uses a real temporary Git repository, because the union of committed,
staged, unstaged and untracked paths is exactly what it has to prove.
"""

import importlib.util
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/test-affected.py"
SPEC = importlib.util.spec_from_file_location("test_affected", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
affected = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(affected)

M = "github.com/iryzhkov/UpKeeper"
BASE_SHA = "27564941dca468ab4f03767929b2f5acb1d21d00"

PACKAGE_DIRS = {
    "cmd/upkeeper": f"{M}/cmd/upkeeper",
    "internal/author": f"{M}/internal/author",
    "internal/cli": f"{M}/internal/cli",
    "internal/policytest": f"{M}/internal/policytest",
    "internal/canonjson": f"{M}/internal/canonjson",
}

# go list -e -f '{{.ImportPath}}|{{.Dir}}' ./...
GO_LIST_DIRS = "".join(f"{pkg}|/repo/{rel}\n" for rel, pkg in PACKAGE_DIRS.items())

# go list -e -test -deps=false -f '{{.ImportPath}}|{{join .Deps ","}}' ./...
# canonjson is imported by author (production) and only by cli's external test;
# author's tests import policytest; cmd/upkeeper imports cli.
GO_LIST_TEST_DEPS = "\n".join(
    [
        f"{M}/internal/canonjson|bytes,fmt",
        f"{M}/internal/policytest|os,testing",
        f"{M}/internal/author|bytes,{M}/internal/canonjson",
        f"{M}/internal/author [{M}/internal/author.test]|bytes,{M}/internal/canonjson",
        f"{M}/internal/author_test [{M}/internal/author.test]|"
        f"{M}/internal/author [{M}/internal/author.test],{M}/internal/policytest",
        f"{M}/internal/author.test|{M}/internal/author [{M}/internal/author.test],"
        f"{M}/internal/author_test [{M}/internal/author.test],{M}/internal/canonjson,"
        f"{M}/internal/policytest",
        f"{M}/internal/cli|fmt,os",
        f"{M}/internal/cli_test [{M}/internal/cli.test]|{M}/internal/canonjson,{M}/internal/cli",
        f"{M}/internal/cli.test|{M}/internal/cli,{M}/internal/cli_test [{M}/internal/cli.test],"
        f"{M}/internal/canonjson",
        f"{M}/cmd/upkeeper|{M}/internal/cli,fmt,os",
    ]
) + "\n"


class FakeRunner:
    """Answers git and go list from canned output and records every command."""

    def __init__(self, *, diff="", untracked="", base_ok=True, call_status=None):
        self.diff = diff
        self.untracked = untracked
        self.base_ok = base_ok
        self.call_status = call_status or {}
        self.captured: list[list[str]] = []
        self.called: list[list[str]] = []

    def capture(self, argv):
        self.captured.append(list(argv))
        if argv[:3] == ["git", "rev-parse", "--verify"]:
            if self.base_ok:
                return 0, BASE_SHA + "\n", ""
            return 128, "", "fatal: Needed a single revision\n"
        if argv[:2] == ["git", "diff"]:
            return 0, self.diff, ""
        if argv[:2] == ["git", "ls-files"]:
            return 0, self.untracked, ""
        if argv[:2] == ["go", "list"] and "-test" in argv:
            return 0, GO_LIST_TEST_DEPS, ""
        if argv[:2] == ["go", "list"]:
            return 0, GO_LIST_DIRS, ""
        raise AssertionError(f"unexpected capture {argv}")

    def call(self, argv):
        self.called.append(list(argv))
        return self.call_status.get(argv[0] if argv[0] != "bash" else "go", 0)


def run_main(runner, *args, env=None):
    return affected.main([*args], runner=runner, root=Path("/repo"), environ=env or {})


def test_requires_base(capsys):
    assert run_main(FakeRunner()) == 2
    assert "usage: make test-affected BASE=<commit> [LIST=1]" in capsys.readouterr().err
    assert run_main(FakeRunner(), "--base", "") == 2
    assert "usage: make test-affected BASE=<commit> [LIST=1]" in capsys.readouterr().err


def test_rejects_unknown_base(capsys):
    runner = FakeRunner(base_ok=False)
    assert run_main(runner, "--base", "no-such-commit") == 2
    assert runner.captured[0] == ["git", "rev-parse", "--verify", "no-such-commit^{commit}"]
    assert "no-such-commit" in capsys.readouterr().err
    assert runner.called == []


@pytest.mark.parametrize(
    ("path", "rule", "package"),
    [
        # (a) documentation
        ("docs/developer-testing.md", "ignored", None),
        ("docs/plans/notes.txt", "ignored", None),
        ("README.md", "ignored", None),
        # (b) inside a package directory, testdata included
        ("internal/author/catalog.go", "package", f"{M}/internal/author"),
        ("internal/author/catalog_test.go", "package", f"{M}/internal/author"),
        ("internal/author/testdata/fixture.json", "package", f"{M}/internal/author"),
        ("cmd/upkeeper/main.go", "package", f"{M}/cmd/upkeeper"),
        # (c) module files
        ("go.mod", "all-go", None),
        ("go.sum", "all-go", None),
        # (d) Python only
        ("src/dev_fleet/probe.py", "pytest", None),
        ("tests/test_ci_workflows.py", "pytest", None),
        ("scripts/go-race-shards.py", "pytest", None),
        ("pyproject.toml", "pytest", None),
        ("uv.lock", "pytest", None),
        # (e) everything else
        ("tests/fixtures/localhost_inventory.yml", "everything", None),
        ("desired/release.json", "everything", None),
        ("Makefile", "everything", None),
        ("scripts/with-policy-test-inputs.sh", "everything", None),
        ("internal/README.md", "everything", None),
        # a deleted package: its directory is no longer listed by go list
        ("internal/removed/removed.go", "everything", None),
        # a Go file in a directory below a package is not that package's source
        ("internal/author/gone/gone.go", "everything", None),
        # a directory prefix is a whole path component
        ("cmd/upkeeper-extra/x.go", "everything", None),
    ],
)
def test_classification_table(path, rule, package):
    got = affected.classify(path, PACKAGE_DIRS)
    assert (got.rule, got.package) == (rule, package)
    if rule == "everything":
        assert path in got.reason


def test_package_dirs_relative_to_root():
    runner = FakeRunner()
    assert affected.package_dirs(runner, Path("/repo")) == PACKAGE_DIRS


def test_reverse_closure_includes_test_only_importers():
    graph = affected.parse_test_deps(GO_LIST_TEST_DEPS)
    modules = set(PACKAGE_DIRS.values())
    # cli imports canonjson only from its external test; author imports it in
    # production; cmd/upkeeper does not depend on canonjson at all.
    assert affected.reverse_closure({f"{M}/internal/canonjson"}, graph, modules) == [
        f"{M}/internal/author",
        f"{M}/internal/canonjson",
        f"{M}/internal/cli",
    ]
    # cmd/upkeeper imports cli; author's tests import policytest.
    assert affected.reverse_closure({f"{M}/internal/cli"}, graph, modules) == [
        f"{M}/cmd/upkeeper",
        f"{M}/internal/cli",
    ]
    assert affected.reverse_closure({f"{M}/internal/policytest"}, graph, modules) == [
        f"{M}/internal/author",
        f"{M}/internal/policytest",
    ]


def git(root, *args):
    subprocess.run(["git", *args], cwd=root, check=True, capture_output=True)


def test_untracked_and_unstaged_paths_included(tmp_path):
    git(tmp_path, "init", "-q")
    git(tmp_path, "config", "user.email", "test@example.invalid")
    git(tmp_path, "config", "user.name", "test")
    for name in ("committed.txt", "unstaged.txt", "renamed.txt", "deleted.txt"):
        (tmp_path / name).write_text(name)
    (tmp_path / ".gitignore").write_text("ignored.txt\n")
    git(tmp_path, "add", ".")
    git(tmp_path, "commit", "-qm", "base")
    base = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=tmp_path, check=True, capture_output=True, text=True
    ).stdout.strip()
    (tmp_path / "committed.txt").write_text("changed")
    git(tmp_path, "commit", "-qam", "later")
    (tmp_path / "unstaged.txt").write_text("changed")
    (tmp_path / "staged.txt").write_text("new")
    git(tmp_path, "add", "staged.txt")
    git(tmp_path, "mv", "renamed.txt", "moved.txt")
    (tmp_path / "deleted.txt").unlink()
    (tmp_path / "untracked.txt").write_text("new")
    (tmp_path / "ignored.txt").write_text("ignored")

    paths = affected.changed_paths(affected.Runner(tmp_path), base)
    assert paths == [
        "committed.txt",
        "deleted.txt",
        "moved.txt",
        "renamed.txt",
        "staged.txt",
        "unstaged.txt",
        "untracked.txt",
    ]


def test_no_changes_exits_zero(capsys):
    runner = FakeRunner()
    assert run_main(runner, "--base", "HEAD") == 0
    out = capsys.readouterr().out
    assert f"test-affected: no changes against {BASE_SHA}; nothing to run" in out
    assert runner.called == []
    # Documentation only is the same: nothing is affected.
    runner = FakeRunner(diff="docs/go-binary.md\nREADME.md\n")
    assert run_main(runner, "--base", "HEAD") == 0
    out = capsys.readouterr().out
    assert "docs/go-binary.md: ignored" in out
    assert f"test-affected: no changes against {BASE_SHA}; nothing to run" in out
    assert runner.called == []


def test_policy_wrapper_only_for_policytest_dependents():
    graph = affected.parse_test_deps(GO_LIST_TEST_DEPS)
    policy = f"{M}/internal/policytest"
    assert affected.needs_policy_inputs([f"{M}/internal/author"], graph)
    assert not affected.needs_policy_inputs([f"{M}/internal/cli", f"{M}/cmd/upkeeper"], graph)
    assert affected.needs_policy_inputs([policy], graph)

    wrapped = affected.go_command([f"{M}/internal/author"], policy_inputs=True, environ={})
    assert wrapped[:2] == ["bash", "scripts/with-policy-test-inputs.sh"]
    assert wrapped[2:] == [
        "go", "test", "-race", "-count=1", "-timeout=25m", f"{M}/internal/author"
    ]
    plain = affected.go_command([f"{M}/internal/cli"], policy_inputs=False, environ={})
    assert plain == ["go", "test", "-race", "-count=1", "-timeout=25m", f"{M}/internal/cli"]

    runner = FakeRunner(diff="internal/cli/data_roots.go\n")
    assert run_main(runner, "--base", "HEAD") == 0
    assert runner.called[0][0] == "go"
    runner = FakeRunner(diff="internal/canonjson/x.go\n")
    assert run_main(runner, "--base", "HEAD") == 0
    assert runner.called[0][:2] == ["bash", "scripts/with-policy-test-inputs.sh"]


@pytest.mark.parametrize(
    ("goflags", "expected"),
    [
        ("", ["-timeout=25m"]),
        ("-mod=readonly", ["-timeout=25m"]),
        ("-timeout=40m", []),
        ("-mod=readonly -timeout=5m", []),
        ("--timeout=5m", []),
    ],
)
def test_caller_timeout_wins(goflags, expected):
    argv = affected.go_command([f"{M}/internal/cli"], policy_inputs=False,
                               environ={"GOFLAGS": goflags})
    assert argv == ["go", "test", "-race", "-count=1", *expected, f"{M}/internal/cli"]


def test_list_mode_runs_nothing(capsys):
    runner = FakeRunner(diff="internal/canonjson/x.go\nMakefile\n", untracked="tests/test_new.py\n")
    assert run_main(runner, "--base", "HEAD", "--list") == 0
    assert runner.called == []
    out = capsys.readouterr().out
    assert "Makefile: every Go package and pytest" in out
    assert "tests/test_new.py: pytest only" in out
    for package in PACKAGE_DIRS.values():
        assert f"  {package}\n" in out
    assert "policy fixtures: needed" in out
    assert "pytest: runs" in out
    assert "bash scripts/with-policy-test-inputs.sh go test -race -count=1 -timeout=25m" in out
    assert "  make test\n" in out
    assert "nothing was run" in out


def test_go_and_pytest_both_run_and_any_failure_fails(capsys):
    runner = FakeRunner(diff="internal/cli/x.go\ntests/test_x.py\n", call_status={"go": 1})
    assert run_main(runner, "--base", "HEAD") == 1
    assert [argv[0] for argv in runner.called] == ["go", "make"]
    assert runner.called[1] == ["make", "test"]
    runner = FakeRunner(diff="internal/cli/x.go\ntests/test_x.py\n")
    assert run_main(runner, "--base", "HEAD") == 0
    runner = FakeRunner(diff="tests/test_x.py\n", call_status={"make": 2})
    assert run_main(runner, "--base", "HEAD") == 1
    assert runner.called == [["make", "test"]]
