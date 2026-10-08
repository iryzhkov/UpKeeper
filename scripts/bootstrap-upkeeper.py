#!/usr/bin/env python3
"""Local-only, operator-pinned standalone UpKeeper installer (Python 3.11+, Linux)."""

from __future__ import annotations

import argparse
import contextlib
import fcntl
import hashlib
import json
import os
import re
import selectors
import signal
import stat
import struct
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path

REPOSITORY = "github.com/iryzhkov/UpKeeper"
NAMESPACE = ".upkeeper-bootstrap"
OWNER = b"upkeeper-standalone-bootstrap-v1\n"
TRANSACTION = b"upkeeper-standalone-stage-v1\n"
MAX_RECEIPT = 64 * 1024
MAX_ARTIFACT = 256 * 1024 * 1024
HEX64 = re.compile(r"[0-9a-f]{64}\Z")
HEX40 = re.compile(r"[0-9a-f]{40}\Z")
TAG = re.compile(r"v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.+-]+)?\Z")
PRIVATE = re.compile(r"(?:\.stage-|\.activate-|\.upkeeper-bootstrap-init-)[0-9a-f]{32}\Z")
NOFOLLOW = os.O_NOFOLLOW | os.O_CLOEXEC
FILES = {"transaction", "upkeeper", "approval.json", "committed.json"}


class Refusal(ValueError):
    """Input or owned state fails the v1 boundary."""


def require(condition, message):
    if not condition:
        raise Refusal(message)


def boundary(name):
    """In-process fault-injection seam; no production flag or environment override."""


def digest(data):
    return hashlib.sha256(data).hexdigest()


def pairs(items):
    result = {}
    for key, value in items:
        require(key not in result, "duplicate JSON key")
        result[key] = value
    return result


def parse_json(raw):
    try:
        return json.loads(raw.decode("utf-8"), object_pairs_hook=pairs)
    except (UnicodeError, json.JSONDecodeError, RecursionError) as exc:
        raise Refusal("invalid UTF-8 JSON") from exc


def fields(obj, names):
    require(type(obj) is dict and set(obj) == set(names.split()), "unknown/missing JSON fields")


def positive(value, cap=None):
    require(type(value) is int and value > 0 and (cap is None or value <= cap),
            "invalid positive integer")


def approval(raw, trust):
    require(type(trust) is str and HEX64.fullmatch(trust), "invalid trust SHA256")
    require(0 < len(raw) <= MAX_RECEIPT, "approval size limit")
    require(digest(raw) == trust, "approval trust digest mismatch")
    obj = parse_json(raw)
    fields(obj, "schema_version repository source_commit release_tag platform artifact_sha256 "
           "artifact_size expected_version_stdout ci")
    require(type(obj["schema_version"]) is int and obj["schema_version"] == 1, "schema version")
    require(obj["repository"] == REPOSITORY, "repository identity")
    require(type(obj["source_commit"]) is str and HEX40.fullmatch(obj["source_commit"]),
            "source commit")
    require(type(obj["release_tag"]) is str and TAG.fullmatch(obj["release_tag"]), "release tag")
    require(obj["platform"] in ("debian13-amd64", "arch-amd64"), "receipt platform")
    require(type(obj["artifact_sha256"]) is str and HEX64.fullmatch(obj["artifact_sha256"]),
            "artifact SHA256")
    positive(obj["artifact_size"], MAX_ARTIFACT)
    output = obj["expected_version_stdout"]
    require(type(output) is str and 0 < len(output.encode("utf-8")) <= 4096,
            "version stdout limit")
    require(output == f"upkeeper {obj['release_tag']} (commit {obj['source_commit']})\n",
            "tag/source/version association")
    ci = obj["ci"]
    fields(ci, "evidence_mode workflow_path workflow_name repository head_sha run_id "
           "status conclusion jobs")
    require(ci["evidence_mode"] == "operator-attested", "CI evidence mode")
    require(ci["workflow_path"] == ".github/workflows/ci.yml"
            and ci["workflow_name"] == "phase-1-gate", "CI workflow identity")
    require(ci["repository"] == REPOSITORY and ci["head_sha"] == obj["source_commit"],
            "CI source identity")
    positive(ci["run_id"])
    require(ci["status"] == "completed" and ci["conclusion"] == "success", "CI outcome")
    fields(ci["jobs"], "go-gate gate")
    for job in ci["jobs"].values():
        fields(job, "status conclusion")
        require(job["status"] == "completed" and job["conclusion"] == "success", "CI job outcome")
    return obj


