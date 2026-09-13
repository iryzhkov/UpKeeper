import hashlib
import io
import tarfile

import pytest
from test_release_u0 import remote_namespace


def archive(body, symlink=False):
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w") as tar:
        member = tarfile.TarInfo("packaging/opencode/steward-session.js")
        member.size = len(body)
        if symlink:
            member.type = tarfile.SYMTYPE
            member.linkname = "/tmp/elsewhere"
        tar.addfile(member, io.BytesIO(body))
    stream.seek(0)
    return tarfile.open(fileobj=stream)


def test_hook_hash_regular_file_and_repair_detection(tmp_path):
    ns = remote_namespace(tmp_path)
    body = b"export const Hook = async () => ({});"
    spec = {"session_hook_sha256": hashlib.sha256(body).hexdigest()}
    assert not ns["session_hook_converged"](spec)
    with archive(body) as tar:
        candidate = ns["stage_session_hook"](tar, tmp_path, spec)
    assert candidate.read_bytes() == body
    target = ns["steward_session_hook"](spec)
    target.parent.mkdir(parents=True)
    target.write_bytes(body)
    assert ns["session_hook_converged"](spec)
    target.write_bytes(b"drift")
    assert not ns["session_hook_converged"](spec)
    target.unlink()
    target.symlink_to(candidate)
    assert not ns["session_hook_converged"](spec)
    for tar in (archive(b"wrong"), archive(body, symlink=True)):
        with tar, pytest.raises(ns["StepFailed"]):
            ns["stage_session_hook"](tar, tmp_path, spec)
    assert ns["session_hook_converged"]({})
    assert ns["stage_session_hook"](None, tmp_path, {}) is None


def test_current_binary_missing_hook_requires_repair_without_dry_run_writes(tmp_path):
    ns = remote_namespace(tmp_path)
    ns["DRY_RUN"] = True
    ns["verify_pair_binary"] = lambda _: ("0.0.38", "0.0.38")
    spec = {"path": ".local/bin/t3-steward", "version": "0.11.0-rc.11",
            "session_hook_sha256": "a" * 64}
    actions = []
    assert ns["apply_pinned_steward"](spec, {"version": spec["version"],
                                            "service_active": True}, actions) == "updated"
    assert not ns["steward_session_hook"](spec).exists()
    assert actions
