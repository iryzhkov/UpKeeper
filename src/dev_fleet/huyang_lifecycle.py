"""Install a missing pinned Huyang checkout without replacing existing paths."""
from __future__ import annotations

import shutil
import tempfile
from pathlib import Path


def bootstrap_huyang(home, spec, run_id, dry_run, env, must, probe, actions):
    checkout = home / spec["path"]
    unit = home / ".config/systemd/user" / spec["service"]
    if checkout.exists() or checkout.is_symlink():
        raise ValueError(f"{checkout} exists but is not a Git checkout; preserving it")
    if unit.exists() or unit.is_symlink():
        raise ValueError("Huyang checkout is absent but a user unit exists; inspect it first")
    for tool in ("git", "go", "make"):
        if shutil.which(tool, path=env.get("PATH")) is None:
            raise ValueError(f"Huyang build prerequisite missing: {tool}")
    if dry_run:
        actions.append(f"would clone {spec['source']} at {spec['commit']}, build and enable Huyang")
        return "updated"
    checkout.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=".huyang-bootstrap-", dir=checkout.parent))
    candidate = stage / "checkout"
    # Failed build artifacts stay private for observation; no live path changes yet.
    must(["git", "clone", "--quiet", "--no-checkout", spec["source"], str(candidate)],
         timeout=180, what="Huyang clone")
    must(["git", "-C", str(candidate), "fetch", "--quiet", "origin", spec["ref"]],
         timeout=180, what="Huyang fetch")
    must(["git", "-C", str(candidate), "merge-base", "--is-ancestor",
          spec["commit"], "FETCH_HEAD"], timeout=30, what="Huyang pin provenance")
    must(["git", "-C", str(candidate), "checkout", "--quiet", "--detach", spec["commit"]],
         timeout=60, what="Huyang pinned checkout")
    must(list(spec.get("build", ["make", "build"])), cwd=candidate,
         timeout=600, what="Huyang build")
    # mkdir is an exclusive claim; do not replace even an empty pre-existing directory.
    checkout.mkdir()
    candidate.rename(checkout)
    stage.rmdir()
    unit.parent.mkdir(parents=True, exist_ok=True)
    content = (
        # No After=default.target: the unit is wanted by default.target, and that
        # ordering would put every unit ordered after it in a cycle at boot.
        "[Unit]\nDescription=Huyang semantic workspace service\n\n"
        "[Service]\nType=simple\n"
        f"WorkingDirectory=%h/{spec['path']}\n"
        f"ExecStart=%h/{spec['path']}/bin/huyang serve\n"
        "Restart=on-failure\nRestartSec=2\n\n[Install]\nWantedBy=default.target\n"
    )
    created_unit = False
    try:
        with unit.open("x") as handle:
            handle.write(content)
        created_unit = True
        unit.chmod(0o644)
        must(["systemctl", "--user", "daemon-reload"], what="Huyang unit reload")
        must(["systemctl", "--user", "enable", "--now", spec["service"]],
             timeout=60, what="Huyang service start")
        result = probe(checkout, spec["probe"]["tools"])
        if not result["ok"]:
            raise ValueError(f"Huyang bootstrap probe failed: {result}")
    except Exception:
        # Keep candidate bytes and unit for diagnosis. The scheduler's host hold prevents retries.
        if created_unit:
            must(["systemctl", "--user", "disable", "--now", spec["service"]],
                 timeout=60, what="stop failed Huyang bootstrap")
        raise
    actions.append(f"installed Huyang {spec['commit'][:12]}; enabled {spec['service']}")
    return "updated"