def host_platform():
    # Internal test seam only: CLI and environment offer no platform override.
    require(sys.platform == "linux" and os.uname().machine in ("x86_64", "amd64"),
            "unsupported OS/architecture")
    info = {}
    with open("/etc/os-release", encoding="utf-8") as stream:
        for line in stream:
            key, sep, value = line.strip().partition("=")
            if sep:
                require(key not in info, "duplicate os-release key")
                info[key] = value.strip('"').strip("'")
    if info.get("ID") == "debian" and info.get("VERSION_ID") == "13":
        return "debian13-amd64"
    require(info.get("ID") in ("arch", "omarchy"), "unsupported distribution")
    return "arch-amd64"


def canonical(path):
    require(isinstance(path, (str, Path)), "invalid path")
    text = str(path)
    require(text.startswith("/") and text == os.path.normpath(text)
            and "//" not in text, "path must be canonical absolute")
    current = Path("/")
    for part in Path(text).parts[1:]:
        current /= part
        require(not stat.S_ISLNK(current.lstat().st_mode), "symlink path refused")
    return Path(text)


def safe_prefix(path):
    # Writable ancestors could redirect ownership writes. Sticky shared parents permit
    # only the owning UID/root to rename our entry, which is the explicit threat limit.
    for parent in (path, *path.parents):
        info = parent.lstat()
        require(info.st_uid in (0, os.geteuid()), "foreign-owned prefix traversal")
        require(stat.S_ISDIR(info.st_mode), "prefix traversal is not a directory")
        shared_write = info.st_mode & 0o022
        require(not shared_write or (parent != path and info.st_mode & stat.S_ISVTX),
                "unsafe writable prefix traversal")
    require(path.stat().st_uid == os.geteuid(), "prefix must belong to installer UID")


def directory(path, owned=False):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW)
    info = os.fstat(fd)
    try:
        if owned:
            require(info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) in (0o700, 0o500),
                    "unsafe owned directory")
    except BaseException:
        os.close(fd)
        raise
    return fd


def regular(path):
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | NOFOLLOW)
    try:
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1, "not a private regular file")
    except BaseException:
        os.close(fd)
        raise
    return fd


def snapshot(info):
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def read_bytes(path, cap):
    fd = regular(path)
    try:
        before = snapshot(os.fstat(fd))
        chunks = []
        total = 0
        while chunk := os.read(fd, min(65536, cap + 1 - total)):
            chunks.append(chunk)
            total += len(chunk)
            require(total <= cap, "file size limit")
        require(before == snapshot(os.fstat(fd)), "file changed while reading")
        return b"".join(chunks)
    finally:
        os.close(fd)


def fsync_dir(path, name):
    fd = directory(path)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    boundary(name)


def write_file(path, data, name, mode=0o400):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | NOFOLLOW, mode)
    try:
        with os.fdopen(fd, "wb", closefd=False) as stream:
            stream.write(data)
            stream.flush()
            os.fsync(fd)
        boundary(name)
    finally:
        os.close(fd)


def generation_id(trust, artifact):
    return digest(f"upkeeper-standalone-generation-v1\n{trust}\n{artifact}\n".encode())


def state_bytes(obj, trust):
    return json.dumps({
        "schema_version": 1, "state": "committed",
        "generation_id": generation_id(trust, obj["artifact_sha256"]),
        "approval_sha256": trust, "artifact_sha256": obj["artifact_sha256"],
        "artifact_size": obj["artifact_size"],
    }, sort_keys=True, separators=(",", ":")).encode() + b"\n"


def verify_binary(path, obj):
    fd = regular(path)
    try:
        info = os.fstat(fd)
        require(info.st_size == obj["artifact_size"], "artifact size mismatch")
        hasher = hashlib.sha256()
        while chunk := os.read(fd, 65536):
            hasher.update(chunk)
        require(snapshot(info) == snapshot(os.fstat(fd)), "binary changed while verifying")
        require(hasher.hexdigest() == obj["artifact_sha256"], "artifact hash mismatch")
        return snapshot(info)
    finally:
        os.close(fd)


