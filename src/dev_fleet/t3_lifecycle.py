"""T3 service ownership and one-way retirement of the legacy updater."""
from __future__ import annotations

from pathlib import Path

UPDATER_UNITS = ("t3-update.timer", "t3-update.service")
UNIT = """# Seeded by UpKeeper; yours to edit.
[Unit]
Description=T3 Code server (agent control surface)
Documentation=https://github.com/pingdotgg/t3code
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%h
ExecStart=%h/.local/share/mise/shims/t3 serve --host 127.0.0.1 --port 7391 --no-browser
Environment=PATH=%h/.local/share/mise/shims:%h/.local/bin:/usr/local/bin:/usr/bin:/bin
Environment=HOME=%h
EnvironmentFile=-%h/.config/claude/oauth.env
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
"""


def unit_status(run, name):
    result = run(["systemctl", "--user", "show", name,
                  "--property=LoadState,ActiveState,UnitFileState,FragmentPath"], timeout=15)
    fields = dict(line.split("=", 1) for line in result["output"].splitlines() if "=" in line)
    # systemctl exits nonzero for an absent unit but still reports not-found.
    if fields.get("LoadState") == "not-found":
        return fields
    if not result["ok"] or not fields.get("LoadState") or not fields.get("ActiveState"):
        raise RuntimeError("cannot observe user unit " + name)
    return fields


def retire_updater(home: Path, run, must, dry_run: bool):
    """Retirement is irreversible release policy, even during a historical pull."""
    directory = home / ".config/systemd/user"
    states = {name: unit_status(run, name) for name in UPDATER_UNITS}
    paths = [directory / name for name in UPDATER_UNITS]
    present = any(p.exists() or p.is_symlink() for p in paths) or any(
        s.get("LoadState") != "not-found" for s in states.values()
    )
    if not present:
        return {"state": "converged", "actions": []}
    for name, state in states.items():
        fragment = state.get("FragmentPath")
        if fragment and Path(fragment) != directory / name:
            raise RuntimeError("legacy updater has an unexpected unit path: " + name)
    if dry_run:
        return {"state": "updated", "actions": ["would disable and delete legacy updater units"]}
    # Stop new activations first. Never kill an in-flight package installation.
    timer = states["t3-update.timer"]
    if timer.get("LoadState") != "not-found":
        must(["systemctl", "--user", "disable", "--now", "t3-update.timer"],
             what="disable legacy updater timer")
    service = unit_status(run, "t3-update.service")
    if service.get("ActiveState") not in {None, "inactive", "failed"}:
        return {"state": "deferred", "actions": [
            "legacy timer disabled; updater service still running, observe before retry"]}
    if service.get("LoadState") != "not-found":
        must(["systemctl", "--user", "disable", "t3-update.service"],
             what="disable legacy updater service")
        must(["systemctl", "--user", "stop", "t3-update.service"],
             what="stop legacy updater service")
    for path in paths:
        if path.is_dir():
            raise RuntimeError("legacy updater unit is a directory")
        path.unlink(missing_ok=True)
    must(["systemctl", "--user", "daemon-reload"], what="reload after updater retirement")
    for name in UPDATER_UNITS:
        state = unit_status(run, name)
        if (state.get("LoadState") != "not-found"
                or state.get("ActiveState") not in {None, "inactive", "failed"}):
            raise RuntimeError("legacy updater retirement did not verify: " + name)
    return {"state": "updated", "actions": ["disabled and deleted legacy updater units"]}


def seed_t3_service(home: Path, run, must, dry_run: bool, actions):
    """Seed only an absent unit; preserve all existing host-local service settings."""
    path = home / ".config/systemd/user/t3code.service"
    status = unit_status(run, "t3code.service")
    if path.exists() or path.is_symlink() or status.get("LoadState") != "not-found":
        return False
    if not (home / ".config/claude/oauth.env").is_file():
        actions.append("warning: no ~/.config/claude/oauth.env; "
                       "provider authentication needs host provisioning")
    if dry_run:
        actions.append("would seed t3code.service with loopback binding "
                       "and optional host-local oauth.env")
        return True
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("x", encoding="utf-8") as stream:
        stream.write(UNIT)
    path.chmod(0o644)
    must(["systemctl", "--user", "daemon-reload"], what="reload seeded T3 service")
    must(["systemctl", "--user", "enable", "--now", "t3code.service"],
         what="start seeded T3 service")
    actions.append("seeded t3code.service; future host edits are preserved")
    return True
