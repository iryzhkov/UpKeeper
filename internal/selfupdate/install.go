package selfupdate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
	"github.com/iryzhkov/UpKeeper/internal/pytime"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// Installer changes one home directory's installation.
type Installer struct {
	Layout Layout
	// Client fetches release assets; nil means a client with a bounded timeout.
	Client *http.Client
	// BaseURL replaces https://github.com, so that a test serves the assets.
	BaseURL string
	// Target is the asset label to install; "" means this machine's.
	Target string
	// Now is the clock the journal records; nil means the real one.
	Now func() time.Time
	// Fault, when set, is consulted before every fsync and rename that moves
	// the installation, and an error it returns is treated as that step
	// failing. It exists so that tests can interrupt a switch at each boundary.
	Fault func(step string) error
	// Flush makes the installation's files and directory entries durable; its
	// zero value flushes for real. Every failure of a flush is a refusal: a
	// generation is not named, and current is not switched, on the strength of
	// a flush that did not happen.
	Flush fsatomic.Flusher
}

// Result is what an installation or rollback did.
type Result struct {
	// Generation is the generation current names afterwards.
	Generation string
	// Previous is the generation a rollback would restore afterwards.
	Previous string
	// Path is the binary of Generation, which is what a re-exec runs: never the
	// current link, so that a concurrent switch cannot redirect it.
	Path string
	// Switched is true when current moved.
	Switched bool
	// Adopted names the generation an unmanaged binary was adopted into.
	Adopted string
	// Transaction identifies the switch this call made, or "" when current did
	// not move. UndoSwitch takes it, so that an automatic rollback undoes this
	// switch and nothing that happened to the installation after it.
	Transaction string
}