def static_amd64(path):
    # This structural check rejects scripts/dynamic ELF. Go provenance remains an operator claim.
    fd = regular(path)
    try:
        header = os.read(fd, 64)
        require(len(header) == 64 and header[:7] == b"\x7fELF\x02\x01\x01", "ELF64 required")
        kind, machine = struct.unpack_from("<HH", header, 16)
        require(kind == 2 and machine == 62, "static Linux amd64 executable required")
        offset = struct.unpack_from("<Q", header, 32)[0]
        size, count = struct.unpack_from("<HH", header, 54)
        require(size == 56 and 0 < count <= 128, "invalid ELF program table")
        os.lseek(fd, offset, os.SEEK_SET)
        table = os.read(fd, size * count)
        require(len(table) == size * count, "truncated ELF program table")
        require(all(struct.unpack_from("<I", table, i * size)[0] != 3 for i in range(count)),
                "dynamic ELF interpreter refused")
    finally:
        os.close(fd)


def probe(path, expected, timeout=5):
    with tempfile.TemporaryDirectory(prefix="upkeeper-version-") as home:
        env = {"HOME": home, "XDG_CONFIG_HOME": home + "/config",
               "XDG_DATA_HOME": home + "/data", "XDG_STATE_HOME": home + "/state",
               "XDG_CACHE_HOME": home + "/cache", "PATH": "", "LANG": "C", "LC_ALL": "C"}
        process = subprocess.Popen([str(path), "version"], env=env, cwd=home,
                                   stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.DEVNULL, start_new_session=True)
        result = bytearray()
        if process.stdout is None:
            raise Refusal("version probe pipe unavailable")
        try:
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout.fileno(), selectors.EVENT_READ)
                deadline = time.monotonic() + timeout
                while selector.get_map():
                    remaining = deadline - time.monotonic()
                    require(remaining > 0, "version probe timeout")
                    for key, _ in selector.select(min(remaining, 0.1)):
                        chunk = os.read(key.fd, 4097)
                        if not chunk:
                            selector.unregister(key.fileobj)
                        else:
                            result.extend(chunk)
                            require(len(result) <= 4096, "version probe stdout limit")
                remaining = deadline - time.monotonic()
                require(remaining > 0, "version probe timeout")
                try:
                    code = process.wait(timeout=remaining)
                except subprocess.TimeoutExpired as exc:
                    raise Refusal("version probe timeout") from exc
                require(code == 0 and bytes(result) == expected.encode(), "version output mismatch")
        finally:
            # Cleans ordinary descendants too, even if the leader already exited.
            with contextlib.suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            process.stdout.close()


def validate_generation(path, expected_id=None):
    fd = directory(path, owned=True)
    os.close(fd)
    if HEX64.fullmatch(path.name):
        require(stat.S_IMODE(path.stat().st_mode) == 0o500, "generation directory permissions")
    require(set(os.listdir(path)) == FILES, "unknown/incomplete generation")
    require(read_bytes(path / "transaction", 128) == TRANSACTION, "transaction marker")
    raw = read_bytes(path / "approval.json", MAX_RECEIPT)
    state = parse_json(read_bytes(path / "committed.json", 4096))
    fields(state, "schema_version state generation_id approval_sha256 artifact_sha256 "
           "artifact_size")
    obj = approval(raw, state["approval_sha256"])
    require(state_bytes(obj, state["approval_sha256"]) == read_bytes(path / "committed.json", 4096),
            "committed metadata mismatch")
    gid = generation_id(state["approval_sha256"], obj["artifact_sha256"])
    require(expected_id is None or gid == expected_id, "generation identity mismatch")
    verify_binary(path / "upkeeper", obj)
    static_amd64(path / "upkeeper")
    require(stat.S_IMODE((path / "upkeeper").stat().st_mode) == 0o500, "binary permissions")
    for name in FILES:
        require((path / name).stat().st_uid == os.geteuid(), "generation file owner")
        if name != "upkeeper":
            require(stat.S_IMODE((path / name).stat().st_mode) == 0o400,
                    "metadata permissions")
    return obj, state


