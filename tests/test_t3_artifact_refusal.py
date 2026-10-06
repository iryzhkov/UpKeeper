import json
from typing import Any, cast

import pytest
from test_release import FakeMachine
from test_release_u0 import remote_namespace

from dev_fleet.release import CaptureError, _capture_t3


@pytest.mark.parametrize("artifact", [None, {}, {"source_commit": "pending"}])
@pytest.mark.parametrize("handler", ["observe_t3", "apply_t3"])
def test_direct_remote_controlled_entry_refuses_before_effects(tmp_path, artifact, handler):
    ns = cast(dict[str, Any], remote_namespace(tmp_path))
    ns["run"] = lambda *args, **kwargs: pytest.fail("command before controlled refusal")
    ns["must"] = lambda *args, **kwargs: pytest.fail("mutation before controlled refusal")
    spec = {"version": "0.0.45", "artifact": artifact}
    args = (spec,) if handler == "observe_t3" else (spec, {}, [])
    with pytest.raises(ns["StepFailed"], match="Go agent"):
        ns[handler](*args)


@pytest.mark.parametrize("handler", ["observe_t3", "apply_t3"])
def test_retained_state_survives_removed_package_marker(tmp_path, handler):
    ns = cast(dict[str, Any], remote_namespace(tmp_path))
    state = tmp_path / ".local/state/dev-fleet/t3-controlled"
    state.mkdir(parents=True)
    (state / "orphaned-pin").write_text("{}")
    ns["run"] = lambda *args, **kwargs: pytest.fail("command with retained provenance")
    args = ({"version": "0.0.45"},)
    if handler == "apply_t3":
        args += ({"installed": "0.0.45", "served": "0.0.45", "service_active": True}, [])
    with pytest.raises(ns["StepFailed"], match="Go agent"):
        ns[handler](*args)


def test_metadata_only_legacy_operation_refuses_before_npm_install(tmp_path):
    ns = cast(dict[str, Any], remote_namespace(tmp_path))
    prefix = tmp_path / "prefix"
    package = prefix / "lib/node_modules/t3/package.json"
    package.parent.mkdir(parents=True)
    package.write_text(json.dumps({"upkeeperControlledBuild": {}}))
    ns["run"] = lambda *args, **kwargs: {"ok": True, "output": str(prefix)}
    ns["must"] = lambda *args, **kwargs: pytest.fail("npm fallback or restart")
    with pytest.raises(ns["StepFailed"], match="Go agent"):
        ns["apply_t3"](
            {"version": "0.0.45"},
            {"installed": "0.0.45", "served": "0.0.45", "service_active": True},
            [],
        )


def test_python_capture_retained_previous_and_metadata_refuse(tmp_path):
    steward = {"path": ".local/bin/t3-steward", "version": "0.11.0"}
    machine = FakeMachine()
    with pytest.raises(CaptureError, match="Go agent"):
        _capture_t3(machine, tmp_path, steward, {"artifact": None})
    state = tmp_path / ".local/state/dev-fleet/t3-controlled"
    state.mkdir(parents=True)
    with pytest.raises(CaptureError, match="Go agent"):
        _capture_t3(machine, tmp_path, steward)


def test_python_capture_refuses_lost_origin(tmp_path):
    class Ambiguous(FakeMachine):
        def __call__(self, argv, cwd, timeout):
            result = super().__call__(argv, cwd, timeout)
            if argv[:2] == ["npm", "ls"]:
                result.stdout = '{"dependencies":{"t3":{"version":"0.0.38"}}}'
            return result

    with pytest.raises(CaptureError, match="origin ambiguous"):
        _capture_t3(
            Ambiguous(), tmp_path, {"path": ".local/bin/t3-steward", "version": "0.11.0"}
        )
