# Generated Steward user-unit migration

The pinned Steward adapter migrates the old generated user unit's exact
`ProtectSystem=full` setting to `ProtectSystem=false`, retaining
`NoNewPrivileges=true`. User-service mount namespaces can remap root-owned
OpenSSH configuration to an unmapped UID, causing SSH to refuse the configuration.

This runs for a changed binary pin and for an otherwise converged pin. It does
not run Steward's force-regeneration command. Recognition requires the complete
known generated template, allowing only its generated ExecStart, PATH and paired
After/PartOf values. Unknown customization in a unit requiring migration is
refused. An already compatible custom unit is left untouched, including the
coordinator wrapper used on Normandy. Symlinks, external ancestors, non-owner
files, non-0644 unit modes and files over 64 KiB are refused.

Before mutation, the exact original is retained privately at
`~/.local/state/dev-fleet/unit-migrations/steward-protectsystem.pending`.
An interrupted activation retries only if the current unit is one of the two
known transaction images. The adapter writes atomically, reloads systemd, restarts
Steward and requires both an active unit and effective ProtectSystem=no/false.
A drop-in overriding the base unit therefore cannot produce false success.
Unknown drop-ins are never changed.

Effective-unit verification precedes binary replacement. The existing pinned
binary installation and compensation remain in use. On failure the migration
restores the original unit, reloads and restarts it, retaining pending evidence.
Successful activation retains a private `steward-unit-prior-*` backup and removes
the pending marker. A later historical release convergence retains the compatible
unit; the setting corrects the service environment independently of model/catalog
or binary authorization. Explicit operator restoration can use the retained exact
unit bytes if the migration itself must be undone.

Qualification: `TestStewardOwnedUnitMigration` covers same-pin repair, binary
upgrade, dry-run, repeat convergence, compatible customization, unknown legacy
customization, symlink and size refusal, interrupted activation, concurrent drift,
restart failure and an effective setting shadowed by a drop-in. Disposable process
runners simulate systemd; actual user-namespace/OpenSSH failure was reproduced
separately by the live gate. This test does not itself prove host convergence.