def namespace_valid(ns):
    fd = directory(ns, owned=True)
    os.close(fd)
    require(stat.S_IMODE(ns.stat().st_mode) == 0o700, "namespace permissions")
    require(read_bytes(ns / "owner", 128) == OWNER, "unowned namespace")
    require(stat.S_IMODE((ns / "owner").stat().st_mode) == 0o400, "owner permissions")
    fd = directory(ns / "generations", owned=True)
    os.close(fd)
    require(stat.S_IMODE((ns / "generations").stat().st_mode) == 0o700,
            "generations permissions")
    lock = regular(ns / "lock")
    try:
        info = os.fstat(lock)
        require(info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) == 0o600
                and info.st_size == 0, "unsafe lock")
    finally:
        os.close(lock)
    for name in os.listdir(ns):
        require(name in {"owner", "generations", "lock", "current"}
                or re.fullmatch(r"\.activate-[0-9a-f]{32}", name), "unknown namespace entry")
        if name.startswith(".activate-"):
            require((ns / name).is_symlink()
                    and re.fullmatch(r"generations/[0-9a-f]{64}", os.readlink(ns / name)),
                    "invalid pending selector")
            target = os.readlink(ns / name)
            validate_generation(ns / target, target.split("/")[1])


def active(ns):
    current = ns / "current"
    if not os.path.lexists(current):
        return None
    require(current.is_symlink(), "current must be owned selector")
    target = os.readlink(current)
    require(re.fullmatch(r"generations/[0-9a-f]{64}", target), "unowned current target")
    obj, state = validate_generation(ns / target, target.split("/")[1])
    return {"state": "installed", "generation_id": state["generation_id"],
            "source_commit": obj["source_commit"], "release_tag": obj["release_tag"],
            "artifact_sha256": obj["artifact_sha256"], "artifact_size": obj["artifact_size"],
            "approval_sha256": state["approval_sha256"], "approval_mode": "operator-approved-pin",
            "ci_evidence": "operator-attested/pinned", "platform": obj["platform"],
            "executable": str(current / "upkeeper")}


def pending_entries(ns):
    pending = []
    for name in os.listdir(ns / "generations"):
        path = ns / "generations" / name
        if re.fullmatch(r"\.stage-[0-9a-f]{32}", name):
            validate_stage(path)
            pending.append(path)
        else:
            require(HEX64.fullmatch(name), "unknown generations entry")
            validate_generation(path, name)
    return pending


def validate_stage(path):
    # No recursion through attacker-controlled paths; only this protocol's flat private staging.
    fd = directory(path, owned=True)
    os.close(fd)
    names = set(os.listdir(path))
    require(names <= FILES, "unknown staging entry")
    if "transaction" in names:
        require(read_bytes(path / "transaction", 128) == TRANSACTION, "unowned staging")
    else:
        require(not names, "missing staging ownership")
    for name in names:
        fd = regular(path / name)
        os.close(fd)
    return names


def discard_stage(path):
    names = validate_stage(path)
    os.chmod(path, 0o700)
    for name in sorted(names - {"transaction"}):
        (path / name).unlink()
        boundary("recovery-unlink")
    if "transaction" in names:
        (path / "transaction").unlink()
        boundary("recovery-marker")
    path.rmdir()
    boundary("recovery-rmdir")


def validate_init(path):
    fd = directory(path, owned=True)
    os.close(fd)
    names = set(os.listdir(path))
    require(names <= {"owner", "lock", "generations"}, "unknown namespace preparation")
    if "owner" in names:
        require(read_bytes(path / "owner", 128) == OWNER, "unowned namespace preparation")
    else:
        require(not names, "missing preparation marker")
    if "lock" in names:
        require(read_bytes(path / "lock", 1) == b"", "invalid prepared lock")
    if "generations" in names:
        fd = directory(path / "generations", owned=True)
        os.close(fd)
        require(not os.listdir(path / "generations"), "nonempty namespace preparation")
    return names


def namespace_preparations(prefix):
    result = []
    for path in prefix.iterdir():
        if re.fullmatch(r"\.upkeeper-bootstrap-init-[0-9a-f]{32}", path.name):
            validate_init(path)
            result.append(path)
    return result


