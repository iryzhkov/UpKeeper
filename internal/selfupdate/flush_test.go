package selfupdate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
)

// realFlushError is what a flush that really fails returns: the kernel's
// answer to a flush of /proc, whose filesystem cannot flush a directory, or
// EIO where /proc accepts one.
func realFlushError(t *testing.T) error {
	t.Helper()
	if handle, err := os.Open("/proc"); err == nil {
		defer func() { _ = handle.Close() }()
		if err := handle.Sync(); err != nil {
			return err
		}
	}
	return &os.PathError{Op: "sync", Path: "/proc", Err: syscall.EIO}
}

// failingFlush flushes everything for real, except the files and directories
// fails selects, whose flush fails as a real one does. The count is how many
// flushes failed, so that a test can tell its flush was reached at all.
func failingFlush(t *testing.T, fails func(path string) bool) (fsatomic.Flusher, error, *int) {
	failed, hits := realFlushError(t), new(int)
	return fsatomic.Flusher{Sync: func(handle *os.File) error {
		if fails(handle.Name()) {
			*hits++
			return failed
		}
		return handle.Sync()
	}}, failed, hits
}

// stagedFor reports whether path is a staged generation directory whose
// identity names source.
func stagedFor(layout Layout, path, source string) bool {
	if filepath.Dir(path) != layout.Staging() {
		return false
	}
	content, err := os.ReadFile(filepath.Join(path, "identity.json"))
	return err == nil && strings.Contains(string(content), `"source":"`+source)
}

// Every flush an install relies on is checked. When one fails, the install
// refuses before it names the generation or switches current to it, and the
// host keeps running what it ran.
func TestAFailedFlushRefusesBeforeAnythingIsNamedOrSwitched(t *testing.T) {
	old, update := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
	for _, flush := range []struct {
		name string
		// fails selects the flush that fails, given the installation.
		fails func(layout Layout, path string) bool
		// named is whether the new generation exists under its name after
		// the failure: only the generations directory's own flush follows the
		// rename that names it.
		named bool
	}{
		{"staged binary", func(l Layout, path string) bool {
			return filepath.Dir(filepath.Dir(path)) == l.Staging() && filepath.Base(path) == binaryName
		}, false},
		{"generation identity", func(l Layout, path string) bool {
			return filepath.Dir(filepath.Dir(path)) == l.Staging() && filepath.Base(path) == "identity.json"
		}, false},
		{"staged directory", func(l Layout, path string) bool {
			return stagedFor(l, path, "release ")
		}, false},
		{"generations directory", func(l Layout, path string) bool {
			return path == l.Generations()
		}, true},
		{"switch record", func(l Layout, path string) bool {
			return strings.HasPrefix(filepath.Base(path), ".switch.json-")
		}, true},
		{"switch record's directory", func(l Layout, path string) bool {
			// The record is the first entry of the installation's root an
			// apply publishes, before either link moves.
			return path == l.Root()
		}, true},
	} {
		t.Run(flush.name, func(t *testing.T) {
			store, server := serve(t)
			target := installer(t, server)
			managedOn(t, store, target, old)
			record, _ := os.ReadFile(target.Layout.Switch())
			store.publish(update)
			flusher, failed, hits := failingFlush(t, func(path string) bool {
				return flush.fails(target.Layout, path)
			})
			target.Flush = flusher
			_, err := target.Apply(update.pin())
			if *hits == 0 {
				t.Fatal("the apply never made the flush")
			}
			if err == nil || !strings.Contains(err.Error(), failed.Error()) {
				t.Fatalf("a failed flush of the %s was reported as %v", flush.name, err)
			}
			target.Flush = fsatomic.Flusher{}
			if current := target.Layout.CurrentGeneration(); current != old.sha() {
				t.Errorf("current moved to %q", current)
			}
			if got := runBinary(t, target.Layout); got != versionOf(old) {
				t.Errorf("the host runs %q", got)
			}
			if _, err := os.Lstat(target.Layout.Generation(update.sha())); !flush.named && err == nil {
				t.Errorf("a generation whose flush failed was named")
			}
			if after, _ := os.ReadFile(target.Layout.Switch()); flush.name == "switch record" && !bytes.Equal(after, record) {
				t.Errorf("a switch record whose flush failed replaced the last one")
			}
			if _, err := target.Apply(update.pin()); err != nil {
				t.Fatalf("the next apply did not finish: %v", err)
			}
			if got := runBinary(t, target.Layout); got != versionOf(update) {
				t.Errorf("the next apply left the host running %q", got)
			}
		})
	}
}

