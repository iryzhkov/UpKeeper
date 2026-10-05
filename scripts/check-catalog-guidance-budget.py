"""Count actual helper-derived complete bundles with the approved finite encoder.

Invoke /usr/bin/unshare -Urn /usr/bin/python3.14 -I -B -S FILE
FIXTURE_ROOT TOKENIZER_ROOT MANIFEST. This is offline projection qualification,
not a positive production prepare or an installation.
"""
# ruff: noqa: E501
import base64
import importlib.machinery
import importlib.util
import json
import os
import platform
import socket
import sys
import sysconfig
from pathlib import Path


def load_helper():
    path = Path(__file__).absolute().parents[1] / "shared/catalog-guidance/config/bin/agents-tool-project"
    loader = importlib.machinery.SourceFileLoader("catalog_guidance", str(path))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


def qualify(helper, root, manifest_path):
    """Validate all finite bytes before importing any approved encoder code."""
    with helper.Held() as held:
        raw = held.read(manifest_path, limit=262144)
        helper.require(helper.H(raw) == helper.TOKENIZER_PIN, code=5)
        manifest = json.loads(raw)
        interpreter = manifest["interpreter"]
        helper.require(sys.flags.isolated and sys.flags.no_site and sys.dont_write_bytecode, code=5)
        helper.require(sys.implementation.name == "cpython" and sys.version == interpreter["version"]
                       and platform.machine() == interpreter["machine"]
                       and list(platform.libc_ver()) == interpreter["libc"]
                       and sysconfig.get_config_var("SOABI") == interpreter["soabi"], code=5)
        executable = Path(sys.executable).resolve()
        data = held.read(executable, limit=16777216)
        helper.require(len(data) == interpreter["bytes"] and helper.H(data) == interpreter["sha256"], code=5)
        for library in interpreter["runtime_shared_libraries"]:
            data = held.read(library["path"], limit=33554432, runtime=True)
            helper.require(len(data) == library["bytes"] and helper.H(data) == library["sha256"], code=5)
        modules = Path(root) / "modules"
        held.open(modules, directory=True, mode=0o700)
        observed_files, observed_dirs = set(), set()
        for current, directories, files in os.walk(modules, followlinks=False):
            for name in directories:
                path = Path(current) / name
                held.open(path, directory=True, mode=0o755)
                observed_dirs.add(path.relative_to(modules).as_posix())
            for name in files:
                observed_files.add((Path(current) / name).relative_to(modules).as_posix())
        projection = manifest["projection"]
        helper.require(observed_files == {r["path"] for r in projection["files"]}
                       and observed_dirs == {r["path"] for r in projection["directories"]}, code=5)
        for record in projection["files"]:
            data = held.read(modules / record["path"], mode=record["mode"], limit=33554432)
            helper.require(len(data) == record["bytes"] and helper.H(data) == record["sha256"], code=5)
        data = held.read(Path(root) / "downloads/cl100k_base.tiktoken", mode=0o644, limit=33554432)
        helper.require(len(data) == manifest["encoder_data"]["bytes"]
                       and helper.H(data) == manifest["encoder_data"]["sha256"], code=5)
        ranks_list = [(base64.b64decode(token, validate=True), int(rank))
                      for token, rank in (line.split() for line in data.splitlines())]
        ranks = dict(ranks_list)
        helper.require(len(ranks_list) == len(ranks) == 100256
                       and set(ranks.values()) == set(range(100256)), code=5)
        held.check()
        sys.path.insert(0, str(modules))
        import tiktoken
        import tiktoken.load
        import tiktoken.registry
        helper.require(Path(tiktoken.__file__).resolve().is_relative_to(modules), code=5)

        def deny(*_args, **_kwargs):
            raise helper.Refused(5)
        for name in ("read_file", "read_file_cached", "load_tiktoken_bpe",
                     "data_gym_to_mergeable_bpe_ranks"):
            setattr(tiktoken.load, name, deny)
        tiktoken.get_encoding = deny
        tiktoken.registry.get_encoding = deny
        encoder = tiktoken.Encoding(name="cl100k_base", pat_str=manifest["pattern"],
                                   mergeable_ranks=ranks, special_tokens=manifest["special_tokens"])
        held.check()
        return encoder, projection


