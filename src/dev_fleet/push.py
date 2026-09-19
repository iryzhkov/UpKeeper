"""`upkeeper push`: capture this machine as the release and publish it.

Publishing means committing `desired/release.json` plus the stored agent files
to this repository and pushing the current branch to the named remotes. Targets
never read the remote themselves; the controller carries the manifest to them
with `upkeeper pull`. Publishing exists so the release is durable, reviewable
and recoverable from another controller.
"""

from __future__ import annotations

from collections.abc import Sequence
from pathlib import Path
from typing import Any, TextIO

from .release import (
    AGENT_CONFIG_DIR,
    RELEASE_FILE,
    Runner,
    capture_release,
    default_runner,
    describe,
    write_release,
)

RELEASE_PATHS = ("--", RELEASE_FILE, AGENT_CONFIG_DIR)


class PushError(RuntimeError):
    """The release could not be committed or pushed."""


def _git(runner: Runner, root: Path, *arguments: str, timeout: int = 60) -> str:
    completed = runner(["git", "-C", str(root), *arguments], None, timeout)
    if completed.returncode != 0:
        raise PushError(f"git {' '.join(arguments)} failed: {completed.stderr.strip()[-400:]}")
    return completed.stdout.strip()


def configured_remotes(root: Path, runner: Runner = default_runner) -> list[str]:
    return [line for line in _git(runner, root, "remote").splitlines() if line]


def push_release(
    root: Path,
    *,
    controller: str,
    remotes: list[str],
    commit: bool = True,
    push: bool = True,
    home: Path | None = None,
    runner: Runner = default_runner,
    components: Sequence[str] | None = None,
    publish_diverged_instructions: bool = False,
    notice: TextIO | None = None,
) -> dict[str, Any]:
    # The resident-instruction gate lives inside capture_release, so it runs
    # before the candidate manifest is written and before any Git index is
    # touched: a refusal leaves no partial manifest, no commit to amend and no
    # branch behind. A manifest-only run is gated too, because writing the
    # candidate manifest is already a write.
    release = capture_release(
        root,
        controller=controller,
        home=home,
        runner=runner,
        only=components,
        publish_diverged_instructions=publish_diverged_instructions,
        notice=notice,
    )
    write_release(root, release)
    report: dict[str, Any] = {
        "schema_version": 1,
        "command": "push",
        "controller": controller,
        "release_sha256": release.sha256(),
        "summary": describe(release),
        "committed": None,
        "pushed": [],
    }
    if not commit:
        return report
    known = set(configured_remotes(root, runner))
    unknown = [remote for remote in remotes if remote not in known]
    if unknown:
        raise PushError(f"unknown git remote(s): {', '.join(unknown)}")
    _git(runner, root, "add", *RELEASE_PATHS)
    staged = _git(runner, root, "diff", "--cached", "--name-only", *RELEASE_PATHS)
    if staged:
        message = (
            f"Capture the fleet release from {controller}\n\n"
            + "\n".join(f"- {line}" for line in describe(release))
            + "\n"
        )
        _git(runner, root, "commit", "--quiet", "--only", "-m", message, *RELEASE_PATHS)
        report["committed"] = _git(runner, root, "rev-parse", "HEAD")
    else:
        report["committed"] = None
    report["head"] = _git(runner, root, "rev-parse", "HEAD")
    if not push:
        return report
    branch = _git(runner, root, "rev-parse", "--abbrev-ref", "HEAD")
    if branch == "HEAD":
        raise PushError("cannot push a detached HEAD; check out a branch first")
    for remote in remotes:
        _git(runner, root, "push", "--quiet", remote, f"HEAD:refs/heads/{branch}", timeout=120)
        report["pushed"].append(f"{remote} {branch}")
    return report