// An unmanaged host adopts its binary into a generation before the first
// switch. When the flush of the adopted generation's directory fails, a
// copied file and a link alike, nothing is adopted or switched and the host
// still runs its own binary, as it was.
func TestAFailedFlushOfAnAdoptedGenerationLeavesTheHostUnmanaged(t *testing.T) {
	update := newProgram("v0.1.31", "b")
	for _, adopted := range []struct{ kind, source string }{
		{"file", "adopted file"}, {"link", "adopted link"},
	} {
		t.Run(adopted.kind, func(t *testing.T) {
			store, server := serve(t)
			target := installer(t, server)
			legacy := unmanagedHost(t, target, adopted.kind)
			before, _ := os.Lstat(target.Layout.Binary())
			link, _ := os.Readlink(target.Layout.Binary())
			store.publish(update)
			flusher, failed, hits := failingFlush(t, func(path string) bool {
				return stagedFor(target.Layout, path, adopted.source)
			})
			target.Flush = flusher
			_, err := target.Apply(update.pin())
			if *hits != 1 {
				t.Fatalf("the adopted generation's directory was flushed %d times", *hits)
			}
			if err == nil || !strings.Contains(err.Error(), failed.Error()) {
				t.Fatalf("a failed flush of the adopted generation was reported as %v", err)
			}
			target.Flush = fsatomic.Flusher{}
			after, err := os.Lstat(target.Layout.Binary())
			if err != nil || after.Mode() != before.Mode() {
				t.Fatalf("the unmanaged binary changed: %v %v", after, err)
			}
			if now, _ := os.Readlink(target.Layout.Binary()); now != link {
				t.Errorf("the unmanaged link now names %q", now)
			}
			if got := runBinary(t, target.Layout); got != versionOf(legacy) {
				t.Errorf("the host runs %q", got)
			}
			if _, err := os.Lstat(target.Layout.Current()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("current was switched: %v", err)
			}
			entries, _ := os.ReadDir(target.Layout.Generations())
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "adopted") {
					t.Errorf("a generation whose flush failed was named: %s", entry.Name())
				}
			}
			if _, err := target.Apply(update.pin()); err != nil {
				t.Fatalf("the next apply did not finish: %v", err)
			}
			if _, err := target.Rollback("operator"); err != nil {
				t.Fatalf("rollback: %v", err)
			}
			if got := runBinary(t, target.Layout); got != versionOf(legacy) {
				t.Errorf("a rollback restores %q, not the adopted program", got)
			}
		})
	}
}

// A link is replaced by a rename whose directory is then flushed. When that
// flush fails the apply reports it rather than success, current names a
// complete generation, and the installation is one the next apply finishes.
func TestAFailedFlushOfALinkIsReported(t *testing.T) {
	old, update := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
	for _, step := range []string{"previous", "current"} {
		t.Run(step, func(t *testing.T) {
			store, server := serve(t)
			target := installer(t, server)
			managedOn(t, store, target, old)
			store.publish(update)
			// The fault hook is crossed just before the flush of the link's
			// directory, which is the one flush that fails.
			armed := false
			target.Fault = func(at string) error {
				armed = at == "sync-"+step
				return nil
			}
			flusher, failed, hits := failingFlush(t, func(path string) bool {
				return armed && path == target.Layout.Root()
			})
			target.Flush = flusher
			result, err := target.Apply(update.pin())
			if *hits != 1 {
				t.Fatalf("the link's directory was flushed %d times", *hits)
			}
			if err == nil || !strings.Contains(err.Error(), failed.Error()) {
				t.Fatalf("a failed flush of the %s link was reported as %v (%+v)", step, err, result)
			}
			target.Fault, target.Flush = nil, fsatomic.Flusher{}
			current := target.Layout.CurrentGeneration()
			if (current != old.sha() && current != update.sha()) || !target.Layout.validGeneration(current) {
				t.Fatalf("current names %q", current)
			}
			if _, err := target.Apply(update.pin()); err != nil {
				t.Fatalf("the next apply did not finish: %v", err)
			}
			if got := runBinary(t, target.Layout); got != versionOf(update) {
				t.Errorf("the next apply left the host running %q", got)
			}
		})
	}
}

// A rollback records its hold before it switches. When the hold's flush
// fails, the rollback refuses and current stays where it was.
func TestAFailedFlushOfTheHoldRefusesTheRollback(t *testing.T) {
	old, update := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
	for _, flush := range []struct {
		name  string
		fails func(layout Layout, path string) bool
	}{
		{"hold", func(l Layout, path string) bool { return strings.HasPrefix(filepath.Base(path), ".held-") }},
		{"hold's directory", func(l Layout, path string) bool { return path == l.Root() }},
	} {
		t.Run(flush.name, func(t *testing.T) {
			store, server := serve(t)
			target := installer(t, server)
			managedOn(t, store, target, old)
			managedOn(t, store, target, update)
			flusher, failed, hits := failingFlush(t, func(path string) bool {
				return flush.fails(target.Layout, path)
			})
			target.Flush = flusher
			_, err := target.Rollback("operator")
			if *hits == 0 {
				t.Fatal("the rollback never flushed the hold")
			}
			if err == nil || !strings.Contains(err.Error(), failed.Error()) {
				t.Fatalf("a failed flush of the %s was reported as %v", flush.name, err)
			}
			if current := target.Layout.CurrentGeneration(); current != update.sha() {
				t.Errorf("current moved to %q", current)
			}
		})
	}
}