def cleanup_init(prefix):
    for path in namespace_preparations(prefix):
        names = validate_init(path)
        if "lock" in names:
            (path / "lock").unlink()
        if "generations" in names:
            (path / "generations").rmdir()
        if "owner" in names:
            (path / "owner").unlink()
        path.rmdir()
        boundary("recovery-init")
        fsync_dir(prefix, "recovery-prefix-fsync")


def create_namespace(prefix):
    ns = prefix / NAMESPACE
    if os.path.lexists(ns):
        namespace_valid(ns)
        # A previous process may have crashed between namespace rename and PREFIX fsync.
        fsync_dir(prefix, "existing-prefix-fsync")
        return ns
    temp = prefix / (".upkeeper-bootstrap-init-" + uuid.uuid4().hex)
    temp.mkdir(mode=0o700)
    write_file(temp / "owner", OWNER, "owner-file-fsync")
    write_file(temp / "lock", b"", "lock-file-fsync", 0o600)
    (temp / "generations").mkdir(mode=0o700)
    fsync_dir(temp / "generations", "initial-generations-fsync")
    fsync_dir(temp, "initial-namespace-fsync")
    os.rename(temp, ns)
    boundary("namespace-rename")
    fsync_dir(prefix, "prefix-fsync")
    return ns


def recover(ns):
    namespace_valid(ns)
    active(ns)  # Never repair a corrupt current by reinstalling over it.
    stages = pending_entries(ns)
    for path in stages:
        discard_stage(path)
        fsync_dir(ns / "generations", "recovery-generations-fsync")
    for name in os.listdir(ns):
        if name.startswith(".activate-"):
            (ns / name).unlink()
            boundary("recovery-selector-unlink")
            fsync_dir(ns, "recovery-namespace-fsync")


def copy_artifact(source, target, obj):
    fd = regular(source)
    try:
        initial = os.fstat(fd)
        require(initial.st_size == obj["artifact_size"], "input size mismatch")
        out = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | NOFOLLOW, 0o500)
        try:
            with os.fdopen(out, "wb", closefd=False) as stream:
                total = 0
                copied = hashlib.sha256()
                while chunk := os.read(fd, 65536):
                    total += len(chunk)
                    copied.update(chunk)
                    require(total <= obj["artifact_size"], "input grew while copying")
                    stream.write(chunk)
                    boundary("copy-chunk")
                stream.flush()
                os.fsync(out)
                boundary("binary-file-fsync")
            # Timestamp granularity can hide a same-size in-place rewrite.
            # Rebind the source bytes before any staged binary is executed.
            os.lseek(fd, 0, os.SEEK_SET)
            current = hashlib.sha256()
            observed = 0
            while chunk := os.read(fd, 65536):
                observed += len(chunk)
                require(observed <= obj["artifact_size"], "source changed while copying")
                current.update(chunk)
            require(observed == obj["artifact_size"]
                    and current.hexdigest() == copied.hexdigest()
                    and snapshot(initial) == snapshot(os.fstat(fd))
                    and snapshot(initial) == snapshot(source.lstat()),
                    "source changed while copying")
        finally:
            os.close(out)
    finally:
        os.close(fd)
    return verify_binary(target, obj)


