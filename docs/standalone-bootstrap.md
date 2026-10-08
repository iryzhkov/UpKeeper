# Standalone local binary bootstrap v1

This separate, explicitly invoked installer provisions only a static Linux amd64
UpKeeper Go binary. Debian 13 amd64, Arch amd64 and Omarchy amd64 compatibility is
**provisional**: exact `ID=omarchy` maps to the existing `arch-amd64` receipt;
`ID_LIKE=arch` alone does not admit other distributions.
Injected platform fixtures are not actual distro execution certificates. It does
not require Omarchy, inventory, desired state, a managed home, credentials, an SSH
agent, Git, Go, jq or third-party Python modules. The operator must provide Python
3.11+ standard library on Linux (including flock), an existing real absolute
prefix directory, writable private storage, and a filesystem with working atomic
rename and file/directory fsync. Missing prerequisites refuse; the installer never
installs packages, uses sudo, contacts services/network, or changes PATH.

The installed static Go executable's version/help needs no Python interpreter.
Production adapters have separate Git/OpenSSH/systemd and component requirements
documented in [go-binary.md](go-binary.md). They are unassessed by bootstrap inspection.
Doctor still observes Python/Ansible and can fail without them. Installation is
neither adapter readiness nor full S1 acceptance.

## Invocation and operator preparation

The operator obtains the canonical release artifact `upkeeper-linux-amd64`
separately; the existing release workflow also publishes arm64 (unsupported here)
and SHA256SUMS. Checksums adjacent to a download cannot authorize it. Verify source,
tag, artifact and exact-source CI by your independent approval process, then prepare
a local receipt and supply its trusted **raw-byte** SHA256 separately:

```console
python3 scripts/bootstrap-upkeeper.py install --prefix /absolute/existing/prefix \
  --artifact /absolute/upkeeper-linux-amd64 --approval /absolute/approval.json \
  --trust-sha256 LOWERCASE_HEX64
python3 scripts/bootstrap-upkeeper.py inspect --prefix /absolute/existing/prefix --json
python3 scripts/bootstrap-upkeeper.py --help
```

All paths must be canonical absolute paths with no symlink traversal. Prefix
ancestors must belong to the installer UID or root and prohibit shared writes
(except sticky shared ancestors); the prefix must belong to the installer UID. Prefix and
ancestors are never created. No default HOME prefix is selected. Upgrade and binary
rollback use the same command with a separately approved artifact and receipt.
Retained generations are validated, never silently trusted as a rollback cache.

The only exposed executable is
`PREFIX/.upkeeper-bootstrap/current/upkeeper`. No bin symlink, wrapper, global config,
service, runtime grant, shared instruction or installed owner pin is written.
Unrelated prefix files are preserved. Existing CLI, Python wrappers, source
selection, module27/setup/install and scheduled paths are unchanged; none
automatically invokes this installer.

## Strict approval schema

Receipt is at most 64 KiB of exact UTF-8 JSON bytes. Its SHA256 must match
`--trust-sha256` before parsing or artifact execution. JSON duplicate keys at every
object level, unknown/missing fields, incompatible versions/types, and oversized
inputs refuse. Field spelling and accepted values are:

| Field | Requirement |
| --- | --- |
| schema_version | integer 1; booleans refused |
| repository | github.com/iryzhkov/UpKeeper |
| source_commit | 40 lowercase hex characters |
| release_tag | exact versioned tag, optional v prefix, three numeric components and optional prerelease/build suffix |
| platform | debian13-amd64 or arch-amd64 (exact Arch or Omarchy ID); must match actual local OS/uname |
| artifact_sha256 | 64 lowercase hex characters |
| artifact_size | positive integer, at most 256 MiB |
| expected_version_stdout | exact stdout, at most 4 KiB |
| ci | exact object below |

Actual output is `upkeeper TAG (commit SOURCE_COMMIT)\n`, including the newline;
TAG is the exact embedded version, not a stripped tag. A source build can report
`0.0.0-dev`, and that is not a release certificate. Receipt output must agree with
its tag and source and match the staged executable's fixed `version` invocation.

`ci` has exactly `evidence_mode: "operator-attested"`,
`workflow_path: ".github/workflows/ci.yml"`, `workflow_name: "phase-1-gate"`,
`repository: "github.com/iryzhkov/UpKeeper"`, `head_sha` equal to source_commit,
positive integer `run_id`, `status: "completed"`, `conclusion: "success"`, and
`jobs` with exactly `go-gate` and `gate`. Each job has exactly
`status: "completed"` and `conclusion: "success"`.

