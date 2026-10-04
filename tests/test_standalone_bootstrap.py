"""Synthetic protocol fixtures; no release approval or distro qualification is asserted."""

import copy
import errno
import importlib.util
import json
import multiprocessing
import os
import signal
import struct
import subprocess
import sys
import time
from pathlib import Path
from types import SimpleNamespace

import pytest

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("bootstrap", ROOT / "scripts/bootstrap-upkeeper.py")
assert SPEC is not None and SPEC.loader is not None
bootstrap = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bootstrap)
BASE = "da88c9ae868d0dc4a502d908b3039226483b0d3e"


def synthetic_elf(suffix=b"fixture"):
    # Structurally static ELF, deliberately not executable test code.
    header = bytearray(64)
    header[:7] = b"\x7fELF\x02\x01\x01"
    struct.pack_into("<HH", header, 16, 2, 62)
    struct.pack_into("<Q", header, 32, 64)
    struct.pack_into("<HH", header, 54, 56, 1)
    return bytes(header) + bytes(56) + suffix


def receipt(data, platform="debian13-amd64", tag="v1.0.0", source=BASE):
    return {
        "schema_version": 1, "repository": bootstrap.REPOSITORY, "source_commit": source,
        "release_tag": tag, "platform": platform, "artifact_sha256": bootstrap.digest(data),
        "artifact_size": len(data),
        "expected_version_stdout": f"upkeeper {tag} (commit {source})\n",
        "ci": {"evidence_mode": "operator-attested", "workflow_path": ".github/workflows/ci.yml",
               "workflow_name": "phase-1-gate", "repository": bootstrap.REPOSITORY,
               "head_sha": source, "run_id": 123, "status": "completed", "conclusion": "success",
               "jobs": {name: {"status": "completed", "conclusion": "success"}
                        for name in ("go-gate", "gate")}},
    }


def save_receipt(path, obj):
    raw = json.dumps(obj).encode()
    path.write_bytes(raw)
    return bootstrap.digest(raw)


@pytest.fixture
def setup(tmp_path, monkeypatch):
    prefix = tmp_path / "prefix"
    prefix.mkdir()
    artifact = tmp_path / "artifact"
    artifact.write_bytes(synthetic_elf())
    approval = tmp_path / "approval.json"
    obj = receipt(artifact.read_bytes())
    trust = save_receipt(approval, obj)
    monkeypatch.setattr(bootstrap, "host_platform", lambda: "debian13-amd64")
    probes = []

    def fixture_probe(path, expected):
        # Transaction fixtures certify byte binding, not executable/platform semantics.
        probes.append((path.read_bytes(), expected))

    monkeypatch.setattr(bootstrap, "probe", fixture_probe)
    return prefix, artifact, approval, trust, obj, probes


def run(setup):
    return bootstrap.install(*setup[:4])


@pytest.mark.parametrize("platform", ["debian13-amd64", "arch-amd64"])
def test_install_idempotence_upgrade_rollback_preserves_unrelated(setup, monkeypatch, platform):
    prefix, artifact, approval, _, obj, probes = setup
    monkeypatch.setattr(bootstrap, "host_platform", lambda: platform)
    obj["platform"] = platform
    trust = save_receipt(approval, obj)
    args = (prefix, artifact, approval, trust)
    unrelated = prefix / "preferences"
    unrelated.write_text("keep")
    original = bootstrap.install(*args)
    assert bootstrap.install(*args) == original
    assert bootstrap.inspect(prefix)["artifact_sha256"] == obj["artifact_sha256"]
    artifact.write_bytes(synthetic_elf(b"upgrade"))
    upgraded = receipt(artifact.read_bytes(), platform, "v2.0.0")
    trust2 = save_receipt(approval, upgraded)
    newer = bootstrap.install(prefix, artifact, approval, trust2)
    assert newer["generation_id"] != original["generation_id"]
    artifact.write_bytes(synthetic_elf())
    trust = save_receipt(approval, obj)
    assert bootstrap.install(prefix, artifact, approval, trust) == original
    assert len(list((prefix / bootstrap.NAMESPACE / "generations").iterdir())) == 2
    assert unrelated.read_text() == "keep"
    assert len(probes) == 4
    assert set(prefix.iterdir()) == {prefix / bootstrap.NAMESPACE, unrelated}