def install(prefix, artifact, receipt, trust):
    # All caller arguments, approval and platform validated before the first write.
    prefix, artifact, receipt = canonical(prefix), canonical(artifact), canonical(receipt)
    safe_prefix(prefix)
    prefix_fd = directory(prefix)
    try:
        raw = read_bytes(receipt, MAX_RECEIPT)
        obj = approval(raw, trust)
        require(obj["platform"] == host_platform(), "host/approval platform mismatch")
        input_fd = regular(artifact)
        try:
            require(os.fstat(input_fd).st_size == obj["artifact_size"], "input size mismatch")
        finally:
            os.close(input_fd)
        fcntl.flock(prefix_fd, fcntl.LOCK_EX)
        cleanup_init(prefix)
        ns = create_namespace(prefix)
        lock = regular(ns / "lock")
        try:
            fcntl.flock(lock, fcntl.LOCK_EX)
            recover(ns)
            gid = generation_id(trust, obj["artifact_sha256"])
            final = ns / "generations" / gid
            stage = ns / "generations" / (".stage-" + uuid.uuid4().hex)
            stage.mkdir(mode=0o700)
            write_file(stage / "transaction", TRANSACTION, "transaction-file-fsync")
            fsync_dir(stage, "transaction-directory-fsync")
            before = copy_artifact(artifact, stage / "upkeeper", obj)
            write_file(stage / "approval.json", raw, "approval-file-fsync")
            static_amd64(stage / "upkeeper")
            probe(stage / "upkeeper", obj["expected_version_stdout"])
            boundary("post-probe")
            require(before == verify_binary(stage / "upkeeper", obj), "staged object substituted")
            require(read_bytes(stage / "approval.json", MAX_RECEIPT) == raw,
                    "staged approval changed")
            write_file(stage / "committed.json", state_bytes(obj, trust), "committed-file-fsync")
            validate_generation(stage, gid)
            os.chmod(stage, 0o500)
            fsync_dir(stage, "generation-directory-fsync")
            boundary("pre-generation-rename")
            require(before == verify_binary(stage / "upkeeper", obj), "staged object substituted")
            validate_generation(stage, gid)
            if os.path.lexists(final):
                validate_generation(final, gid)
                discard_stage(stage)
            else:
                os.rename(stage, final)
                boundary("generation-rename")
            fsync_dir(ns / "generations", "generations-fsync")
            selector = ns / (".activate-" + uuid.uuid4().hex)
            os.symlink("generations/" + gid, selector)
            boundary("selector-created")
            os.replace(selector, ns / "current")
            boundary("current-replace")
            fsync_dir(ns, "namespace-fsync")
            result = active(ns)
            if result is None or result["generation_id"] != gid:
                raise Refusal("activation inspection mismatch")
            return result
        finally:
            os.close(lock)
    finally:
        os.close(prefix_fd)


def inspect(prefix):
    prefix = canonical(prefix)
    fd = directory(prefix)
    result = {
        "state": "absent", "compatibility": "provisional; actual distro qualification pending",
        "prerequisites": {"installer": "Python 3.11+ stdlib/Linux flock",
                          "binary_version_help": "no interpreter/Omarchy required",
                          "production_adapters": "unassessed; see docs/go-binary.md"},
        "durability": "requires tested Linux filesystem rename/fsync semantics",
        "enforcement": "same-UID/root mutation and sealed descendants unqualified",
    }
    try:
        fcntl.flock(fd, fcntl.LOCK_SH)
        ns = prefix / NAMESPACE
        prepared = bool(namespace_preparations(prefix))
        if not os.path.lexists(ns):
            if prepared:
                result["state"] = "pending"
            return result
        namespace_valid(ns)
        lock = regular(ns / "lock")
        try:
            fcntl.flock(lock, fcntl.LOCK_SH)
            installed = active(ns)
            pending = pending_entries(ns)
            pending += [p for p in ns.iterdir() if p.name.startswith(".activate-")]
            if installed:
                result.update(installed)
            elif pending or os.listdir(ns / "generations"):
                result["state"] = "pending"
            result["pending"] = bool(pending or prepared)
        finally:
            os.close(lock)
        return result
    finally:
        os.close(fd)


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Local-only operator-pinned UpKeeper binary installation. "
                    "Compatibility provisional; no service/network/package actions.")
    commands = parser.add_subparsers(dest="command", required=True)
    add = commands.add_parser("install", help="install, owned upgrade or approved rollback")
    for name in ("prefix", "artifact", "approval", "trust-sha256"):
        add.add_argument("--" + name, required=True)
    show = commands.add_parser("inspect", help="offline read-only byte/receipt inspection")
    show.add_argument("--prefix", required=True)
    show.add_argument("--json", action="store_true")
    args = parser.parse_args(argv)
    try:
        require(sys.version_info >= (3, 11), "Python 3.11+ required")
        require(sys.platform == "linux", "Linux required")
        if args.command == "install":
            result = install(args.prefix, args.artifact, args.approval, args.trust_sha256)
            result["compatibility"] = "provisional; actual distro qualification pending"
        else:
            result = inspect(args.prefix)
        print(json.dumps(result, sort_keys=True))
        return 0
    except (OSError, Refusal, TypeError, KeyError, OverflowError, UnicodeError) as exc:
        print(json.dumps({"state": "invalid", "error": str(exc)}, sort_keys=True), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
