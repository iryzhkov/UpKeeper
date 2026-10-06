package selfupdate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// marked reports whether a generation is recorded as durable.
func marked(layout Layout, name string) bool {
	info, err := os.Lstat(filepath.Join(layout.Generation(name), ".durable"))
	return err == nil && info.Mode().IsRegular()
}

// tracingFlush flushes everything for real and records, in order, the path
// of every file and directory it flushed.
func tracingFlush() (fsatomic.Flusher, *[]string) {
	trace := &[]string{}
	return fsatomic.Flusher{Sync: func(handle *os.File) error {
		*trace = append(*trace, handle.Name())
		return handle.Sync()
	}}, trace
}

// durableBeforeSwitch checks, in the trace of a change, that a generation was
// recorded as durable before anything switched to it: the generations
// directory was flushed, then the generation's marker was written and the
// generation's directory flushed, all before the first flush of the
// installation's root, which is the switch record's, the hold's or a link's.
func durableBeforeSwitch(t *testing.T, layout Layout, name string, trace []string) {
	t.Helper()
	generation := layout.Generation(name)
	first := func(from int, match func(path string) bool) int {
		for index := from; index < len(trace); index++ {
			if match(trace[index]) {
				return index
			}
		}
		return -1
	}
	marker := first(0, func(path string) bool {
		return filepath.Dir(path) == generation && strings.Contains(filepath.Base(path), ".durable")
	})
	if marker < 0 {
		t.Fatalf("the durability of %s was never recorded: %v", name, trace)
	}
	if parent := slices.Index(trace[:marker], layout.Generations()); parent < 0 {
		t.Fatalf("%s was recorded as durable without a flush of the generations directory: %v", name, trace)
	}
	recorded := first(marker+1, func(path string) bool { return path == generation })
	if recorded < 0 {
		t.Fatalf("the marker of %s was written but its directory never flushed: %v", name, trace)
	}
	switched := first(0, func(path string) bool {
		return path == layout.Root() || strings.HasPrefix(filepath.Base(path), ".switch.json-")
	})
	if switched < recorded {
		t.Fatalf("the switch to %s did not wait for its durability to be recorded: %v", name, trace)
	}
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
			if current := target.Layout.CurrentGeneration(); current != old.sha() {
				t.Errorf("current moved to %q", current)
			}
			if got := runBinary(t, target.Layout); got != versionOf(old) {
				t.Errorf("the host runs %q", got)
			}
			if _, err := os.Lstat(target.Layout.Generation(update.sha())); !flush.named && err == nil {
				t.Errorf("a generation whose flush failed was named")
			}
			if flush.name == "generations directory" && marked(target.Layout, update.sha()) {
				t.Errorf("a generation whose name was never flushed is recorded as durable")
			}
			if after, _ := os.ReadFile(target.Layout.Switch()); flush.name == "switch record" && !bytes.Equal(after, record) {
				t.Errorf("a switch record whose flush failed replaced the last one")
			}
			// The flush still fails. A retry finds whatever the failed attempt
			// left, makes the same flush again and refuses again, rather than
			// trusting what it found.
			reached := *hits
			if _, err := target.Apply(update.pin()); err == nil || !strings.Contains(err.Error(), failed.Error()) {
				t.Fatalf("a retry while the flush of the %s still fails was reported as %v", flush.name, err)
			}
			if *hits == reached {
				t.Errorf("a retry never made the flush that failed")
			}
			if current := target.Layout.CurrentGeneration(); current != old.sha() {
				t.Errorf("a retry moved current to %q", current)
			}
			// Once the flush succeeds, the next apply finishes, and a generation
			// that was not yet recorded as durable is flushed and recorded before
			// anything switches to it.
			unmarked := !marked(target.Layout, update.sha())
			flusher, trace := tracingFlush()
			target.Flush = flusher
			if _, err := target.Apply(update.pin()); err != nil {
				t.Fatalf("the next apply did not finish: %v", err)
			}
			if unmarked {
				durableBeforeSwitch(t, target.Layout, update.sha(), *trace)
			}
			if !marked(target.Layout, update.sha()) {
				t.Errorf("current names a generation not recorded as durable")
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

// The reviewer's reproduction of round 3: the flush of the generations
// directory keeps failing, and a retry must not switch to the generation the
// first attempt named on the strength of a flush that never succeeded.
func TestReviewerPersistentGenerationsSyncFailureRefusesRetry(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	old, update := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
	managedOn(t, store, target, old)
	store.publish(update)
	hits := 0
	target.Flush = fsatomic.Flusher{Sync: func(h *os.File) error {
		if h.Name() == target.Layout.Generations() {
			proc, err := os.Open("/proc")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = proc.Close() }()
			hits++
			return proc.Sync()
		}
		return h.Sync()
	}}
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := target.Apply(update.pin())
		if err == nil {
			t.Errorf("attempt %d accepted while generations Sync still fails (hits=%d)", attempt, hits)
		}
		if got := target.Layout.CurrentGeneration(); got != old.sha() {
			t.Errorf("attempt %d switched current to %q", attempt, got)
		}
	}
}