def measure(helper, fixture, tokenizer, manifest):
    helper.require([name for _, name in socket.if_nameindex()] == ["lo"], code=5)
    isolation = {"namespace": os.readlink("/proc/self/ns/net"), "interfaces": socket.if_nameindex()}
    for family, address in ((socket.AF_INET, ("1.1.1.1", 443)),
                            (socket.AF_INET6, ("2606:4700:4700::1111", 443))):
        with socket.socket(family, socket.SOCK_STREAM) as connection:
            connection.settimeout(1)
            try:
                connection.connect(address)
            except OSError as exc:
                isolation[str(family)] = exc.errno
            else:
                raise helper.Refused(5)
    encoder, projection = qualify(helper, tokenizer, manifest)

    def count(data):
        ids = encoder.encode(data.decode("utf-8"), allowed_special=set(), disallowed_special=())
        helper.require(encoder.decode(ids).encode() == data)
        return len(ids)

    with helper.Held() as held:
        snapshot = helper._snapshot(held, fixture, Path(__file__).absolute().parents[1] / "shared/catalog-guidance")
        outputs, guidance, discovery = helper._complete_projection(snapshot)
        held.check()
    helper.require(len(outputs) == 25)
    families = {
        "codex": [".codex/AGENTS.md"] + sorted(p for p in outputs if p.endswith(".config.toml")),
        "claude": [".claude/omarchy-setup/CLAUDE.md"] + sorted(p for p in outputs if p.startswith(".claude/agents/")),
        "opencode": [".config/agents/AGENTS.md"],
    }
    helper.require([len(p) for p in families.values()] == [6, 6, 1])
    bundles = []
    for family, paths in families.items():
        data = b"\n".join(outputs[name] for name in paths)
        tokens = count(data)
        helper.require(tokens <= 3000, code=6)
        bundles.append({"family": family, "paths": paths, "bytes": len(data),
                        "sha256": helper.H(data), "tokens": tokens, "limit": 3000})
    discovery_count = count(discovery)
    helper.require(discovery_count <= 1200, code=6)
    boundaries = []
    for number in (1200, 1201, 3000, 3001):
        text = (" a" * number).encode()
        ids = encoder.encode(text.decode(), allowed_special=set(), disallowed_special=())
        helper.require(len(ids) == number and encoder.decode(ids).encode() == text)
        boundaries.append({"tokens": number, "sha256": helper.H(text),
                           "ids_sha256": helper.H(helper.C(ids)),
                           "resident_pass": number <= 3000, "discovery_pass": number <= 1200})
    parity = []
    for name in helper.CLOSURE["instruction_paths"]:
        if name != ".config/agents/operational-guidance.md":
            parity.append({"path": name, "unchanged": outputs[name] == snapshot["baseline/" + name],
                           "sha256": helper.H(outputs[name])})
    helper.require(sum(p["unchanged"] for p in parity) == 15)
    models = helper.decode(snapshot["inputs/model-blocks.json"])
    for name, block in models.items():
        helper.require(outputs[name].count(block.encode()) == 1)
    policy_rows = []
    import re
    for name in models:
        policy_rows += [json.loads(row) for row in re.findall(r"ordered policy (\{.*\})", outputs[name].decode())]
    helper.require(len(policy_rows) == 17)
    with helper.Held() as held:
        repository, _, source_pins = helper._source(held)
        skills = []
        for name in helper.CLOSURE["skills_paths"]:
            source = repository / "shared/agent-instructions/config/claude/skills" / name
            data = held.read(source, mode=0o644)
            fixture_data = held.read(Path(fixture) / "skills" / name, mode=0o644)
            helper.require(data == fixture_data)
            skills.append({"path": name, "sha256": helper.H(data)})
        held.check()
    return {
        "schema_version": 1, "status": "FRESH_SOURCE_PROJECTION_BUDGET_PASS_REVIEW_PENDING",
        "stage_b": "NOT_MET", "production_prepare": "REQUIRED_OWNER_AND_FIXTURE_BLOCKED",
        "consumer": "internal source projection only; no genuine Go author/store or installation",
        "token_usage": "unavailable", "source_sha256": source_pins,
        "helper_sha256": source_pins["shared/catalog-guidance/config/bin/agents-tool-project"],
        "tokenizer_manifest_sha256": helper.TOKENIZER_PIN,
        "finite_projection": {key: projection[key] for key in
                              ("inventory_sha256", "mapping_sha256", "normalized_tree_sha256")},
        "bundles": bundles, "discovery": {"tokens": discovery_count, "limit": 1200,
                                         "bytes": len(discovery), "sha256": helper.H(discovery)},
        "outputs": [{"path": name, "bytes": len(data), "sha256": helper.H(data)}
                    for name, data in sorted(outputs.items())],
        "projection_sha256": helper.H(helper.C(guidance)), "boundaries": boundaries,
        "parity": parity, "skills": skills, "ordered_policy_rows": policy_rows,
        "policy_sha256": helper.POLICY_PIN, "isolation": isolation,
        "runtime": sys.version, "argv": sys.argv,
        "method": "whole main plus all five ASCII-sorted natives joined by one LF; no per-file sums",
    }


def main():
    helper = load_helper()
    try:
        helper.require(len(sys.argv) == 4, code=2)
        proof = measure(helper, *sys.argv[1:])
        sys.stdout.buffer.write(helper.C(proof))
        return 0
    except helper.Refused as exc:
        sys.stderr.write(helper.ERRORS[exc.code] + "\n")
        return exc.code
    except (OSError, ValueError, KeyError, UnicodeError):
        sys.stderr.write("tokenizer-refused\n")
        return 5


if __name__ == "__main__":
    sys.exit(main())