BAD = [
    ("schema_version", True), ("schema_version", 2), ("repository", "evil/UpKeeper"),
    ("source_commit", "1" * 39), ("source_commit", "A" * 40), ("release_tag", "latest"),
    ("release_tag", "v2.0.0"), ("platform", "linux-amd64"), ("artifact_size", True),
    ("artifact_size", 0), ("artifact_size", 256 * 1024 * 1024 + 1),
    ("artifact_sha256", "G" * 64), ("expected_version_stdout", "x" * 4097),
    ("expected_version_stdout", "upkeeper v1.0.0 (commit " + "0" * 40 + ")\n"),
    ("ci.evidence_mode", "verified"), ("ci.workflow_path", ".github/workflows/release.yml"),
    ("ci.workflow_name", "release"), ("ci.repository", "other/repo"),
    ("ci.head_sha", "0" * 40), ("ci.run_id", True), ("ci.run_id", 0),
    ("ci.status", "queued"), ("ci.conclusion", "failure"),
    ("ci.jobs.go-gate.status", "queued"), ("ci.jobs.gate.conclusion", "skipped"),
    ("ci.jobs", {"other": {"status": "completed", "conclusion": "success"}}),
]


@pytest.mark.parametrize(("field", "value"), BAD)
def test_strict_approval_before_any_write_or_probe(setup, field, value):
    prefix, _, path, _, obj, probes = setup
    changed = copy.deepcopy(obj)
    target = changed
    keys = field.split(".")
    for key in keys[:-1]:
        target = target[key]
    target[keys[-1]] = value
    trust = save_receipt(path, changed)
    with pytest.raises(bootstrap.Refusal):
        bootstrap.install(prefix, setup[1], path, trust)
    assert list(prefix.iterdir()) == []
    assert probes == []


@pytest.mark.parametrize("where", ["root", "ci", "job"])
@pytest.mark.parametrize("kind", ["duplicate", "unknown"])
def test_duplicate_unknown_at_every_identity_level(setup, where, kind):
    obj = copy.deepcopy(setup[4])
    target = {"root": obj, "ci": obj["ci"], "job": obj["ci"]["jobs"]["gate"]}[where]
    target["unexpected"] = 1
    raw = json.dumps(obj).encode()
    if kind == "duplicate":
        raw = raw.replace(b'"unexpected": 1', b'"unexpected": 1, "unexpected": 2')
    with pytest.raises(bootstrap.Refusal):
        bootstrap.approval(raw, bootstrap.digest(raw))


@pytest.mark.parametrize("raw", [b"\xff", b"{", b" " * (65536 + 1), b"[]"])
def test_invalid_receipt_bytes(raw):
    with pytest.raises(bootstrap.Refusal):
        bootstrap.approval(raw, bootstrap.digest(raw))


def test_pin_raw_bytes_and_unapproved_never_executed(setup):
    prefix, artifact, path, trust, _, probes = setup
    path.write_bytes(path.read_bytes() + b" ")
    with pytest.raises(bootstrap.Refusal, match="trust"):
        run(setup)
    assert probes == []
    assert not list(prefix.iterdir())
    path.write_bytes(path.read_bytes()[:-1])
    artifact.write_bytes(b"#!/bin/sh\ntouch /unapproved-must-never-run\n")
    with pytest.raises(bootstrap.Refusal):
        bootstrap.install(prefix, artifact, path, trust)
    assert probes == []


