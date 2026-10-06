"""Behaviour of the race shard runner against a finite fake go program.

The fake program stands in for the toolchain only to exercise the runner's
validation, partition and failure handling. It is never evidence of real race
coverage; that comes from running the script against the real toolchain.
"""

import importlib.util
import json
import os
import re
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/go-race-shards.py"
SPEC = importlib.util.spec_from_file_location("go_race_shards", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
shards = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(shards)

AUTHOR = shards.AUTHOR_PACKAGE
OTHERS = [f"{shards.MODULE}/cmd/upkeeper", f"{shards.MODULE}/internal/agent"]
PACKAGES = [OTHERS[0], AUTHOR, OTHERS[1]]
RUNNABLE = [
    "TestCatalogOne",
    "TestCatalogBaselineOne",
    "TestCatalogTwo",
    "TestAuthorOne",
    "FuzzAuthor",
    "ExampleAuthor",
]

FAKE_GO = r"""
import json, os, re, signal, sys, time

spec = json.load(open(os.environ["FAKE_GO_SPEC"]))
args = sys.argv[1:]
with open(os.environ["FAKE_GO_LOG"], "a") as log:
    log.write(json.dumps(args) + "\n")

def emit(**event):
    print(json.dumps(event), flush=True)

if args[0] == "version":
    print("go version go0.0-fake linux/amd64")
elif args[0] == "list":
    sys.stdout.write(spec["go_list"])
    sys.exit(spec.get("go_list_exit", 0))
elif "-list" in args:
    sys.stdout.write(spec["test_list"])
    sys.stderr.write(spec.get("test_list_stderr", ""))
    sys.exit(spec.get("test_list_exit", 0))
else:
    if spec.get("hang"):
        def stop(signum, frame):
            open(spec["hang"] + ".signalled", "w").write(str(signum))
            sys.exit(128 + signum)
        signal.signal(signal.SIGTERM, stop)
        open(spec["hang"], "w").write(str(os.getpid()))
        time.sleep(60)
        sys.exit(0)
    failed = False
    if "-run" in args:
        pattern = args[args.index("-run") + 1]
        package = args[-1]
        for name in spec["test_names"]:
            if not re.fullmatch(pattern, name):
                continue
            result = spec.get("results", {}).get(name, "pass")
            if result == "omit":
                continue
            emit(Action="run", Package=package, Test=name)
            emit(Action="output", Package=package, Test=name,
                 Output=f"--- {result.upper()}: {name} (0.00s)\n")
            emit(Action="run", Package=package, Test=name + "/sub")
            emit(Action=result, Package=package, Test=name + "/sub")
            emit(Action=result, Package=package, Test=name)
            failed = failed or result == "fail"
        packages = [package]
    else:
        packages = [a for a in args if not a.startswith("-") and a != "test"]
    packages += spec.get("extra_packages", [])
    for package in packages:
        if package in spec.get("omit_packages", []):
            continue
        result = spec.get("package_results", {}).get(package, "fail" if failed else "pass")
        failed = failed or result == "fail"
        emit(Action="output", Package=package, Output=f"{result}\t{package}\n")
        emit(Action=result, Package=package, Elapsed=0.5)
    print("not a json line", flush=True)
    sys.exit(spec.get("run_exit", 1 if failed else 0))
"""


def listing(names, package=AUTHOR, summary="0.010s"):
    return "".join(f"{name}\n" for name in names) + f"ok  \t{package}\t{summary}\n"


@pytest.fixture
def fake(tmp_path, monkeypatch):
    """Point the runner at the fake go program and return a spec writer."""
    program = tmp_path / "fake_go.py"
    program.write_text(FAKE_GO)
    spec_path = tmp_path / "spec.json"
    log_path = tmp_path / "log.jsonl"
    log_path.write_text("")
    monkeypatch.setenv("FAKE_GO_SPEC", str(spec_path))
    monkeypatch.setenv("FAKE_GO_LOG", str(log_path))
    monkeypatch.setattr(shards, "_GO", (sys.executable, str(program)))

    class Fake:
        log = log_path
        go = (sys.executable, str(program))

        def __call__(self, **overrides):
            spec = {
                "go_list": "".join(f"{p}\n" for p in PACKAGES),
                "test_list": listing(RUNNABLE + ["BenchmarkAuthor"]),
                "test_names": RUNNABLE,
            }
            spec.update(overrides)
            spec_path.write_text(json.dumps(spec))

        def calls(self):
            return [json.loads(line) for line in self.log.read_text().splitlines()]

        def runs(self):
            return [c for c in self.calls() if c[0] == "test" and "-json" in c]

    return Fake()


def run_main(args, tmp_path):
    receipt = tmp_path / "receipt.json"
    status = shards.main([*args, "--receipt", str(receipt)])
    return status, json.loads(receipt.read_text())


# Inventory parsing


def test_listing_keeps_tests_fuzz_and_examples_and_reports_benchmarks_separately():
    runnable, benchmarks = shards.parse_test_list(listing(RUNNABLE + ["BenchmarkAuthor"]))
    assert runnable == RUNNABLE
    assert benchmarks == ["BenchmarkAuthor"]


def test_listing_accepts_a_cached_summary():
    runnable, _ = shards.parse_test_list(listing(["TestA"], summary="(cached)"))
    assert runnable == ["TestA"]


@pytest.mark.parametrize(
    "text, message",
    [
        ("", "printed nothing"),
        ("TestA\n", "did not end with the author summary"),
        (listing(["TestA"], package=f"{shards.MODULE}/internal/agent"), "author summary"),
        (listing(["TestA", "TestA"]), "twice"),
        (listing(["TestA", "Test-B"]), "unrecognised"),
        (listing(["TestA", "Testé"]), "unrecognised"),
        (listing(["TestA", "helper output"]), "unrecognised"),
        (listing(["TestA", ""]), "unrecognised"),
        (listing(["TestA"]) + "trailing\n", "author summary"),
        (listing(["TestA"]) + listing([]), "unrecognised"),
        (listing([]), "no runnable tests"),
        (listing(["BenchmarkOnly"]), "no runnable tests"),
    ],
)
def test_listing_refuses_malformed_or_incomplete_output(text, message):
    with pytest.raises(shards.InventoryError, match=message):
        shards.parse_test_list(text)


@pytest.mark.parametrize(
    "go_list, exit_code, message",
    [
        ("", 0, "no packages"),
        (f"{AUTHOR}\n{AUTHOR}\n", 0, "twice"),
        (f"{AUTHOR}\nexample.com/foreign\n", 0, "unexpected package"),
        (f"{AUTHOR}\n{shards.MODULE}/x y\n", 0, "unexpected package"),
        (f"{OTHERS[0]}\n", 0, "did not report"),
        (f"{AUTHOR}\n", 1, "exited 1"),
    ],
)
def test_package_inventory_refusals(fake, go_list, exit_code, message):
    fake(go_list=go_list, go_list_exit=exit_code)
    with pytest.raises(shards.InventoryError, match=message):
        shards.go_list_packages(shards.Runner())


# Partition and commands


def test_partition_is_exact_and_assigns_future_names_once():
    future = RUNNABLE + [
        "TestCatalogAddedLater",
        "TestCatalogBaselineAddedLater",
        "FuzzAddedLater",
        "ExampleAddedLater",
    ]
    groups = shards.partition(PACKAGES, future)
    baseline = groups["author-baseline"]["tests"]
    catalog = groups["author-catalog"]["tests"]
    other = groups["author-other"]["tests"]
    assert baseline == ["TestCatalogBaselineOne", "TestCatalogBaselineAddedLater"]
    assert catalog == ["TestCatalogOne", "TestCatalogTwo", "TestCatalogAddedLater"]
    assert other == [
        "TestAuthorOne",
        "FuzzAuthor",
        "ExampleAuthor",
        "FuzzAddedLater",
        "ExampleAddedLater",
    ]
    assert sorted(baseline + catalog + other) == sorted(future)
    assert groups["other-packages"]["packages"] == OTHERS
    assert groups["other-packages"]["argv"][-2:] == OTHERS


@pytest.mark.parametrize(
    "runnable, message",
    [
        (["TestAuthorOne"], "nonempty"),
        (["TestCatalogOne"], "nonempty"),
        (["TestCatalogBaselineOne"], "nonempty"),
        (["TestCatalogOne", "TestAuthorOne"], "baseline 0"),
        (["TestCatalogBaselineOne", "TestAuthorOne"], "catalog 0"),
        (["TestCatalogOne", "TestCatalogBaselineOne"], "other 0"),
    ],
)
def test_partition_refuses_an_empty_author_group(runnable, message):
    with pytest.raises(shards.InventoryError, match=message):
        shards.partition(PACKAGES, runnable)


def test_partition_refuses_when_author_is_the_only_package():
    with pytest.raises(shards.InventoryError, match="other-packages"):
        shards.partition([AUTHOR], RUNNABLE)


def test_run_patterns_are_anchored_to_exact_names():
    groups = shards.partition(PACKAGES, RUNNABLE)
    argv = groups["author-catalog"]["argv"]
    assert argv[-1] == AUTHOR
    assert "-race" in argv and "-count=1" in argv
    for forbidden in ("-short", "-timeout", "-skip", "-tags", "-count=0"):
        assert not any(arg.startswith(forbidden) for arg in argv)
    pattern = argv[argv.index("-run") + 1]
    assert pattern == "^(TestCatalogOne|TestCatalogTwo)$"
    assert re.fullmatch(pattern, "TestCatalogOne")
    for near in (
        "TestCatalogOneMore",
        "XTestCatalogOne",
        "TestCatalog",
        "TestCatalogBaselineOne",
        "TestAuthorOne",
    ):
        assert not re.search(pattern, near)
    baseline = groups["author-baseline"]["argv"]
    assert baseline[baseline.index("-run") + 1] == "^(TestCatalogBaselineOne)$"


def test_anchored_escapes_regex_syntax():
    pattern = shards.anchored(["a.b", "c|d", "(e)"])
    assert pattern == r"^(a\.b|c\|d|\(e\))$"
    assert re.fullmatch(pattern, "c|d") and not re.fullmatch(pattern, "axb")


def test_oversized_command_is_refused_not_truncated(monkeypatch):
    monkeypatch.setattr(shards, "MAX_ARG_BYTES", 40)
    with pytest.raises(shards.InventoryError, match="refusing rather than dropping"):
        shards.partition(PACKAGES, RUNNABLE)


# End-to-end runs against the fake program


def test_plan_lists_the_real_shape_without_running_tests(fake, tmp_path):
    fake()
    status, receipt = run_main(["--group", "plan"], tmp_path)
    assert status == 0
    assert receipt["counts"] == {
        "packages": 3,
        "author_runnable": 6,
        "author-baseline": 1,
        "author-catalog": 2,
        "author-other": 3,
        "other-packages": 2,
    }
    assert receipt["author_benchmarks_not_run"] == ["BenchmarkAuthor"]
    assert receipt["source_commit"] and receipt["go_version"].startswith("go version")
    assert fake.runs() == []
    listings = [c for c in fake.calls() if "-list" in c]
    assert listings == [["test", "-race", "-list", ".", AUTHOR]]
    assert ["list", "-race", "./..."] in fake.calls()


def test_all_runs_every_group_and_checks_each_result(fake, tmp_path, capfd):
    fake()
    status, receipt = run_main(["--group", "all"], tmp_path)
    assert status == 0, receipt
    assert receipt["result"] == "pass"
    assert [run["group"] for run in receipt["runs"]] == list(shards.GROUPS)
    baseline, catalog, other, rest = receipt["runs"]
    assert baseline["tests"]["pass"] == ["TestCatalogBaselineOne"]
    assert catalog["tests"]["pass"] == ["TestCatalogOne", "TestCatalogTwo"]
    assert other["tests"]["pass"] == sorted(["TestAuthorOne", "FuzzAuthor", "ExampleAuthor"])
    assert sorted(rest["packages"]) == sorted(OTHERS)
    assert len(fake.runs()) == 4
    output = capfd.readouterr().out
    assert "--- PASS: FuzzAuthor" in output
    assert "not a json line" in output


def test_single_group_rederives_the_whole_partition(fake, tmp_path):
    fake()
    status, receipt = run_main(["--group", "author-other"], tmp_path)
    assert status == 0
    assert receipt["counts"]["author-catalog"] == 2
    assert [run["group"] for run in receipt["runs"]] == ["author-other"]
    assert len(fake.runs()) == 1


def test_a_failed_shard_fails_the_run_but_later_groups_still_run(fake, tmp_path, capfd):
    fake(results={"TestCatalogTwo": "fail"})
    status, receipt = run_main(["--group", "all"], tmp_path)
    assert status == 1
    assert receipt["result"] == "fail"
    assert receipt["runs"][1]["tests"]["fail"] == ["TestCatalogTwo"]
    assert [run["result"] for run in receipt["runs"]] == ["pass", "fail", "pass", "pass"]
    assert "--- FAIL: TestCatalogTwo" in capfd.readouterr().out


def test_skipped_tests_are_reported_by_name(fake, tmp_path):
    fake(results={"TestAuthorOne": "skip"})
    status, receipt = run_main(["--group", "author-other"], tmp_path)
    assert status == 0
    assert receipt["runs"][0]["tests"]["skip"] == ["TestAuthorOne"]


def test_a_planned_test_without_a_result_fails_even_when_go_exits_zero(fake, tmp_path):
    fake(results={"ExampleAuthor": "omit"})
    status, receipt = run_main(["--group", "author-other"], tmp_path)
    assert status == 1
    assert "missing ['ExampleAuthor']" in receipt["runs"][0]["problems"][0]


@pytest.mark.parametrize(
    "override, message",
    [
        ({"omit_packages": [OTHERS[1]]}, "missing"),
        ({"extra_packages": [f"{shards.MODULE}/internal/surprise"]}, "extra"),
        ({"package_results": {OTHERS[0]: "fail"}}, "failed packages"),
        ({"run_exit": 2}, "go test exited 2"),
    ],
)
def test_package_results_must_match_the_plan(fake, tmp_path, override, message):
    fake(**override)
    status, receipt = run_main(["--group", "other-packages"], tmp_path)
    assert status == 1
    assert any(message in problem for problem in receipt["runs"][0]["problems"])


def test_failed_enumeration_runs_nothing(fake, tmp_path, capfd):
    fake(test_list="", test_list_exit=1, test_list_stderr="cannot build race binary\n")
    status, receipt = run_main(["--group", "all"], tmp_path)
    assert status == 2
    assert receipt["result"] == "inventory refused"
    assert "cannot build race binary" in receipt["error"]
    assert "runs" not in receipt
    assert fake.runs() == []
    assert "cannot build race binary" in capfd.readouterr().err


def test_receipt_never_replaces_an_existing_file(fake, tmp_path):
    fake()
    existing = tmp_path / "receipt.json"
    existing.write_text("keep")
    with pytest.raises(FileExistsError):
        shards.main(["--group", "plan", "--receipt", str(existing)])
    assert existing.read_text() == "keep"


def test_cancellation_reaches_the_child_and_never_reports_a_pass(fake, tmp_path):
    marker = tmp_path / "child.pid"
    fake(hang=str(marker))
    receipt = tmp_path / "receipt.json"
    driver = (
        "import importlib.util, sys\n"
        f"spec = importlib.util.spec_from_file_location('s', {str(SCRIPT)!r})\n"
        "s = importlib.util.module_from_spec(spec); spec.loader.exec_module(s)\n"
        f"s._GO = {fake.go!r}\n"
        f"sys.exit(s.main(['--group', 'all', '--receipt', {str(receipt)!r}]))\n"
    )
    process = subprocess.Popen(
        [sys.executable, "-c", driver], stdout=subprocess.DEVNULL, env=os.environ.copy()
    )
    deadline = time.monotonic() + 30
    while not marker.exists() or not marker.read_text():
        assert process.poll() is None and time.monotonic() < deadline
        time.sleep(0.05)
    child = int(marker.read_text())
    process.send_signal(signal.SIGTERM)
    assert process.wait(timeout=30) == 128 + signal.SIGTERM
    assert Path(str(marker) + ".signalled").read_text() == str(signal.SIGTERM)
    with pytest.raises(ProcessLookupError):
        os.kill(child, 0)
    document = json.loads(receipt.read_text())
    assert document["result"] == "cancelled"
    assert [run["group"] for run in document["runs"]] == ["author-baseline"]
    assert document["runs"][0]["result"] == "fail"
    assert len(fake.runs()) == 1
