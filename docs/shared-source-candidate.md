# Shared instruction source candidate (S1a inert preparation)

UpKeeper's shared/agent-instructions is a pending source authority, copied from
omarchy-setup c6f58c421053690cff9ef1de04bc544cbb1fe720 through Huyang.
Every unrelated skill and guidance clause remains. Only candidate publishing scope
removes agent99 as an agent tool option. The legacy source remains available.

Build and inspect: go run ./cmd/upkeeper-shared-prepare --repository LOCAL_UPKEEPER --commit FULL_COMMIT.
This separate opt-in command reads a local immutable Git object, checks repository,
descriptor and exact inventory identities (canonical origin push URL metadata is checked;
this is trusted source configuration, not cryptographic artifact authentication), generates a tar stream in a private scratch
tree, and returns input source revision plus source/output hashes. It never fetches,
captures, writes desired files, installs or activates. PrepareShared exposes disposable
DerivedSources to scratch callers; Close removes them. Legacy author CLI is unchanged.
owner.json declares the version, exact instruction paths, skill roots and future merges.
It is a source descriptor, never an activation marker or authorization proof.
No active release selects the candidate. Existing environment projection is trusted-host
configuration, not sealed enforcement. Static inventory does not prove effective models.

## SAME-ACTIVATION contract for the next task

One destination lock must cover owner validation, every output/import write and commit
of the generation receipt. Reuse environment projection, its prior-assets rollback and
target manual/timer lock; first reconcile their exact boundaries in the activation task.
The receipt binds the approved release identity, source revision/hashes, owner descriptor,
owned output identities and complete reachable legacy-writer inventory.
No user-editable marker alone authorizes activation.

Before making candidate bytes observable, prove all reachable writers fenced:
Omarchy generator install mode, module 31, module 28 script distribution,
UpKeeper setupManagedFilePairs and setupRunModules, homelab-cli sync's destination
write/import and source-following installers/post-update hooks.
Historical installed/copied generators, sync clients, delayed writes and rollback binaries
must be inventoried too. Unknown or unfenced reachability refuses activation.
A sender-side sync check is insufficient.

Pending, invalid or interrupted transitions refuse writes, with effect-free help,
check and dry-run still available. Old uncut hosts retain legacy behavior.
Rollback restores approved artifact bytes while maintaining fences and current grants;
it must never revive competing writers or agent99 advertisements.
RequireLegacyWriter defines a deliberately unwired rejection interface: legacy succeeds,
every transition state refuses. It does not solve races or authenticate an active owner.

## Blocking acceptance retained

Unimplemented: atomic same-activation fencing at all named writers; destination lock
spanning validation and writes; corrupt/pending committed owner validation; interrupted
activation recovery; rollback without old writers/grants; racing direct/manual/timer
writers; delayed and historical unfenced binaries; fresh install/upgrade/reboot/rollback
no-agent99 proofs. Interface rejection tests are not activation tests.
Independent source-order and mixed old/new compatibility acceptance is required before
publication. Source-following main updates cannot be assumed safe from sequencing alone.

Independent nvim HEAD a8c430ac52f0502034f37d68f5b74594290cd624 already removed
agent99. This is a BLOCKING complete-S1 preservation criterion for the editor owner,
not a passed plugin test or permission to modify nvim/dependencies/theme bridge.

The candidate-only transaction substrate is documented in
[shared-generation-store.md](shared-generation-store.md). It stages pinned generated
instructions and canonical skills into explicit private scratch stores; candidate-current
and rollback select inert bytes only. Existing runtime writers and defaults remain unchanged.
Its scratch lock supplies no destination fence or historical writer proof.

Next S1 tasks retain qualified Debian13/Arch capability tests and the separately reviewed
destination adapter and activation implementation with all blocking tests above. Candidate
source and mixed-version review plus exact source CI remain required before publication.
No full S1 or cutover completion is claimed here.