def test_hash_size_static_and_platform_refusals(setup, monkeypatch):
    prefix, artifact, path, _, obj, probes = setup
    artifact.write_bytes(synthetic_elf(b"changed"))
    trust = save_receipt(path, {**obj, "artifact_size": artifact.stat().st_size})
    with pytest.raises(bootstrap.Refusal, match="hash"):
        bootstrap.install(prefix, artifact, path, trust)
    assert probes == []
    artifact.write_bytes(b"x" * 120)
    trust = save_receipt(path, receipt(artifact.read_bytes()))
    with pytest.raises(bootstrap.Refusal, match="ELF"):
        bootstrap.install(prefix, artifact, path, trust)
    assert probes == []
    monkeypatch.setattr(bootstrap, "host_platform", lambda: "arch-amd64")
    with pytest.raises(bootstrap.Refusal, match="platform"):
        bootstrap.install(prefix, artifact, path, trust)


@pytest.mark.parametrize("change", ["inplace", "replace", "symlink"])
def test_input_changes_during_copy_fail_before_execution(setup, monkeypatch, change):
    artifact = setup[1]
    fired = False

    def change_source(name):
        nonlocal fired
        if name == "copy-chunk" and not fired:
            fired = True
            if change == "inplace":
                artifact.write_bytes(synthetic_elf(b"mutated"))
            else:
                artifact.unlink()
                if change == "replace":
                    artifact.write_bytes(synthetic_elf())
                else:
                    artifact.symlink_to(setup[2])

    monkeypatch.setattr(bootstrap, "boundary", change_source)
    with pytest.raises(bootstrap.Refusal, match="source changed"):
        run(setup)
    assert setup[5] == []
    assert bootstrap.inspect(setup[0])["state"] == "pending"


@pytest.mark.parametrize("point", ["post-probe", "pre-generation-rename"])
@pytest.mark.parametrize("change", ["bytes", "inode", "approval"])
def test_staged_substitution_before_commit(setup, monkeypatch, point, change):
    def substitute(name):
        if name != point:
            return
        stage = next((setup[0] / bootstrap.NAMESPACE / "generations").glob(".stage-*"))
        if change == "approval":
            (stage / "approval.json").chmod(0o600)
            (stage / "approval.json").write_bytes(setup[2].read_bytes() + b" ")
        else:
            binary = stage / "upkeeper"
            data = binary.read_bytes()
            if change == "inode":
                # Check substitution detection, not hostile same-UID containment.
                stage.chmod(0o700)
                binary.unlink()
                binary.write_bytes(data)
                binary.chmod(0o500)
                if point == "pre-generation-rename":
                    stage.chmod(0o500)
            else:
                binary.chmod(0o700)
                binary.write_bytes(data[:-1] + b"!")
    monkeypatch.setattr(bootstrap, "boundary", substitute)
    with pytest.raises(bootstrap.Refusal):
        run(setup)
    assert not os.path.lexists(setup[0] / bootstrap.NAMESPACE / "current")


class Crash(BaseException):
    pass


BOUNDARIES = [
    "owner-file-fsync", "lock-file-fsync", "initial-generations-fsync",
    "initial-namespace-fsync", "namespace-rename", "prefix-fsync", "transaction-file-fsync",
    "transaction-directory-fsync", "copy-chunk", "binary-file-fsync", "approval-file-fsync",
    "post-probe", "committed-file-fsync", "generation-directory-fsync", "pre-generation-rename",
    "generation-rename", "generations-fsync", "selector-created", "current-replace",
    "namespace-fsync",
]