// Map renders a result for a report.
func (r Result) Map() map[string]any {
	return map[string]any{
		"generation": r.Generation, "previous": nullable(r.Previous), "path": r.Path,
		"switched": r.Switched, "adopted": nullable(r.Adopted),
		"transaction": nullable(r.Transaction),
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// identity is the record each generation carries beside its binary.
type identity struct {
	Source      string `json:"source"`
	VersionLine string `json:"version_line,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Size        int64  `json:"size,omitempty"`
	AdoptedFrom string `json:"adopted_from,omitempty"`
	InstalledAt string `json:"installed_at"`
}

func (i *Installer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Installer) fault(step string) error {
	if i.Fault == nil {
		return nil
	}
	if err := i.Fault(step); err != nil {
		return fmt.Errorf("%s: %w", step, err)
	}
	return nil
}

// Apply installs the binary a release pins for this machine and makes it
// current. Every refusal leaves current exactly as it was.
func (i *Installer) Apply(pin *release.ProgramPin) (Result, error) {
	if pin == nil {
		return Result{}, failure.Value("this release pins no upkeeper binary")
	}
	target := i.Target
	if target == "" {
		target = release.ProgramTarget()
	}
	asset, known := pin.Assets[target]
	if !known {
		return Result{}, failure.Value("the upkeeper pin has no binary for this machine (%q)", target)
	}
	var result Result
	err := i.locked(func() error {
		// A rollback is an operator's or a failed update's decision, and an
		// automatic update may not undo it on the next scheduled run.
		if reason, held := i.held(); held {
			return failure.Value(
				"self-update is held (%s); observe this host, then install a binary with "+
					"`upkeeper self install` or remove %s", reason, i.Layout.Hold())
		}
		// An unrecoverable current does not block an install: the switch
		// below replaces it, and it is never kept as the previous generation.
		_ = i.recover()
		name := asset.SHA256
		if !i.Layout.validGeneration(name) {
			staged, cleanup, err := i.fetch(pin.URL(i.BaseURL, target), asset)
			defer cleanup()
			if err != nil {
				return err
			}
			if err := probe(staged, pin.VersionLine()); err != nil {
				return err
			}
			if err := i.installGeneration(staged, name, identity{
				Source: "release " + pin.Repository + "@" + pin.Tag, VersionLine: pin.VersionLine(),
				SHA256: asset.SHA256, Size: asset.Size,
			}); err != nil {
				return err
			}
		} else if err := probe(i.Layout.GenerationBinary(name), pin.VersionLine()); err != nil {
			// An installed generation is probed again rather than trusted for
			// its name alone: its name proves its bytes, not what they say.
			return err
		}
		// Adoption happens only once the new binary is verified and
		// installed, so that a refusal leaves an unmanaged host unmanaged.
		adopted, err := i.adopt()
		if err != nil {
			return err
		}
		result.Adopted = adopted
		transaction, err := i.switchTo(name, "apply "+pin.Tag)
		if err != nil {
			return err
		}
		result.Switched, result.Transaction = transaction != "", transaction
		result = i.describe(result)
		return nil
	})
	return result, err
}

// InstallFile installs a binary that is already on this machine, such as the
// one a controller staged for the first bootstrap, after checking it against
// the digest the operator expects and the version line it must print.
func (i *Installer) InstallFile(path, expectSHA256, versionLine string) (Result, error) {
	if !releasedName.MatchString(expectSHA256) {
		return Result{}, failure.Validation("--expect-sha256 must be 64 lowercase hex digits")
	}
	var result Result
	err := i.locked(func() error {
		_ = i.recover()
		staging, err := i.stagingDirectory()
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(staging) }()
		staged := filepath.Join(staging, binaryName)
		size, digest, err := copyVerified(path, staged)
		if err != nil {
			return err
		}
		if digest != expectSHA256 {
			return failure.Value("binary sha256 %s does not match the expected %s; nothing installed",
				digest, expectSHA256)
		}
		if err := probe(staged, versionLine); err != nil {
			return err
		}
		adopted, err := i.adopt()
		if err != nil {
			return err
		}
		result.Adopted = adopted
		if !i.Layout.validGeneration(digest) {
			if err := i.installGeneration(staged, digest, identity{
				Source: "self install", VersionLine: versionLine, SHA256: digest, Size: size,
			}); err != nil {
				return err
			}
		}
		transaction, err := i.switchTo(digest, "self install")
		if err != nil {
			return err
		}
		result.Switched, result.Transaction = transaction != "", transaction
		// An explicit install is the operator's answer to a held update.
		if err := os.Remove(i.Layout.Hold()); err == nil {
			i.journal(map[string]any{"event": "release-hold", "reason": "self install"})
		}
		result = i.describe(result)
		return nil
	})
	return result, err
}

// Rollback makes the previous generation current again, and the generation it
// replaces becomes the previous one, so a second rollback undoes the first.
func (i *Installer) Rollback(reason string) (Result, error) {
	var result Result
	err := i.locked(func() error {
		if err := i.recover(); err != nil {
			return err
		}
		var err error
		result, err = i.rollback(reason)
		return err
	})
	return result, err
}

// UndoSwitch is the rollback a failed self-update makes. It undoes exactly
// the switch its transaction names, and only while that switch is still the
// last change to the installation: current and previous are where it left
// them. Anything since, such as an operator's `self rollback` or install in
// the moment between the switch and the re-exec, was somebody's later
// decision, and is kept rather than reversed.
func (i *Installer) UndoSwitch(transaction, reason string) (Result, error) {
	if transaction == "" {
		return Result{}, failure.Value(
			"this update switched no generation, so it has nothing of its own to roll back")
	}
	var result Result
	err := i.locked(func() error {
		if err := i.recover(); err != nil {
			return err
		}
		record, _ := i.lastSwitch()
		current, previous := i.Layout.CurrentGeneration(), i.Layout.PreviousGeneration()
		if record.ID != transaction || current != record.To || previous != record.Previous {
			return failure.Value(
				"the installation changed after this update switched to %s; current is %s and "+
					"previous is %s, and that later change is kept: nothing rolled back",
				displayGeneration(record.To, record.ID == transaction), displayName(current),
				displayName(previous))
		}
		var err error
		result, err = i.rollback(reason)
		return err
	})
	return result, err
}

// rollback is Rollback for a caller that holds the lock and has recovered.
func (i *Installer) rollback(reason string) (Result, error) {
	var result Result
	// A binary somebody put in place by hand is not a generation; linking
	// over it would destroy it rather than roll anything back.
	if target, err := os.Readlink(i.Layout.Binary()); err != nil || target != binaryLink {
		return result, failure.Value("%s is not managed by generations; nothing to roll back",
			i.Layout.Binary())
	}
	previous := i.Layout.PreviousGeneration()
	if previous == "" || !i.Layout.validGeneration(previous) ||
		previous == i.Layout.CurrentGeneration() {
		return result, failure.Value("there is no previous upkeeper generation to roll back to")
	}
	// The hold goes first: a crash after it and before the switch holds
	// updates on the old binary, which is the conservative outcome.
	if err := i.Flush.WriteFile(i.Layout.Hold(),
		[]byte("rolled back from "+i.Layout.CurrentGeneration()+": "+reason+"\n"), 0o600); err != nil {
		return result, failure.Value("cannot record the rollback hold: %v", err)
	}
	transaction, err := i.switchTo(previous, "rollback: "+reason)
	result.Switched, result.Transaction = transaction != "", transaction
	if err == nil {
		result = i.describe(result)
	}
	return result, err
}

func displayGeneration(name string, known bool) string {
	if !known {
		return "a generation"
	}
	return displayName(name)
}

func displayName(name string) string {
	if name == "" {
		return "none"
	}
	return name
}

// held reports the rollback hold, which stops Apply until an operator acts.
func (i *Installer) held() (string, bool) {
	content, err := os.ReadFile(i.Layout.Hold())
	if err != nil {
		return "", false
	}
	reason := strings.TrimSpace(string(content))
	if reason == "" {
		reason = "rolled back"
	}
	return reason, true
}

// Recover repairs an installation an interrupted switch left behind: stray
// temporary links and staging directories are removed, and a current that
// names no complete generation is pointed back at the previous one.
func (i *Installer) Recover() error {
	return i.locked(i.recover)
}

func (i *Installer) describe(result Result) Result {
	result.Generation = i.Layout.CurrentGeneration()
	result.Previous = i.Layout.PreviousGeneration()
	if result.Generation != "" {
		result.Path = i.Layout.GenerationBinary(result.Generation)
	}
	return result
}

// locked runs a change under the installation's exclusive lock. A second
// change is refused rather than queued: two switches racing is never what
// either of their callers meant.
func (i *Installer) locked(change func() error) error {
	if err := fsatomic.EnsureDirectory(i.Layout.Root(), 0o700); err != nil {
		return failure.Value("cannot create %s: %v", i.Layout.Root(), err)
	}
	handle, err := os.OpenFile(i.Layout.Lock(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return failure.Value("cannot open the self-update lock: %v", err)
	}
	defer func() { _ = handle.Close() }()
	if err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return failure.LockContention("another upkeeper self-update holds %s", i.Layout.Lock())
	}
	defer func() { _ = syscall.Flock(int(handle.Fd()), syscall.LOCK_UN) }()
	return change()
}

func (i *Installer) stagingDirectory() (string, error) {
	if err := fsatomic.EnsureDirectory(i.Layout.Staging(), 0o700); err != nil {
		return "", failure.Value("cannot create the staging directory: %v", err)
	}
	directory, err := os.MkdirTemp(i.Layout.Staging(), "stage-")
	if err != nil {
		return "", failure.Value("cannot create a staging directory: %v", err)
	}
	return directory, nil
}

// installGeneration moves a verified staged binary into its generation. The
// binary is made read-only and synced before the directory appears under its
// final name, so a generation that exists is a generation that is complete.
func (i *Installer) installGeneration(staged, name string, record identity) error {
	if err := os.Chmod(staged, 0o500); err != nil {
		return failure.Value("cannot make the staged binary read-only: %v", err)
	}
	identityPath, err := i.writeIdentity(filepath.Dir(staged), record)
	if err != nil {
		return err
	}
	if err := i.fault("sync-staged"); err != nil {
		return err
	}
	if err := i.Flush.File(staged); err != nil {
		return failure.Value("cannot flush the staged binary: %v", err)
	}
	if err := i.Flush.File(identityPath); err != nil {
		return failure.Value("cannot flush the generation identity: %v", err)
	}
	return i.publishGeneration(filepath.Dir(staged), name)
}

// installLinkGeneration is installGeneration for an adopted link, which has no
// bytes of its own to make read-only or flush: the link is an entry of the
// staged directory, which publishGeneration flushes.
func (i *Installer) installLinkGeneration(staged, name string, record identity) error {
	identityPath, err := i.writeIdentity(filepath.Dir(staged), record)
	if err != nil {
		return err
	}
	if err := i.fault("sync-staged"); err != nil {
		return err
	}
	if err := i.Flush.File(identityPath); err != nil {
		return failure.Value("cannot flush the generation identity: %v", err)
	}
	return i.publishGeneration(filepath.Dir(staged), name)
}

// writeIdentity records a generation's identity beside its binary.
func (i *Installer) writeIdentity(directory string, record identity) (string, error) {
	record.InstalledAt = pytime.ISO8601Z(i.now())
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	path := filepath.Join(directory, "identity.json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o400); err != nil {
		return "", failure.Value("cannot record the generation identity: %v", err)
	}
	return path, nil
}

// publishGeneration gives a staged directory whose files are flushed its
// generation name. The staged directory is flushed first, so that its
// entries are durable before anything can name them, and the generations
// directory after, so that the name itself is; a failure of either stops the
// install before current can be switched to it.
func (i *Installer) publishGeneration(directory, name string) error {
	if err := fsatomic.EnsureDirectory(i.Layout.Generations(), 0o700); err != nil {
		return failure.Value("cannot create the generations directory: %v", err)
	}
	if err := i.fault("sync-staged-directory"); err != nil {
		return err
	}
	if err := i.Flush.Directory(directory); err != nil {
		return failure.Value("cannot flush the staged generation %s: %v", name, err)
	}
	final := i.Layout.Generation(name)
	// A directory of this name is only replaced after it failed to verify, so
	// it is debris from an interruption or a damaged copy. Even when current
	// names it, nothing usable is lost: current already names nothing that
	// runs, and the rename below makes it whole again.
	if _, err := os.Lstat(final); err == nil {
		if err := removeGeneration(final); err != nil {
			return failure.Value("cannot remove the incomplete generation %s: %v", name, err)
		}
	}
	if err := i.fault("rename-generation"); err != nil {
		return err
	}
	if err := os.Rename(directory, final); err != nil {
		return failure.Value("cannot install generation %s: %v", name, err)
	}
	if err := i.fault("sync-generations"); err != nil {
		return err
	}
	if err := i.Flush.Directory(i.Layout.Generations()); err != nil {
		return failure.Value("cannot flush the generations directory: %v", err)
	}
	return nil
}

// switchTo makes a generation current and the one it replaces previous, then
// makes sure ~/.local/bin/upkeeper runs current, then prunes. It returns the
// transaction of the switch, or "" when current already named the generation.
//
// The switch record is written before either link moves. A crash between the
// two leaves a record whose current is not the one on disk, which UndoSwitch
// treats as a later change: it refuses rather than guesses.
func (i *Installer) switchTo(name, reason string) (string, error) {
	old := i.Layout.CurrentGeneration()
	if old == name {
		return "", i.linkBinary()
	}
	keepOld := old != "" && i.Layout.validGeneration(old)
	previous := i.Layout.PreviousGeneration()
	if keepOld {
		previous = old
	}
	transaction, err := i.recordSwitch(switchRecord{From: old, To: name, Previous: previous})
	if err != nil {
		return "", err
	}
	if keepOld {
		if err := i.replaceLink(i.Layout.Previous(), filepath.Join("generations", old), "previous"); err != nil {
			return "", err
		}
	}
	if err := i.replaceLink(i.Layout.Current(), filepath.Join("generations", name), "current"); err != nil {
		return "", err
	}
	now := i.now()
	_ = os.Chtimes(i.Layout.Generation(name), now, now)
	i.journal(map[string]any{"event": "switch", "from": nullable(old), "to": name, "reason": reason,
		"transaction": transaction})
	if err := i.linkBinary(); err != nil {
		return transaction, err
	}
	i.prune()
	return transaction, nil
}

// switchRecord is the last switch made to the installation.
type switchRecord struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Previous string `json:"previous"`
}

// recordSwitch names a switch about to be made with a fresh transaction and
// replaces the record of the last one with it.
func (i *Installer) recordSwitch(record switchRecord) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", failure.Value("cannot name the switch: %v", err)
	}
	record.ID = hex.EncodeToString(buffer)
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	if err := i.Flush.WriteFile(i.Layout.Switch(), append(encoded, '\n'), 0o600); err != nil {
		return "", failure.Value("cannot record the switch: %v", err)
	}
	return record.ID, nil
}

// lastSwitch reads the record of the last switch, if there is one.
func (i *Installer) lastSwitch() (switchRecord, bool) {
	var record switchRecord
	content, err := os.ReadFile(i.Layout.Switch())
	if err != nil || json.Unmarshal(content, &record) != nil {
		return switchRecord{}, false
	}
	return record, true
}

// replaceLink points a symlink at a new target without a moment in which the
// link is missing: the new link is made beside it and renamed over it.
func (i *Installer) replaceLink(link, target, step string) error {
	directory := filepath.Dir(link)
	temporary := filepath.Join(directory, "."+filepath.Base(link)+".tmp-"+randomSuffix())
	if err := os.Symlink(target, temporary); err != nil {
		return failure.Value("cannot create a link to %s: %v", target, err)
	}
	if err := i.fault("rename-" + step); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, link); err != nil {
		_ = os.Remove(temporary)
		return failure.Value("cannot replace %s: %v", link, err)
	}
	if err := i.fault("sync-" + step); err != nil {
		return err
	}
	if err := i.Flush.Directory(directory); err != nil {
		return failure.Value("cannot flush the link %s: %v", link, err)
	}
	return nil
}

// linkBinary makes ~/.local/bin/upkeeper the link to current.
func (i *Installer) linkBinary() error {
	path := i.Layout.Binary()
	if target, err := os.Readlink(path); err == nil && target == binaryLink {
		return nil
	}
	if err := fsatomic.EnsureDirectory(filepath.Dir(path), 0o700); err != nil {
		return failure.Value("cannot create %s: %v", filepath.Dir(path), err)
	}
	return i.replaceLink(path, binaryLink, "binary")
}

// adopt preserves whatever ~/.local/bin/upkeeper held before this host was
// managed by generations, so that a rollback can return to it. A regular file
// is copied into an adopted generation; a symlink elsewhere, such as the
// Python launcher in the checkout, is recorded as an adopted link generation
// whose binary is that same link.
func (i *Installer) adopt() (string, error) {
	path := i.Layout.Binary()
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", failure.Value("cannot inspect %s: %v", path, err)
	}
	staging, err := i.stagingDirectory()
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	staged := filepath.Join(staging, binaryName)
	var name string
	var record identity
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", failure.Value("cannot read %s: %v", path, err)
		}
		if target == binaryLink {
			return "", nil
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		// A link into this installation, spelled some other way, is already
		// a generation; adopting it would make a link through current to
		// itself, or one that dangles once its generation is pruned.
		if strings.HasPrefix(filepath.Clean(target), i.Layout.Root()+string(filepath.Separator)) {
			return "", nil
		}
		if err := os.Symlink(target, staged); err != nil {
			return "", failure.Value("cannot record the adopted link: %v", err)
		}
		name = "adopted-link-" + digestOf([]byte(target))
		record = identity{Source: "adopted link", AdoptedFrom: target}
	case info.Mode().IsRegular():
		size, digest, err := copyVerified(path, staged)
		if err != nil {
			return "", err
		}
		name = "adopted-" + digest
		record = identity{Source: "adopted file", AdoptedFrom: path, SHA256: digest, Size: size}
	default:
		return "", failure.Value("%s is neither a file nor a link; refusing to replace it", path)
	}
	if !i.Layout.validGeneration(name) {
		if record.Source == "adopted link" {
			if err := i.installLinkGeneration(staged, name, record); err != nil {
				return "", err
			}
		} else if err := i.installGeneration(staged, name, record); err != nil {
			return "", err
		}
	}
	if _, err := i.switchTo(name, "adopt "+path); err != nil {
		return "", err
	}
	return name, nil
}

// recover is Recover without the lock, for callers that hold it.
func (i *Installer) recover() error {
	// Only the temporary links replaceLink itself makes are removed; nothing
	// else in ~/.local/bin belongs to this installation.
	for _, link := range []string{i.Layout.Current(), i.Layout.Previous(), i.Layout.Binary()} {
		directory := filepath.Dir(link)
		prefix := "." + filepath.Base(link) + ".tmp-"
		entries, _ := os.ReadDir(directory)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), prefix) && entry.Type()&fs.ModeSymlink != 0 {
				_ = os.Remove(filepath.Join(directory, entry.Name()))
			}
		}
	}
	_ = os.RemoveAll(i.Layout.Staging())
	current := i.Layout.CurrentGeneration()
	_, linkErr := os.Lstat(i.Layout.Current())
	if current != "" && i.Layout.validGeneration(current) {
		return nil
	}
	if current == "" && errors.Is(linkErr, fs.ErrNotExist) {
		// A host that was never managed has nothing to recover.
		return nil
	}
	previous := i.Layout.PreviousGeneration()
	if previous == "" || !i.Layout.validGeneration(previous) {
		return failure.Value(
			"%s names no complete generation and there is no previous one to restore; "+
				"install a released binary with `upkeeper self install`", i.Layout.Current())
	}
	if err := i.replaceLink(i.Layout.Current(), filepath.Join("generations", previous), "current"); err != nil {
		return err
	}
	i.journal(map[string]any{"event": "recover", "from": nullable(current), "to": previous,
		"reason": "current named no complete generation"})
	return nil
}

// prune keeps current, previous and the most recently activated others up to
// Keep in total. Anything it cannot remove is left for the next prune.
func (i *Installer) prune() {
	entries, err := os.ReadDir(i.Layout.Generations())
	if err != nil {
		return
	}
	protected := map[string]bool{
		i.Layout.CurrentGeneration():  true,
		i.Layout.PreviousGeneration(): true,
	}
	type candidate struct {
		name     string
		modified time.Time
	}
	others := []candidate{}
	kept := 0
	for _, entry := range entries {
		if protected[entry.Name()] {
			kept++
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		others = append(others, candidate{entry.Name(), info.ModTime()})
	}
	sort.Slice(others, func(a, b int) bool {
		if others[a].modified.Equal(others[b].modified) {
			return others[a].name < others[b].name
		}
		return others[a].modified.After(others[b].modified)
	})
	for _, other := range others {
		if kept < Keep {
			kept++
			continue
		}
		if removeGeneration(i.Layout.Generation(other.name)) == nil {
			i.journal(map[string]any{"event": "prune", "generation": other.name})
		}
	}
}

// removeGeneration deletes a generation whose binary is read-only.
func removeGeneration(path string) error {
	_ = os.Chmod(path, 0o700)
	return os.RemoveAll(path)
}

// journal appends one event. It is evidence, not state: a failure to write it
// never undoes or blocks the change it describes.
func (i *Installer) journal(event map[string]any) {
	event["at"] = pytime.ISO8601Z(i.now())
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	handle, err := os.OpenFile(i.Layout.Journal(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = handle.Close() }()
	if _, err := handle.Write(append(encoded, '\n')); err == nil {
		_ = handle.Sync()
	}
}
