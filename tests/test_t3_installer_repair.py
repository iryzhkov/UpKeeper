import json
from typing import Any, cast

import pytest
from test_release_u0 import remote_namespace


@pytest.mark.parametrize("handler", ["observe_t3", "apply_t3"])
@pytest.mark.parametrize("origin", [
    "prefix-failed", "prefix-empty", "prefix-relative", "unreadable",
    "malformed", "plain", "marker-lost", "controlled", "missing-manifest",
])
def test_repair_direct_t3_unknown_origin_refuses(tmp_path, handler, origin):
    ns = cast(dict[str, Any], remote_namespace(tmp_path))
    prefix = tmp_path / "prefix"
    package = prefix / "lib/node_modules/t3/package.json"
    package.parent.mkdir(parents=True)
    metadata: dict[str, Any] = {"name": "t3", "version": "0.0.45"}
    if origin == "controlled":
        metadata["upkeeperControlledBuild"] = {}
    if origin == "marker-lost":
        metadata["_resolved"] = "file:controlled-build.tar.gz"
    package.write_text(json.dumps(metadata))
    if origin == "malformed":
        package.write_text("{")
    if origin == "unreadable":
        package.unlink()
        package.mkdir()
    if origin == "missing-manifest":
        package.unlink()
    output = str(prefix)
    if origin == "prefix-empty":
        output = ""
    if origin == "prefix-relative":
        output = "relative"
    ns["COMPONENTS"] = {"t3-steward": {"path": ".local/bin/t3-steward"}}
    ns["unit_active"] = lambda *a: True

    def run(argv, **kwargs):
        if argv[:2] == ["npm", "prefix"]:
            return {"ok": origin != "prefix-failed", "output": output}
        if argv[:2] == ["npm", "ls"]:
            return {"ok": True, "output": '{"dependencies":{"t3":{"version":"0.0.45"}}}'}
        return {"ok": True, "output": (
            "T3 server 0.0.45 serving\nshell snapshot: 4 threads, 0 running"
        )}

    ns["run"] = run
    ns["must"] = lambda *a, **k: pytest.fail("effect before provenance refusal")
    ns["verify_pair_binary"] = lambda *a: None
    spec = {"version": "0.0.45", "service": "t3code.service"}
    args = (spec,)
    if handler == "apply_t3":
        args += ({"installed": "0.0.45", "served": "0.0.45", "service_active": True}, [])
    with pytest.raises(ns["StepFailed"], match="provenance|Go agent"):
        ns[handler](*args)



def test_repair_missing_installation_observe_and_apply(tmp_path):
    ns = cast(dict[str, Any], remote_namespace(tmp_path))
    prefix = tmp_path / "prefix"
    package = prefix / "lib/node_modules/t3/package.json"
    active = False
    commands = []
    ns["COMPONENTS"] = {"t3-steward": {"path": ".local/bin/t3-steward"}}
    ns["verify_pair_binary"] = lambda *a: None
    ns["unit_active"] = lambda *a: active

    def run(argv, **kwargs):
        if argv[:2] == ["npm", "prefix"]:
            return {"ok": True, "output": str(prefix)}
        if argv[:2] == ["npm", "ls"]:
            dependencies = {"t3": {"version": "0.0.38"}} if package.exists() else {}
            return {"ok": True, "output": json.dumps({"dependencies": dependencies})}
        return {"ok": True, "output": (
            "T3 server 0.0.38 serving\nshell snapshot: 4 threads, 0 running"
        )}

    def must(argv, **kwargs):
        nonlocal active
        commands.append(argv)
        if argv[:2] == ["npm", "install"]:
            package.parent.mkdir(parents=True)
            package.write_text(json.dumps({
                "name": "t3", "version": "0.0.38",
                "_resolved": "https://registry.npmjs.org/t3/-/t3-0.0.38.tgz",
            }))
        if "restart" in argv:
            active = True

    ns["run"] = run
    ns["must"] = must
    spec = {"version": "0.0.38", "service": "t3code.service"}
    before = ns["observe_t3"](spec)
    assert before["installed"] is None
    # A stale numeric observation must not converge over a missing installation.
    before["installed"] = "0.0.38"
    assert ns["apply_t3"](spec, before, []) == "updated"
    assert ["npm", "install", "-g", "t3@0.0.38"] in commands
    assert ["systemctl", "--user", "restart", "t3code.service"] in commands


@pytest.mark.parametrize("origin", ["legacy", "modern", "legacy-npm-ls", "missing"])
def test_repair_direct_t3_positive_upstream_or_absent(tmp_path, origin):
    ns = cast(dict[str, Any], remote_namespace(tmp_path))
    prefix = tmp_path / "prefix"
    if origin != "missing":
        package = prefix / "lib/node_modules/t3/package.json"
        package.parent.mkdir(parents=True)
        package.write_text(json.dumps({
            "name": "t3", "version": "0.0.38",
            "_resolved": "https://registry.npmjs.org/t3/-/t3-0.0.38.tgz",
            "bin": {"t3": "./dist/bin.mjs" if origin == "legacy" else "./bin/t3.js"},
        }))
    if origin == "legacy-npm-ls":
        metadata = json.loads(package.read_text())
        del metadata["_resolved"]
        metadata["bin"] = {"t3": "./dist/bin.mjs"}
        package.write_text(json.dumps(metadata))

    def run(argv, **kwargs):
        if argv[:2] == ["npm", "ls"]:
            return {"ok": True, "output": json.dumps({"dependencies": {"t3": {
                "version": "0.0.38", "resolved": "https://registry.npmjs.org/t3/-/t3-0.0.38.tgz",
            }}})}
        return {"ok": True, "output": str(prefix)}

    ns["run"] = run
    ns["refuse_controlled_t3"]({"version": "0.0.38"})
    if origin != "missing":
        ns["verify_pair_binary"] = lambda *a: None
        ns["COMPONENTS"] = {"t3-steward": {"path": ".local/bin/t3-steward"}}
        assert ns["apply_t3"](
            {"version": "0.0.38", "service": "t3code.service"},
            {"installed": "0.0.38", "served": "0.0.38", "service_active": True}, [],
        ) == "converged"