@pytest.mark.parametrize("point", BOUNDARIES)
@pytest.mark.parametrize("existing", [False, True])
def test_every_durability_boundary_recovers_truthfully(setup, monkeypatch, point, existing):
    prior = run(setup) if existing else None
    if existing and point in BOUNDARIES[:6]:
        pytest.skip("fresh-namespace-only boundary")
    if existing:
        setup[1].write_bytes(synthetic_elf(b"upgrade"))
        setup = (*setup[:3], save_receipt(setup[2], receipt(setup[1].read_bytes(), tag="v2.0.0")),
                 *setup[4:])
    fired = False

    def crash(name):
        nonlocal fired
        if name == point:
            fired = True
            raise Crash()

    monkeypatch.setattr(bootstrap, "boundary", crash)
    with pytest.raises(Crash):
        run(setup)
    assert fired
    observed = bootstrap.inspect(setup[0])
    if observed["state"] == "installed":
        if point in ("current-replace", "namespace-fsync"):
            assert observed["artifact_sha256"] == bootstrap.digest(setup[1].read_bytes())
        else:
            assert prior is not None
            assert observed["generation_id"] == prior["generation_id"]
    else:
        assert not existing
        assert observed["state"] in ("pending", "absent")
    monkeypatch.setattr(bootstrap, "boundary", lambda name: None)
    installed = run(setup)
    assert bootstrap.inspect(setup[0])["generation_id"] == installed["generation_id"]


@pytest.mark.parametrize("point", ["recovery-unlink", "recovery-marker", "recovery-rmdir",
                                   "recovery-generations-fsync", "recovery-selector-unlink",
                                   "recovery-namespace-fsync", "recovery-init",
                                   "recovery-prefix-fsync"])
def test_recovery_itself_interrupted(setup, monkeypatch, point):
    first = ("selector-created" if "selector" in point or "namespace" in point
             else "lock-file-fsync" if "init" in point or "prefix" in point
             else "approval-file-fsync")

    def crash(name):
        if name == first:
            raise Crash()
    monkeypatch.setattr(bootstrap, "boundary", crash)
    with pytest.raises(Crash):
        run(setup)

    def recover_crash(name):
        if name == point:
            raise Crash()
    monkeypatch.setattr(bootstrap, "boundary", recover_crash)
    with pytest.raises(Crash):
        run(setup)
    monkeypatch.setattr(bootstrap, "boundary", lambda name: None)
    assert run(setup)["state"] == "installed"


@pytest.mark.parametrize("operation", ["fsync", "rename", "replace", "write"])
def test_disk_failure_no_false_success_or_wrong_receipt(setup, monkeypatch, operation):
    original = run(setup)
    setup[1].write_bytes(synthetic_elf(b"upgrade"))
    setup = (*setup[:3], save_receipt(setup[2], receipt(setup[1].read_bytes(), tag="v2.0.0")),
             *setup[4:])
    real = getattr(os, operation)

    def failure(*args, **kwargs):
        raise OSError(errno.ENOSPC, "synthetic disk full")
    with monkeypatch.context() as patch:
        patch.setattr(os, operation, failure)
        if operation == "write":
            # Buffered copying uses fdopen/write, inject the equivalent write-file failure.
            patch.setattr(bootstrap, "write_file", failure)
        with pytest.raises(OSError):
            run(setup)
    assert getattr(os, operation) is real
    assert bootstrap.inspect(setup[0])["generation_id"] == original["generation_id"]
    assert run(setup)["state"] == "installed"


@pytest.mark.parametrize("target", ["prefix", "artifact", "approval", "namespace", "lock",
                                    "generation", "current", "receipt", "binary"])
def test_symlinks_unowned_and_tampered_targets(setup, target):
    prefix, artifact, approval, trust, _, _ = setup
    if target in ("prefix", "artifact", "approval"):
        path = {"prefix": prefix, "artifact": artifact, "approval": approval}[target]
        moved = path.with_name(path.name + "-real")
        path.rename(moved)
        path.symlink_to(moved, target_is_directory=target == "prefix")
        with pytest.raises(bootstrap.Refusal, match="symlink"):
            bootstrap.install(prefix, artifact, approval, trust)
        return
    original = run(setup)
    ns = prefix / bootstrap.NAMESPACE
    generation = ns / "generations" / original["generation_id"]
    if target == "current":
        (ns / "current").unlink()
        (ns / "current").symlink_to("../../unrelated")
    elif target == "receipt":
        (generation / "approval.json").chmod(0o600)
        (generation / "approval.json").write_bytes(approval.read_bytes() + b" ")
    elif target == "binary":
        (generation / "upkeeper").chmod(0o700)
        (generation / "upkeeper").write_bytes(synthetic_elf(b"corrupt"))
    else:
        path = {"namespace": ns, "lock": ns / "lock", "generation": generation}[target]
        moved = path.with_name(path.name + "-real")
        path.rename(moved)
        path.symlink_to(moved, target_is_directory=target != "lock")
    with pytest.raises((bootstrap.Refusal, OSError)):
        bootstrap.inspect(prefix)
    with pytest.raises((bootstrap.Refusal, OSError)):
        run(setup)


