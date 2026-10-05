"""Exercise the real fixture helper with bounded, network-free Git and Go stubs."""

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/prepare-policy-test-inputs.sh"
STEWARD = "d2327563e90e9b66adaf80c5a7a03746fce16763"
LEGACY = "88d9b534a6648e2615037dc3a655e6a8d0e9bebc"
SHARED = "b32c0b6f48e12b7d74a60f8a010f84bab73d299f"
TREE = "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52"

STUB = r"""
import json
import os
import sys
from pathlib import Path

args = sys.argv[1:]
tool = Path(sys.argv[0]).name
with open(os.environ["STUB_LOG"], "a") as log:
    log.write(json.dumps({"tool": tool, "args": args, "cwd": os.getcwd(),
                          "no_replace": os.environ.get("GIT_NO_REPLACE_OBJECTS"),
                          "cgo": os.environ.get("CGO_ENABLED")}) + "\n")
if tool == "go":
    assert args[0:2] == ["build", "-o"] and args[3:] == ["./cmd/t3-steward"]
    output = Path(args[2])
    output.write_text("#!/bin/sh\nexit 0\n")
    if os.environ.get("STUB_FAULT") != "build-mode":
        output.chmod(0o755)
elif args[:3] == ["clone", "--quiet", "--no-checkout"]:
    assert len(args) == 5
    assert args[3] in ("https://github.com/iryzhkov/t3-steward.git",
                       "https://github.com/iryzhkov/omarchy-setup.git")
    repository = Path(args[4])
    repository.mkdir()
    (repository / "origin").write_text(args[3])
elif args[:1] == ["-C"]:
    repository = Path(args[1])
    command = args[2:]
    if command[:3] == ["checkout", "--quiet", "--detach"]:
        assert len(command) == 4
        (repository / "head").write_text(command[3])
    elif command == ["rev-parse", "HEAD"]:
        value = (repository / "head").read_text()
        if repository.name == "shared-setup" and os.environ.get("STUB_FAULT") == "commit":
            value = "0" * 40
        print(value)
    elif command == ["rev-parse", "HEAD^{tree}"]:
        assert repository.name == "shared-setup"
        print("0" * 40 if os.environ.get("STUB_FAULT") == "tree"
              else "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52")
    else:
        raise AssertionError(command)
else:
    raise AssertionError(args)
"""


@pytest.fixture
def helper(tmp_path):
    commands = tmp_path / "commands"
    commands.mkdir()
    for name in ("git", "go"):
        executable = commands / name
        executable.write_text(f"#!{sys.executable}\n" + STUB)
        executable.chmod(0o755)
    runner = tmp_path / "runner"
    runner.mkdir()
    env_file, path_file, log = (tmp_path / name for name in ("env", "path", "calls"))
    for file in (env_file, path_file, log):
        file.write_text("")
    environment = {
        **os.environ,
        "PATH": f"{commands}:/usr/bin:/bin",
        "RUNNER_TEMP": str(runner),
        "GITHUB_ENV": str(env_file),
        "GITHUB_PATH": str(path_file),
        "STUB_LOG": str(log),
        "GIT_NO_REPLACE_OBJECTS": "0",
    }
    environment.pop("STUB_FAULT", None)

    def run(fault=""):
        result = subprocess.run(
            ["bash", str(SCRIPT)], env={**environment, "STUB_FAULT": fault},
            capture_output=True, text=True, timeout=15,
        )
        exports = dict(line.split("=", 1) for line in env_file.read_text().splitlines())
        calls = [json.loads(line) for line in log.read_text().splitlines()]
        return result, exports, path_file.read_text().splitlines(), calls

    return run


def test_prepares_separate_verified_fixtures_and_executable(helper):
    result, exports, paths, calls = helper()
    assert result.returncode == 0, result.stderr
    assert set(exports) == {
        "M14_GENERATOR_REPOSITORY", "M14_GENERATOR_COMMIT",
        "M14_SHARED_GENERATOR_REPOSITORY", "M14_SHARED_GENERATOR_COMMIT",
    }
    assert exports["M14_GENERATOR_COMMIT"] == LEGACY
    assert exports["M14_SHARED_GENERATOR_COMMIT"] == SHARED
    legacy = Path(exports["M14_GENERATOR_REPOSITORY"])
    shared = Path(exports["M14_SHARED_GENERATOR_REPOSITORY"])
    assert legacy != shared and legacy.parent == shared.parent
    for repository, commit in ((legacy, LEGACY), (shared, SHARED)):
        assert (repository / "origin").read_text() == (
            "https://github.com/iryzhkov/omarchy-setup.git"
        )
        assert (repository / "head").read_text() == commit
    steward = legacy.parent / "steward"
    assert (steward / "head").read_text() == STEWARD
    assert (steward / "origin").read_text() == "https://github.com/iryzhkov/t3-steward.git"
    assert paths == [str(legacy.parent / "bin")]
    executable = Path(paths[0]) / "t3-steward"
    assert executable.is_file() and os.access(executable, os.X_OK)
    build = [call for call in calls if call["tool"] == "go"]
    assert len(build) == 1 and build[0]["cwd"] == str(steward)
    assert build[0]["cgo"] == "0"
    git_calls = [call for call in calls if call["tool"] == "git"]
    assert all(call["no_replace"] == "1" for call in git_calls)
    for repository in (steward, legacy, shared):
        assert ["-C", str(repository), "rev-parse", "HEAD"] in [
            call["args"] for call in git_calls
        ]
    assert ["-C", str(shared), "rev-parse", "HEAD^{tree}"] in [
        call["args"] for call in git_calls
    ]


@pytest.mark.parametrize("fault", ["commit", "tree", "build-mode"])
def test_refuses_invalid_identity_or_nonexecutable_build_without_exports(helper, fault):
    result, exports, paths, _ = helper(fault)
    assert result.returncode != 0
    assert exports == {}
    assert paths == []
