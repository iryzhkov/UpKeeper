#!/usr/bin/env python3
"""Compare both release binaries from independent clean clones and caches."""

import hashlib
import os
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
BINARIES = ("upkeeper-linux-amd64", "upkeeper-linux-arm64")


def command(*args, cwd=ROOT, env=None) -> str:
    return subprocess.check_output(args, cwd=cwd, env=env, text=True).strip()


def main() -> None:
    # Untracked handoff/log artifacts are allowed; all source bytes must be committed.
    if command("git", "status", "--porcelain", "--untracked-files=no"):
        raise SystemExit("reproducibility requires a clean committed tracked tree")
    commit = command("git", "rev-parse", "HEAD")
    tags = command("git", "tag", "--points-at", commit).splitlines()
    with tempfile.TemporaryDirectory(prefix="upkeeper-repro-") as directory:
        digests = []
        for index in (1, 2):
            checkout = Path(directory) / f"checkout-{index}"
            subprocess.run(["git", "clone", "--quiet", "--no-local", str(ROOT), str(checkout)],
                           check=True)
            subprocess.run(["git", "checkout", "--quiet", "--detach", commit],
                           cwd=checkout, check=True)
            # A tag on HEAD must be identical in both clones for the version ldflag.
            if command("git", "tag", "--points-at", commit, cwd=checkout).splitlines() != tags:
                raise SystemExit("clone did not preserve the release tags")
            env = os.environ.copy()
            env["GOCACHE"] = str(Path(directory) / f"cache-{index}")
            env.pop("GOFLAGS", None)
            subprocess.run(["make", "go-cross"], cwd=checkout, env=env, check=True)
            digests.append({
                name: hashlib.sha256((checkout / "bin" / name).read_bytes()).hexdigest()
                for name in BINARIES
            })
        if digests[0] != digests[1]:
            raise SystemExit(f"reproducibility mismatch: {digests}")
        for name, digest in digests[0].items():
            print(f"{digest}  {name}")
        print(f"Reproducible: {commit}; two clean clones, two empty Go build caches")


if __name__ == "__main__":
    main()