def test_unowned_namespace_unknown_stage_missing_prefix_and_traversal(setup):
    prefix = setup[0]
    ns = prefix / bootstrap.NAMESPACE
    ns.mkdir()
    keep = ns / "keep"
    keep.write_text("unowned")
    with pytest.raises((bootstrap.Refusal, OSError)):
        run(setup)
    assert keep.read_text() == "unowned"
    with pytest.raises(OSError):
        bootstrap.install(prefix / "missing", *setup[1:4])
    with pytest.raises(bootstrap.Refusal):
        bootstrap.install(str(prefix) + "/../prefix", *setup[1:4])


def test_unknown_pending_refused_without_deletion(setup):
    run(setup)
    stage = setup[0] / bootstrap.NAMESPACE / "generations" / (".stage-" + "a" * 32)
    stage.mkdir(mode=0o700)
    (stage / "transaction").write_bytes(bootstrap.TRANSACTION)
    (stage / "foreign").write_text("keep")
    with pytest.raises(bootstrap.Refusal):
        run(setup)
    assert (stage / "foreign").read_text() == "keep"


def test_inspect_help_effect_free_no_program_execution(setup, monkeypatch, capsys):
    run(setup)
    before = {str(p): p.lstat().st_mtime_ns for p in setup[0].rglob("*")}

    def forbidden(*args, **kwargs):
        pytest.fail("inspect/help attempted execution")
    monkeypatch.setattr(subprocess, "Popen", forbidden)
    monkeypatch.setattr(bootstrap, "host_platform", forbidden)
    assert bootstrap.main(["inspect", "--prefix", str(setup[0]), "--json"]) == 0
    with pytest.raises(SystemExit) as exit_info:
        bootstrap.main(["--help"])
    assert exit_info.value.code == 0
    assert "provisional" in capsys.readouterr().out
    assert before == {str(p): p.lstat().st_mtime_ns for p in setup[0].rglob("*")}


def test_concurrent_installers_fresh_upgrade_and_rollback(setup):
    # Linux fork inherits synthetic fixture seams; process locks, not simulated mutexes.
    ctx = multiprocessing.get_context("fork")
    for _ in range(3):
        if _ == 1:
            setup[1].write_bytes(synthetic_elf(b"upgrade"))
        elif _ == 2:
            setup[1].write_bytes(synthetic_elf())
        tag = "v2.0.0" if _ == 1 else "v1.0.0"
        setup = (*setup[:3], save_receipt(setup[2], receipt(setup[1].read_bytes(), tag=tag)),
                 *setup[4:])
        processes = [ctx.Process(target=run, args=(setup,)) for i in range(3)]
        for process in processes:
            process.start()
        for process in processes:
            process.join(15)
            assert not process.is_alive()
            assert process.exitcode == 0
        observed = bootstrap.inspect(setup[0])
        assert observed["artifact_sha256"] == bootstrap.digest(setup[1].read_bytes())
        assert not observed["pending"]