// A generation whose name could not be flushed exists and holds what its name
// says, but it is not published until it is recorded as durable. Every path
// that reuses or switches to an existing generation makes the flushes again
// when that record is missing, refuses for as long as one fails, leaving
// current where it was, and records the generation as durable before it
// switches once they succeed.
func TestAGenerationNotRecordedAsDurableIsFlushedBeforeItIsReused(t *testing.T) {
	old, update := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
	generationsOf := func(l Layout, path string) bool { return path == l.Generations() }
	for _, path := range []struct {
		name string
		// fails selects the flush that fails.
		fails func(layout Layout, path string) bool
		// setup brings the host to where the path starts. It returns the
		// change the path makes, a check that a refusal changed nothing, and
		// the name of the generation the path reuses.
		setup func(t *testing.T, store *artifacts, target *Installer) (
			change func() error, unchanged func(t *testing.T), reused func() string)
	}{
		{"apply", generationsOf, func(t *testing.T, store *artifacts, target *Installer) (
			func() error, func(*testing.T), func() string) {
			managedOn(t, store, target, old)
			store.publish(update)
			return func() error { _, err := target.Apply(update.pin()); return err },
				runsOld(target, old), update.sha
		}},
		{"install file", generationsOf, func(t *testing.T, store *artifacts, target *Installer) (
			func() error, func(*testing.T), func() string) {
			managedOn(t, store, target, old)
			staged := filepath.Join(t.TempDir(), "upkeeper")
			if err := os.WriteFile(staged, update.body, 0o700); err != nil {
				t.Fatal(err)
			}
			return func() error { _, err := target.InstallFile(staged, update.sha(), versionOf(update)); return err },
				runsOld(target, old), update.sha
		}},
		{"adopted file", adoptedIn, adoptedReuse("file", update)},
		{"adopted link", adoptedIn, adoptedReuse("link", update)},
		{"rollback", generationsOf, func(t *testing.T, store *artifacts, target *Installer) (
			func() error, func(*testing.T), func() string) {
			managedOn(t, store, target, old)
			managedOn(t, store, target, update)
			unrecord(t, target.Layout, old.sha())
			return func() error { _, err := target.Rollback("operator"); return err },
				func(t *testing.T) {
					if current := target.Layout.CurrentGeneration(); current != update.sha() {
						t.Errorf("current moved to %q", current)
					}
					if _, held := target.held(); held {
						t.Errorf("a refused rollback recorded its hold")
					}
				}, old.sha
		}},
		{"recovery", generationsOf, func(t *testing.T, store *artifacts, target *Installer) (
			func() error, func(*testing.T), func() string) {
			managedOn(t, store, target, old)
			managedOn(t, store, target, update)
			unrecord(t, target.Layout, old.sha())
			if err := removeGeneration(target.Layout.Generation(update.sha())); err != nil {
				t.Fatal(err)
			}
			return target.Recover, func(t *testing.T) {
				if current := target.Layout.CurrentGeneration(); current != update.sha() {
					t.Errorf("a refused recovery moved current to %q", current)
				}
			}, old.sha
		}},
	} {
		t.Run(path.name, func(t *testing.T) {
			store, server := serve(t)
			target := installer(t, server)
			change, unchanged, reused := path.setup(t, store, target)
			flusher, failed, hits := failingFlush(t, func(p string) bool { return path.fails(target.Layout, p) })
			target.Flush = flusher
			for attempt := 1; attempt <= 3; attempt++ {
				reached := *hits
				if err := change(); err == nil || !strings.Contains(err.Error(), failed.Error()) {
					t.Fatalf("attempt %d while the flush still fails was reported as %v", attempt, err)
				}
				if *hits == reached {
					t.Errorf("attempt %d never made the flush that failed", attempt)
				}
				unchanged(t)
				if name := reused(); name == "" || !target.Layout.validGeneration(name) {
					t.Fatalf("attempt %d left no generation to reuse (%q)", attempt, name)
				} else if marked(target.Layout, name) {
					t.Errorf("attempt %d recorded %s as durable", attempt, name)
				}
			}
			flusher, trace := tracingFlush()
			target.Flush = flusher
			if err := change(); err != nil {
				t.Fatalf("once the flush succeeds: %v", err)
			}
			durableBeforeSwitch(t, target.Layout, reused(), *trace)
		})
	}
}

// runsOld checks that a managed host still runs old from current.
func runsOld(target *Installer, old program) func(t *testing.T) {
	return func(t *testing.T) {
		if current := target.Layout.CurrentGeneration(); current != old.sha() {
			t.Errorf("current moved to %q", current)
		}
		if got := runBinary(t, target.Layout); got != versionOf(old) {
			t.Errorf("the host runs %q", got)
		}
	}
}

// adoptedIn selects the flush of the generations directory once an adopted
// generation has been named in it: the pinned generation an adopting apply
// installs first is published, and the adopted one is not.
func adoptedIn(layout Layout, path string) bool {
	return path == layout.Generations() && adoptedGeneration(layout) != ""
}

// adoptedGeneration names the adopted generation, or "".
func adoptedGeneration(layout Layout) string {
	entries, _ := os.ReadDir(layout.Generations())
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "adopted") {
			return entry.Name()
		}
	}
	return ""
}

// adoptedReuse is the setup of an apply on an unmanaged host, whose retry
// reuses the generation the first attempt adopted.
func adoptedReuse(kind string, update program) func(t *testing.T, store *artifacts, target *Installer) (
	func() error, func(*testing.T), func() string) {
	return func(t *testing.T, store *artifacts, target *Installer) (func() error, func(*testing.T), func() string) {
		legacy := unmanagedHost(t, target, kind)
		store.publish(update)
		return func() error { _, err := target.Apply(update.pin()); return err },
			func(t *testing.T) {
				if _, err := os.Lstat(target.Layout.Current()); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("current was switched: %v", err)
				}
				if got := runBinary(t, target.Layout); got != versionOf(legacy) {
					t.Errorf("the host runs %q", got)
				}
			}, func() string { return adoptedGeneration(target.Layout) }
	}
}

// unrecord removes a generation's record of durability, as a generation that
// was named but whose name was never flushed lacks it.
func unrecord(t *testing.T, layout Layout, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(layout.Generation(name), ".durable")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
