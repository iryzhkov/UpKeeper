"""Read immutable release intent and companion files without moving the checkout."""
from __future__ import annotations

import hashlib
import json
import re
import subprocess
from pathlib import Path

from .release import AGENT_CONFIG_DIR, RELEASE_FILE, Release, validate_release
from .validation import ValidationError


def load_historical_release(root: Path, revision: str) -> tuple[Release, str, dict[str, str]]:
    if not re.fullmatch(r"[0-9a-fA-F]{7,40}", revision):
        raise ValidationError("--to must be a release commit hash (7 to 40 hex characters)")

    def git(*arguments: str) -> bytes:
        result = subprocess.run(
            ["git", "-C", str(root), *arguments],
            capture_output=True, check=False, timeout=30,
        )
        if result.returncode:
            raise ValidationError("historical release or companion file is unavailable")
        return result.stdout

    commit = git("rev-parse", "--verify", revision + "^{commit}").decode().strip()
    git("merge-base", "--is-ancestor", commit, "HEAD")
    try:
        manifest = json.loads(git("show", commit + ":" + RELEASE_FILE))
    except (UnicodeError, ValueError) as exc:
        raise ValidationError("historical release is not valid JSON") from exc
    if not isinstance(manifest, dict):
        raise ValidationError("historical release must be an object")
    release = validate_release(manifest)
    stored = {}
    for entry in release.components["agent-config"]["files"]:
        data = git("show", commit + ":" + AGENT_CONFIG_DIR + "/" + entry["store"])
        if hashlib.sha256(data).hexdigest() != entry["sha256"]:
            raise ValidationError("historical companion file digest mismatch: " + entry["store"])
        try:
            stored[entry["store"]] = data.decode("utf-8")
        except UnicodeError as exc:
            raise ValidationError("historical companion file is not UTF-8") from exc
    return release, commit, stored