@pytest.mark.parametrize("mode", ["timeout", "overflow", "wrong", "nonzero", "descendant"])
def test_actual_probe_limits_and_process_group_cleanup(tmp_path, mode):
    program = tmp_path / "program"
    pid_file = tmp_path / "child"
    body = {
        "timeout": "import time; time.sleep(60)",
        "overflow": "import sys; sys.stdout.write('x'*100000); sys.stdout.flush()",
        "wrong": "print('wrong')", "nonzero": "raise SystemExit(2)",
        "descendant": (
            "import os,time; pid=os.fork(); "
            f"open({str(pid_file)!r},'w').write(str(pid)); time.sleep(60)"
        ),
    }[mode]
    program.write_text(f"#!{sys.executable}\n{body}\n")
    program.chmod(0o700)
    start = time.monotonic()
    with pytest.raises(bootstrap.Refusal):
        bootstrap.probe(program, "expected\n", timeout=0.3)
    assert time.monotonic() - start < 3
    if pid_file.exists():
        pid = int(pid_file.read_text())
        if pid:
            # Zombie is already killed; reaping is the external subreaper's responsibility.
            path = Path(f"/proc/{pid}/stat")
            for _ in range(20):
                if not path.exists() or path.read_text().split()[2] == "Z":
                    break
                time.sleep(0.01)
            else:
                os.kill(pid, signal.SIGKILL)
                pytest.fail("ordinary descendant escaped process-group cleanup")


def test_distinct_approved_requests_race_without_mixed_identity(setup):
    prefix, artifact, path, trust, _, _ = setup
    run(setup)
    other = artifact.with_name("upgrade-artifact")
    other.write_bytes(synthetic_elf(b"concurrent-upgrade"))
    other_approval = path.with_name("upgrade-approval.json")
    other_trust = save_receipt(other_approval, receipt(other.read_bytes(), tag="v2.0.0"))
    ctx = multiprocessing.get_context("fork")
    requests = [(prefix, other, other_approval, other_trust), (prefix, artifact, path, trust)]
    workers = [ctx.Process(target=bootstrap.install, args=args) for args in requests]
    for worker in workers:
        worker.start()
    for worker in workers:
        worker.join(15)
        assert not worker.is_alive() and worker.exitcode == 0
    result = bootstrap.inspect(prefix)
    assert result["artifact_sha256"] in {
        bootstrap.digest(artifact.read_bytes()), bootstrap.digest(other.read_bytes())
    }
    assert len(list((prefix / bootstrap.NAMESPACE / "generations").iterdir())) == 2


def test_final_fsync_error_reports_failure_but_new_current_is_truthful(setup, monkeypatch):
    run(setup)
    setup[1].write_bytes(synthetic_elf(b"upgrade"))
    trust = save_receipt(setup[2], receipt(setup[1].read_bytes(), tag="v2.0.0"))

    def failure(name):
        if name == "namespace-fsync":
            raise OSError(errno.EIO, "injected final durability error")
    monkeypatch.setattr(bootstrap, "boundary", failure)
    with pytest.raises(OSError):
        bootstrap.install(*setup[:3], trust)
    assert bootstrap.inspect(setup[0])["artifact_sha256"] == bootstrap.digest(setup[1].read_bytes())


def test_missing_platform_and_unsafe_prefix_permissions_before_writes(setup, monkeypatch):
    def unavailable():
        raise bootstrap.Refusal("unsupported distribution")
    monkeypatch.setattr(bootstrap, "host_platform", unavailable)
    with pytest.raises(bootstrap.Refusal):
        run(setup)
    assert not list(setup[0].iterdir())
    setup[0].chmod(0o777)
    with pytest.raises(bootstrap.Refusal, match="unsafe writable"):
        run(setup)
    assert not list(setup[0].iterdir())


@pytest.mark.parametrize("point", ["namespace-rename", "prefix-fsync"])
def test_recovered_namespace_fsyncs_prefix_before_use(setup, monkeypatch, point):
    def crash(name):
        if name == point:
            raise Crash()
    monkeypatch.setattr(bootstrap, "boundary", crash)
    with pytest.raises(Crash):
        run(setup)
    observed = []

    def recover_crash(name):
        observed.append(name)
        if name == "existing-prefix-fsync":
            raise Crash()
    monkeypatch.setattr(bootstrap, "boundary", recover_crash)
    with pytest.raises(Crash):
        run(setup)
    assert observed == ["existing-prefix-fsync"]
    assert not os.path.lexists(setup[0] / bootstrap.NAMESPACE / "current")
    monkeypatch.setattr(bootstrap, "boundary", lambda name: None)
    assert run(setup)["state"] == "installed"


