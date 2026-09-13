#!/usr/bin/env python3
"""U2 acceptance: remove two shared files, dry-run --self, restore exact originals."""
from __future__ import annotations

import hashlib
import json
import os
import socket
import subprocess
import sys
import tempfile
from pathlib import Path

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[1]
PATHS = (".config/agents/huyang.md", ".config/agents/ov-memory.md")


def main() -> int:
    if socket.gethostname() != "omarchy-pc":
        raise RuntimeError("this acceptance canary is restricted to omarchy-pc")
    home = Path.home()
    files = [home / path for path in PATHS]
    if any(p.is_symlink() or not p.is_file() for p in files):
        raise RuntimeError("canary inputs must be existing regular shared instruction files")
    state = home / ".local/state/dev-fleet"
    state.mkdir(parents=True, exist_ok=True)
    backup = Path(tempfile.mkdtemp(prefix="u2-self-canary-", dir=state))
    backup.chmod(0o700)
    identities = {p.name: (hashlib.sha256(p.read_bytes()).hexdigest(), p.stat().st_mode)
                  for p in files}
    moved = []
    try:
        for path in files:
            os.replace(path, backup / path.name)
            moved.append(path)
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts/upkeeper"), "pull", "--self",
             "--dry-run", "--json"], capture_output=True, text=True, check=False, timeout=300,
            env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"},
        )
        if result.returncode != 0:
            raise RuntimeError("self dry run failed: " + result.stdout + result.stderr)
        report = json.loads(result.stdout)
        if len(report["hosts"]) != 1 or report["hosts"][0]["transport"] != "local":
            raise RuntimeError("self did not select exactly one local host")
        environment = next(c for c in report["hosts"][0]["components"]
                           if c["name"] == "agent-environment")
        if environment["state"] != "updated" or any(p.exists() for p in files):
            raise RuntimeError("dry run did not detect missing files without writing")
        actions = environment["actions"]
        if any(not any(path in action for action in actions) for path in PATHS):
            raise RuntimeError("dry run did not plan both removed instruction files")
        (backup / "dry-run.json").write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps({"run_id": report["run_id"], "self": report["self"],
                          "release_commit": report["desired_manifest_commit"],
                          "missing_detected": list(PATHS), "no_activation": True,
                          "evidence": str(backup / "dry-run.json")}))
    finally:
        conflicts = []
        for path in moved:
            if path.exists() or path.is_symlink():
                conflicts.append(path.name)
                continue
            os.replace(backup / path.name, path)
        if conflicts:
            raise RuntimeError("concurrent edits preserved; originals remain at " + str(backup))
        for path in moved:
            digest, mode = identities[path.name]
            if (hashlib.sha256(path.read_bytes()).hexdigest() != digest
                    or path.stat().st_mode != mode):
                raise RuntimeError("canary restoration did not verify")
    print("original bytes and modes restored")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
