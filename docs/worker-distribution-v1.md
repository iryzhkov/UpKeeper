# Worker distribution contract v1 (U0 → S4)

UpKeeper distributes bootstrap configuration through
`desired/release.json.components.worker-configuration`. It does not enroll hosts,
create coordinator catalog projections, rotate worker epochs or restart workers.
The component contains exactly `kind: worker-configuration` and `hosts`.
An empty hosts map performs no worker effects.

S4 may add this entry after its implementation and enrollment checks are ready:

```json
{
  "kind": "worker-configuration",
  "hosts": {
    "homelab": {
      "schema_version": 1,
      "worker_id": "homelab",
      "coordinator_id": "normandy",
      "transport": "ssh",
      "capabilities": ["git", "huyang"],
      "provider_routes": [],
      "credential_ref": "secretref:f02-protocol/homelab"
    }
  }
}
```

For omarchy-pc use key/worker_id `omarchy-pc` and
`secretref:f02-protocol/omarchy-pc`. Host and worker identity must agree.
Capabilities and provider-route identifiers are sorted and unique. Only routes
whose credentials work on that host belong here; configuration is not evidence of
availability. Inventory supplies the SSH address/user and verified host identity.
S4's authenticated runtime protocol supplies the effective catalog.

`upkeeper pull --hosts homelab --components worker-configuration` validates the
component and writes mode 0600
`~/.config/t3-steward/worker-bootstrap.json` on homelab. The document is exactly
that host's entry. Its digest is reported. A host absent from the map is left alone;
omission does not unenroll it or delete existing runtime configuration.
The previous bootstrap is retained privately under
`~/.local/state/dev-fleet/worker-config/<run-id>/prior.json`.

The opaque reference resolves on the target to
`~/.config/upkeeper/secrets/f02-protocol/<host>`. It must be an existing,
nonempty, regular, current-user-owned 0600 file, contained in that store and not a
symlink. Provisioning that secret value remains the existing host secret authority's
job. UpKeeper distributes the reference, validates availability without reading the
value, and does not copy the value from normandy. Missing or unsafe references fail
before bootstrap activation, including during dry run.

S4 can use `resolve_secret_reference(home, reference)` in
`src/dev_fleet/worker_release.py` as the exact resolver contract, then give that
path to its authenticated worker runtime. Runtime code must open the value only
for protocol authentication and keep it out of catalogs, logs, diagnostics and
artifacts. Unknown bootstrap fields, inline credentials and catalog copies are
rejected by `validate_workers`.

S4 owns consuming this document, validating actual capabilities/routes, persistent
SSH transport, authenticated enrollment and worker startup/reload. U0 only
establishes the distribution interface. No homelab or omarchy-pc enrollment is
performed by U0; the release's worker host set must remain empty until S4.

`push` preserves explicitly reviewed worker entries from the existing manifest;
it does not infer remote enrollment from normandy's coordinator configuration.
Its automatic capture covers the T3 pair and agent environment. Do not put the
duplicated historical coordinator/worker YAML catalogs into this component.