@pytest.mark.parametrize("kind", ["symlink", "foreign", "missing-marker", "bad-marker"])
def test_inspect_refuses_unknown_pending_stage_without_mutation(setup, kind):
    run(setup)
    stage = setup[0] / bootstrap.NAMESPACE / "generations" / (".stage-" + "b" * 32)
    if kind == "symlink":
        stage.symlink_to(setup[0], target_is_directory=True)
    else:
        stage.mkdir(mode=0o700)
        if kind == "foreign":
            (stage / "transaction").write_bytes(bootstrap.TRANSACTION)
            (stage / "foreign").write_text("keep")
        elif kind == "missing-marker":
            (stage / "upkeeper").write_bytes(b"unfinished")
        else:
            (stage / "transaction").write_bytes(b"foreign")
    with pytest.raises((bootstrap.Refusal, OSError)):
        bootstrap.inspect(setup[0])
    assert os.path.lexists(stage)
    assert bootstrap.main(["inspect", "--prefix", str(setup[0]), "--json"]) == 1


@pytest.mark.parametrize("kind", ["symlink", "foreign"])
def test_inspect_refuses_unknown_namespace_preparation(setup, kind):
    pending = setup[0] / (".upkeeper-bootstrap-init-" + "c" * 32)
    if kind == "symlink":
        pending.symlink_to(setup[0], target_is_directory=True)
    else:
        pending.mkdir(mode=0o700)
        (pending / "foreign").write_text("keep")
    with pytest.raises((bootstrap.Refusal, OSError)):
        bootstrap.inspect(setup[0])
    assert os.path.lexists(pending)


def test_foreign_uid_ancestor_cannot_redirect_writes(setup, monkeypatch):
    ancestor = setup[0].parent
    original = Path.lstat

    def foreign_owner(path):
        if path == ancestor:
            return SimpleNamespace(st_uid=os.geteuid() + 1, st_mode=0o040755)
        return original(path)
    monkeypatch.setattr(Path, "lstat", foreign_owner)
    with pytest.raises(bootstrap.Refusal, match="foreign-owned"):
        run(setup)
    assert not list(setup[0].iterdir())


def test_real_current_go_version_help_isolation(tmp_path, monkeypatch):
    binary = ROOT / "bin/upkeeper-linux-amd64"
    if not binary.is_file():
        pytest.skip("run make go-cross before actual current-Go integration")
    data = binary.read_bytes()
    # Version identity comes from the built current source, not a manufactured release approval.
    home = tmp_path / "probe-home"
    home.mkdir()
    env = {"HOME": str(home), "XDG_STATE_HOME": str(home / "state"), "PATH": "",
           "XDG_CONFIG_HOME": str(home / "config"), "XDG_DATA_HOME": str(home / "data")}
    version = subprocess.run([str(binary), "version"], env=env, cwd=home,
                             capture_output=True, check=True).stdout.decode()
    match = bootstrap.re.fullmatch(r"upkeeper (.+) \(commit ([0-9a-f]{40})\)\n", version)
    assert match, version
    tag, source = match.groups()
    prefix = tmp_path / "prefix"
    prefix.mkdir()
    approval = tmp_path / "synthetic-approval.json"
    obj = receipt(data, tag=tag, source=source)
    trust = save_receipt(approval, obj)
    monkeypatch.setattr(bootstrap, "host_platform", lambda: "debian13-amd64")
    installed = bootstrap.install(prefix, binary, approval, trust)
    executable = installed["executable"]
    assert subprocess.run([executable, "version"], env=env, cwd=home, capture_output=True,
                          check=True).stdout.decode() == version
    help_result = subprocess.run([executable, "--help"], env=env, cwd=home, capture_output=True,
                                 check=True)
    assert b"version" in help_result.stdout
    assert bootstrap.inspect(prefix)["source_commit"] == source