Operator-approved pin is an **approval label**, not signing or cryptographic build
attestation. Exact-source CI records, tag/source association and static Go build
origin are operator-approved claims; offline inspection labels them
operator-attested/pinned. No current live CI is fetched. A binary's version output
is a consistency check, not independent authenticity. Main CI does not certify an
unrelated tagged release. No actual approval receipt is supplied by this change.
Tests use synthetic receipts and synthetic nonexecuted ELF data; a separate test
uses current compiled Go bytes with a synthetic receipt, never a release claim.

## Ownership, staging and durability

The reserved namespace has fixed `owner` bytes
`upkeeper-standalone-bootstrap-v1\n`, a private `lock`, `generations`, and
one authoritative relative `current` symlink. Generation ID is SHA256 of
`upkeeper-standalone-generation-v1\nAPPROVAL_SHA256\nARTIFACT_SHA256\n`.
Each immutable generation contains `upkeeper` (0500), exact `approval.json` (0400),
`transaction` (0400, fixed `upkeeper-standalone-stage-v1\n`), and
`committed.json` (0400). The directory is 0500. Metadata is not independently
updated at namespace level.

Committed JSON binds integer schema_version 1, state committed, generation_id,
approval_sha256, artifact_sha256 and artifact_size. The state serialization is
sorted compact JSON with a trailing newline. Inspection rehashes approval and
binary, checks exact metadata and generation identity, and never executes the
program or arbitrary PATH tools. Current cannot escape owned generations.
Unknown/corrupt state, unowned targets, symlinks, unsafe permissions and active
receipt/byte tampering refuse instead of being adopted or overwritten.

Install locks the existing prefix directory and owned namespace lock through
recovery, private copy, verification, probe, commit and switch. Private namespace
creation fsyncs owner, lock, generations and namespace, renames into PREFIX, then
fsyncs PREFIX. Only then does it use committed ownership. Copy uses a no-follow
file descriptor into a private 0700 stage, checks size/digest and source descriptor
and pathname stat before/after; changing the input cannot replace staged bytes.
ELF checks require static ELF64 x86-64 without PT_INTERP. This structural check
does not independently prove Go build provenance.

Only hash-approved staged bytes are probed. Probe uses fixed version argv,
disposable HOME/XDG, empty PATH, no credentials/SSH agent, 5-second deadline,
4-KiB stdout cap, and process-group cleanup. Staged inode/hash and approval are
rechecked after probe and immediately before generation publication.

Durability order: transaction marker/file and directory fsync; binary and approval
file fsync; complete verification; committed-state file fsync; generation directory
fsync; stage-to-generation rename; generations directory fsync; temporary relative
selector creation and atomic replace of current; namespace directory fsync.
Existing namespace ownership is revalidated and PREFIX is fsynced on rerun to
recover an interrupted namespace publication before any further ownership writes.
Success follows the final fsync and matching active-byte inspection. Old generations
remain; no automatic pruning occurs.

Before switch an interrupted upgrade retains the old valid current; after switch
it selects the new complete generation. First install can report absent/pending.
Rerun validates current first, removes only recognized flat private staging or
namespace preparations, and retries the separately approved request. Recovery is
itself interruptible and idempotent. Orphan complete generations stay inactive.
Unknown staging contents refuse and remain for operator investigation. Disk,
permission, rename or fsync errors report failure; a switch followed by failed
fsync can leave a valid new current, and must be inspected before retry.

Owner markers/modes are a format and incidental-write boundary. Arbitrary same-UID
or root mutation, malicious approved code escaping its process group, sealed
descendant enforcement, and unsupported filesystem durability are explicitly
unqualified. This is no sandbox or workload authentication certificate.

## Verification and unchanged S1 limits

Focused fixtures exercise both platform labels, schema/trust/version/CI rejection,
input mutation, stage substitution, symlink/unowned/tampered state, every durability
boundary, interrupted recovery, disk failures and concurrent fresh/upgrade/rollback
writers. Fault injection is an in-process Python test seam only; no production
flag or environment bypass exists. Real current-Go integration follows
`make go-cross`; installer syntax/ruff/full pytest and owning `make go-gate`,
locked dependency setup, pinned collections, `make gate` and required YAML lint
remain mandatory.

Actual Debian/Arch/Omarchy execution has not been established by fixtures. Source
publication compatibility is structural: the candidate adds only this standalone
script, tests and documentation; all old source-following code and configuration
remain byte-identical. This does not close legacy writer fencing/cutover,
shared ownership activation, catalog distribution/token budgets, plugin
reconciliation, sealed/runtime authorization, production rollback qualification,
or any fleet release/deployment gate.
