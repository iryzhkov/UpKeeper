# Jocasta client distribution

The optional `jocasta` component installs a versioned CLI from the private
`iryzhkov/jocasta` GitHub release. The release pins its source commit and SHA-256
for both `linux_amd64` and `linux_arm64` raw binary assets. Targets use their own
existing `gh` authentication; no GitHub credentials are distributed.

Each explicitly configured host gets `~/.local/bin/jocasta` and an owner-only
`~/.config/jocasta/config.json` containing an HTTPS URL and a local token-file path.
The manifest carries only `secretref:jocasta/<inventory-host>`; secret provisioning
must create `~/.config/upkeeper/secrets/jocasta/<inventory-host>` with mode 0600.
The adapter checks secret metadata without reading or hashing the credential.

Capture validates the installed CLI version, source commit, and binary bytes
against published release checksums. It preserves the authored endpoint and host
references. Select `--components jocasta` for a component-scoped capture/pull.
Older manifests without Jocasta remain valid.

Installation verifies artifact digest and version before activation, retains
private previous bytes, verifies the installed files, and compensates synchronous
failures. Unexpected concurrent destination changes stop automatic recovery.
Server deployment, data retention, and backups are owned by homelab-infra.
