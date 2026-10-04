# Offline candidate tool catalog

`upkeeper environment tools --metadata ABS --root ABS --expect-sha256 HEX64 [--platform linux-amd64|linux-arm64] [--json]`

This Linux-only inspection verifies an explicitly operator-pinned metadata document
and every referenced regular file under an explicit physical directory. It executes
nothing, calls no fleet configuration resolver, contacts no service, and writes no
files or telemetry. The reserved environment namespace bypasses the invocation
spool for success, refusal, invalid arguments and help. Existing commands, including
root help and version, keep their existing telemetry policy.

Only exact environment help and tools help forms are accepted; help mixed with
options fails. Each option occurs once; equals forms, abbreviations and positional
tails fail before input access. Paths must be absolute and the SHA256 pin must be
64 lowercase hex characters. Help is available without input or configuration.

The trust mode is **operator-sha256-pin**. Approval covers declarations and content;
it is neither a signature nor independent build/CI provenance. No metadata producer
or multi-tool installer is supplied. The standalone binary installer receipt does
not authenticate this catalog. The fixtures in internal/toolcatalog/testdata are
synthetic declarations, never release or generation approval.

## Strict v1 declarations

All listed fields are mandatory, with no extra fields or recursive duplicate keys.
One UTF-8 JSON object is allowed, with no trailing values. Nulls and coercions are
rejected. Integers use JSON integer syntax. Metadata is at most 256 KiB, nesting
at most 12, and arrays at most 1024 items (sources 128, tools 256).
Strings cannot contain control characters. IDs use
`[a-z][a-z0-9-]{0,63}`; descriptions are at most 512 UTF-8 bytes.

| Object | Exact fields |
| --- | --- |
| root | schema_version (integer 1), state (candidate-inert), sources, artifacts, tools |
| source | id, repository, revision_kind, revision |
| artifact | id, source_id, path, sha256, mode, required |
| tool | id, canonical_tool, aliases, kind, purpose, capabilities, primary_route, artifact_ids, required, platforms |
| primary_route | kind, name |

Sources declare canonical HTTPS repositories with host/path and no credentials,
query or fragment. revision_kind is git-sha1 (40 lowercase hex) or content-sha256
(64 lowercase hex). No source is fetched. Artifact IDs and paths are unique.
Paths are normalized slash-separated relative paths, at most 512 bytes with
segments at most 255 bytes: no empty, dot, dotdot or backslash segments.
Artifact sha256 is lowercase hex64; mode is an integer 0–511 of permission bits;
required is boolean. References must resolve; orphan artifacts fail.

Tools have globally unambiguous ID/canonical/alias ownership; one owner's ID may
equal its canonical identity. Aliases are unique, including within their owner.
kind is cli, mcp-only or third-party; route kind is cli, mcp or external with an
ID name. CLI and MCP-only require their corresponding route kind. Each primary
(kind,name) route is unique. purpose is nonempty. capabilities and artifact_ids
are nonempty unique ID lists. platforms is a nonempty unique subset of
linux-amd64/linux-arm64. Required tools reference only required artifacts.
repository-operations requires canonical_tool huyang. agent99 is rejected in tool
IDs, canonical identities, aliases and route names. This bounded blacklist makes
no whole-host or unknown-alias retirement claim; editor plugin ownership remains
unchanged and full editor reconciliation remains open.

Capabilities and routes are arbitrary declarations, not proof of ability,
endpoint readiness or authorization. Empty arrays at the root are valid and
claim nothing. No selection/filter options exist: all referenced content is checked.
Extra undeclared files are neither scanned nor approved.

## Descriptor safety and observation limits

Every candidate path component is acquired with Linux O_PATH|O_NOFOLLOW and
checked for its required type before proceeding. Nonregular final components
(including FIFOs, sockets, devices and symlinks) are rejected before data open.
The verifier holds the regular inode, then obtains a read descriptor through
its own fd number in a held, validated procfs /proc/self/fd directory, using
O_RDONLY|O_NONBLOCK|O_CLOEXEC. Procfs absent/invalid refuses; there is no caller
pathname fallback. Descriptor identity is checked before reads.

Artifacts are hashed as streams: at most 256 MiB per file and 1 GiB aggregate
actual artifact reads, plus bounded metadata reads. Permission bits and
device/inode/type/size/mtime/ctime are checked, with final path binding against
the held inode. Metadata is independently bound and pinned again at the end.
Metadata cannot overlap an artifact, including by hardlink. Root identity is
also rechecked. No data, pathname or raw operating-system error is printed.

These are per-file observations, **per-file-no-atomic-snapshot**. Files may change
between observations. This is a trusted-host boundary: malicious same-UID/root
actors and privileged procfs replacement are outside it. It is not sealed
confinement, immutable-generation activation or whole-generation race proof.

## Result contract

Exit 0 means required declared content was verified at observation. Optional
absent content is visible as optional-missing; present optional corruption fails.
Required absence, schema, pin, path, mode, hash and binding failures exit 30.
Usage errors exit 2 before reads. Internal/output failures exit 1. Platform
incompatibility is visible and does not fail content verification.

JSON common fields are schema_version=1, command=environment-tools,
result=content-verified|invalid-input, exit_code, catalog_mode=candidate-inspection,
generation_state=candidate-inert, trust_mode=operator-sha256-pin,
sources_provenance=declaration-only, observation_scope=per-file-no-atomic-snapshot.
generation_sha256 is the digest of the exact bytes, only emitted after pin match.
Failures contain error.code and error.stage only, with no untrusted declarations.
Success contains source declarations, artifact observations and tool declarations.

Each successful tool reports selected=declared, content_state=verified|optional-missing,
installed=unknown, configured=unknown, authorized=unknown, enforcement=unqualified,
readiness=unassessed. Without a requested platform, platform_compatibility is
unassessed. With one, it is compatible/incompatible by declaration comparison
only and requested_platform is included. Text reports carry the same trust states
compactly; approved descriptions still pass through secret redaction.

No readiness probe/timestamp, installed authority, active generation, actual
Debian/Arch execution, platform qualification or runtime authorization is claimed.
An implementation candidate still needs a separate exact-commit review and
source publication gate; producer checks alone are not acceptance.
