package selfupdate

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

const testTarget = "linux-amd64"

// program is a stand-in binary: a shell script that answers `version` the way
// the real binary does. The probe runs it with an empty PATH, which a script
// with an absolute interpreter and a builtin echo survives.
type program struct {
	tag    string
	commit string
	body   []byte
}

func newProgram(tag, letter string) program {
	commit := strings.Repeat(letter, 40)
	return program{tag: tag, commit: commit, body: []byte(
		"#!/bin/sh\necho 'upkeeper " + tag + " (commit " + commit + ")'\n")}
}

func (p program) sha() string { return digestOf(p.body) }

func (p program) pin() *release.ProgramPin {
	asset := release.ProgramAsset{Asset: "upkeeper-" + testTarget, SHA256: p.sha(), Size: int64(len(p.body))}
	return &release.ProgramPin{
		Repository: release.ProgramRepository, Tag: p.tag, SourceCommit: p.commit,
		Toolchain: "go1.24.13", Assets: map[string]release.ProgramAsset{
			testTarget:    asset,
			"linux-arm64": {Asset: "upkeeper-linux-arm64", SHA256: strings.Repeat("9", 64), Size: 1},
		},
	}
}

// artifacts is a local release server: every test request stays on loopback.
type artifacts struct {
	mutex    sync.Mutex
	bodies   map[string][]byte
	status   map[string]int
	length   map[string]string
	requests []string
}

func serve(t *testing.T) (*artifacts, *httptest.Server) {
	t.Helper()
	store := &artifacts{bodies: map[string][]byte{}, status: map[string]int{}, length: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		store.mutex.Lock()
		defer store.mutex.Unlock()
		store.requests = append(store.requests, request.URL.Path)
		if status, set := store.status[request.URL.Path]; set {
			writer.WriteHeader(status)
			return
		}
		body, known := store.bodies[request.URL.Path]
		if !known {
			http.NotFound(writer, request)
			return
		}
		if store.length[request.URL.Path] == "chunked" {
			// Flushing before the end leaves the length undeclared, as a
			// streamed response would.
			_, _ = writer.Write(body[:1])
			writer.(http.Flusher).Flush()
			_, _ = writer.Write(body[1:])
			return
		}
		_, _ = writer.Write(body)
	}))
	t.Cleanup(server.Close)
	return store, server
}

func assetPath(tag string) string {
	return "/" + release.ProgramRepository + "/releases/download/" + tag + "/upkeeper-" + testTarget
}

func (a *artifacts) publish(p program) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.bodies[assetPath(p.tag)] = p.body
}

func installer(t *testing.T, server *httptest.Server) *Installer {
	t.Helper()
	return &Installer{
		Layout: Layout{Home: t.TempDir()}, Client: server.Client(), BaseURL: server.URL,
		Target: testTarget,
	}
}

// runBinary runs ~/.local/bin/upkeeper version and returns what it printed.
func runBinary(t *testing.T, layout Layout) string {
	t.Helper()
	var output []byte
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		output, err = exec.Command(layout.Binary(), "version").Output()
		if !errors.Is(err, syscall.ETXTBSY) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("running %s: %v", layout.Binary(), err)
	}
	return strings.TrimSpace(string(output))
}

func versionOf(p program) string { return "upkeeper " + p.tag + " (commit " + p.commit + ")" }

func managedOn(t *testing.T, store *artifacts, target *Installer, p program) {
	t.Helper()
	store.publish(p)
	if _, err := target.Apply(p.pin()); err != nil {
		t.Fatalf("installing %s: %v", p.tag, err)
	}
}

func journalEvents(t *testing.T, layout Layout) []string {
	t.Helper()
	handle, err := os.Open(layout.Journal())
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	defer func() { _ = handle.Close() }()
	events := []string{}
	scanner := bufio.NewScanner(handle)
	for scanner.Scan() {
		events = append(events, scanner.Text())
	}
	return events
}

// The happy path: a host running a hand-installed binary adopts it, installs
// the pinned one as a verified generation and runs it from then on.
func TestApplyAdoptsInstallsAndSwitches(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	original := newProgram("v0.1.30", "a")
	if err := os.MkdirAll(filepath.Dir(target.Layout.Binary()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Layout.Binary(), original.body, 0o755); err != nil {
		t.Fatal(err)
	}
	update := newProgram("v0.1.31", "b")
	store.publish(update)
	result, err := target.Apply(update.pin())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Adopted != "adopted-"+original.sha() || !result.Switched ||
		result.Generation != update.sha() || result.Previous != result.Adopted {
		t.Fatalf("result %+v", result)
	}
	if result.Path != target.Layout.GenerationBinary(update.sha()) {
		t.Errorf("re-exec path %s is not the generation's own binary", result.Path)
	}
	if link, _ := os.Readlink(target.Layout.Binary()); link != "../lib/upkeeper/current/upkeeper" {
		t.Errorf("~/.local/bin/upkeeper links to %q", link)
	}
	if link, _ := os.Readlink(target.Layout.Current()); link != "generations/"+update.sha() {
		t.Errorf("current is the relative link %q", link)
	}
	if got := runBinary(t, target.Layout); got != versionOf(update) {
		t.Errorf("the installed binary says %q", got)
	}
	info, err := os.Stat(result.Path)
	if err != nil || info.Mode().Perm() != 0o500 {
		t.Errorf("generation binary mode %v, %v", info.Mode(), err)
	}
	if ActiveGeneration(target.Layout.Home) != update.sha() {
		t.Errorf("active generation %q", ActiveGeneration(target.Layout.Home))
	}
	if events := journalEvents(t, target.Layout); len(events) != 2 ||
		!strings.Contains(events[0], `"adopt `) || !strings.Contains(events[1], `"apply v0.1.31"`) {
		t.Errorf("journal %v", events)
	}
	if entries, _ := os.ReadDir(target.Layout.Staging()); len(entries) != 0 {
		t.Errorf("staging was left behind: %v", entries)
	}
	// Applying the same pin again changes nothing and downloads nothing.
	before := len(store.requests)
	again, err := target.Apply(update.pin())
	if err != nil || again.Switched || len(store.requests) != before {
		t.Errorf("a second apply switched=%v err=%v requests=%d", again.Switched, err, len(store.requests)-before)
	}
}

// Every refusal leaves current exactly where it was, installs no generation
// and leaves nothing staged.
func TestApplyRefusalsLeaveCurrentUnchanged(t *testing.T) {
	current := newProgram("v0.1.30", "a")
	update := newProgram("v0.1.31", "b")
	impostor := newProgram("v0.1.31", "c")
	cases := []struct {
		name     string
		prepare  func(store *artifacts, pin *release.ProgramPin)
		expected string
	}{
		{"a digest mismatch", func(store *artifacts, pin *release.ProgramPin) {
			store.bodies[assetPath(update.tag)] = impostor.body
		}, "has sha256"},
		{"a shorter body", func(store *artifacts, pin *release.ProgramPin) {
			store.bodies[assetPath(update.tag)] = update.body[:10]
		}, "the pin says"},
		{"a shorter streamed body", func(store *artifacts, pin *release.ProgramPin) {
			store.bodies[assetPath(update.tag)] = update.body[:10]
			store.length[assetPath(update.tag)] = "chunked"
		}, "is 10 bytes"},
		{"a longer body without a length", func(store *artifacts, pin *release.ProgramPin) {
			store.bodies[assetPath(update.tag)] = append(slices.Clone(update.body), "# more\n"...)
			store.length[assetPath(update.tag)] = "chunked"
			asset := pin.Assets[testTarget]
			asset.SHA256 = digestOf(store.bodies[assetPath(update.tag)])
			pin.Assets[testTarget] = asset
		}, "more than"},
		{"a size the pin does not name", func(store *artifacts, pin *release.ProgramPin) {
			asset := pin.Assets[testTarget]
			asset.Size++
			pin.Assets[testTarget] = asset
		}, "the pin says"},
		{"a missing asset", func(store *artifacts, pin *release.ProgramPin) {
			delete(store.bodies, assetPath(update.tag))
		}, "HTTP 404"},
		{"a server error", func(store *artifacts, pin *release.ProgramPin) {
			store.status[assetPath(update.tag)] = http.StatusBadGateway
		}, "HTTP 502"},
		{"the wrong version output", func(store *artifacts, pin *release.ProgramPin) {
			pin.SourceCommit = strings.Repeat("d", 40)
		}, "nothing installed"},
		{"the wrong tag in the version", func(store *artifacts, pin *release.ProgramPin) {
			pin.Tag = "v0.1.32"
			store.bodies[assetPath("v0.1.32")] = update.body
		}, "reports"},
		{"a probe that fails", func(store *artifacts, pin *release.ProgramPin) {
			body := []byte("#!/bin/sh\nexit 3\n")
			store.bodies[assetPath(update.tag)] = body
			pin.Assets[testTarget] = release.ProgramAsset{
				Asset: "upkeeper-" + testTarget, SHA256: digestOf(body), Size: int64(len(body))}
		}, "failed its version probe"},
		{"a probe that needs PATH", func(store *artifacts, pin *release.ProgramPin) {
			body := []byte("#!/bin/sh\nenv | grep -q . && echo '" + versionOf(update) + "'\n")
			store.bodies[assetPath(update.tag)] = body
			pin.Assets[testTarget] = release.ProgramAsset{
				Asset: "upkeeper-" + testTarget, SHA256: digestOf(body), Size: int64(len(body))}
		}, "failed its version probe"},
		{"no binary for this machine", func(store *artifacts, pin *release.ProgramPin) {
			delete(pin.Assets, testTarget)
		}, "no binary for this machine"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store, server := serve(t)
			target := installer(t, server)
			managedOn(t, store, target, current)
			store.publish(update)
			pin := update.pin()
			test.prepare(store, pin)
			_, err := target.Apply(pin)
			if err == nil || !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("error %v, want %q", err, test.expected)
			}
			if !failure.IsExpected(err) {
				t.Errorf("the refusal is not an expected failure: %v", err)
			}
			if target.Layout.CurrentGeneration() != current.sha() {
				t.Errorf("current moved to %q", target.Layout.CurrentGeneration())
			}
			if got := runBinary(t, target.Layout); got != versionOf(current) {
				t.Errorf("the host now runs %q", got)
			}
			entries, _ := os.ReadDir(target.Layout.Generations())
			if len(entries) != 1 {
				t.Errorf("a refused binary left generations behind: %v", entries)
			}
			if entries, _ := os.ReadDir(target.Layout.Staging()); len(entries) != 0 {
				t.Errorf("staging was left behind: %v", entries)
			}
		})
	}
}

// A refusal on a host that was never managed leaves it unmanaged: the binary
// it runs is not adopted, moved or relinked for an update that did not happen.
func TestARefusalLeavesAnUnmanagedHostUnmanaged(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	original := newProgram("v0.1.30", "a")
	if err := os.MkdirAll(filepath.Dir(target.Layout.Binary()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Layout.Binary(), original.body, 0o755); err != nil {
		t.Fatal(err)
	}
	update := newProgram("v0.1.31", "b")
	store.bodies[assetPath(update.tag)] = newProgram("v0.1.31", "c").body
	if _, err := target.Apply(update.pin()); err == nil {
		t.Fatal("a digest mismatch was accepted")
	}
	if info, err := os.Lstat(target.Layout.Binary()); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the unmanaged binary was replaced: %v %v", info, err)
	}
	if _, err := os.Lstat(target.Layout.Current()); err == nil {
		t.Error("a refusal created current")
	}
	if _, err := os.Stat(target.Layout.Generations()); err == nil {
		t.Error("a refusal created generations")
	}
}

// A rollback never links over a binary somebody put in place by hand, never
// reports success for a switch to itself, and holds automatic updates until
// an operator installs a binary again.
func TestRollbackRefusalsAndHold(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	first, second := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
	managedOn(t, store, target, first)
	managedOn(t, store, target, second)

	hand := newProgram("v9.9.9", "9")
	if err := os.Remove(target.Layout.Binary()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Layout.Binary(), hand.body, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Rollback("operator"); err == nil || !strings.Contains(err.Error(), "not managed") {
		t.Fatalf("a rollback over a hand-installed binary produced %v", err)
	}
	if got := runBinary(t, target.Layout); got != versionOf(hand) {
		t.Fatalf("the hand-installed binary was replaced: %q", got)
	}
	if err := os.Remove(target.Layout.Binary()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../lib/upkeeper/current/upkeeper", target.Layout.Binary()); err != nil {
		t.Fatal(err)
	}

	if _, err := target.Rollback("operator"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := target.Apply(second.pin()); err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("an update after a rollback produced %v", err)
	}
	if target.Layout.CurrentGeneration() != first.sha() {
		t.Fatal("a held update switched")
	}
	if target.Status()["held"] == nil {
		t.Error("status does not report the hold")
	}
	staged := filepath.Join(t.TempDir(), "upkeeper")
	if err := os.WriteFile(staged, second.body, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := target.InstallFile(staged, second.sha(), versionOf(second)); err != nil {
		t.Fatalf("an explicit install was refused: %v", err)
	}
	if _, err := os.Stat(target.Layout.Hold()); err == nil {
		t.Error("an explicit install did not release the hold")
	}

	// previous == current, as an interruption between the two links leaves it.
	if err := os.Remove(target.Layout.Previous()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("generations/"+second.sha(), target.Layout.Previous()); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Rollback("operator"); err == nil {
		t.Error("a rollback onto the current generation reported success")
	}
}

// A damaged current whose digest is the pinned one, with nothing to fall back
// on, is repaired by installing the pin again rather than refused forever.
func TestADamagedCurrentWithThePinnedDigestIsReplaced(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	update := newProgram("v0.1.31", "b")
	managedOn(t, store, target, update)
	path := target.Layout.GenerationBinary(update.sha())
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Apply(update.pin()); err != nil {
		t.Fatalf("the damaged generation was not replaced: %v", err)
	}
	if got := runBinary(t, target.Layout); got != versionOf(update) {
		t.Errorf("the host runs %q", got)
	}
}

// A binary path that already links into the installation, spelled
// absolutely, is not adopted as a link to itself.
func TestALinkIntoTheInstallationIsNotAdopted(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	first, second := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
	managedOn(t, store, target, first)
	if err := os.Remove(target.Layout.Binary()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target.Layout.Current(), "upkeeper"), target.Layout.Binary()); err != nil {
		t.Fatal(err)
	}
	store.publish(second)
	result, err := target.Apply(second.pin())
	if err != nil || result.Adopted != "" {
		t.Fatalf("apply %+v, %v", result, err)
	}
	if got := runBinary(t, target.Layout); got != versionOf(second) {
		t.Errorf("the host runs %q", got)
	}
}

// An unmanaged host with nothing at ~/.local/bin/upkeeper has nothing to adopt.
func TestAFreshHostInstallsWithoutAdopting(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	update := newProgram("v0.1.31", "b")
	store.publish(update)
	result, err := target.Apply(update.pin())
	if err != nil || result.Adopted != "" || result.Previous != "" {
		t.Fatalf("result %+v, %v", result, err)
	}
	if _, err := target.Rollback("test"); err == nil {
		t.Error("a rollback with no previous generation was accepted")
	}
}

// A rollback restores the previous generation, and a second one undoes it.
func TestRollbackSwapsCurrentAndPrevious(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	first, second := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
	managedOn(t, store, target, first)
	managedOn(t, store, target, second)
	result, err := target.Rollback("operator")
	if err != nil || result.Generation != first.sha() || result.Previous != second.sha() {
		t.Fatalf("rollback %+v, %v", result, err)
	}
	if got := runBinary(t, target.Layout); got != versionOf(first) {
		t.Errorf("after rollback the host runs %q", got)
	}
	if result, err := target.Rollback("operator"); err != nil || result.Generation != second.sha() {
		t.Fatalf("second rollback %+v, %v", result, err)
	}
}

// The symlink a host held before, such as the Python launcher inside the
// checkout, is preserved as a generation and restored by a rollback.
func TestAnAdoptedLinkIsRestoredByRollback(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	home := target.Layout.Home
	launcher := filepath.Join(home, ".local/share/dev-fleet/scripts/upkeeper")
	legacy := newProgram("0.1.0-python", "e")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, legacy.body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target.Layout.Binary()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../share/dev-fleet/scripts/upkeeper", target.Layout.Binary()); err != nil {
		t.Fatal(err)
	}
	update := newProgram("v0.1.31", "b")
	store.publish(update)
	result, err := target.Apply(update.pin())
	if err != nil || !strings.HasPrefix(result.Adopted, "adopted-link-") {
		t.Fatalf("apply %+v, %v", result, err)
	}
	if _, err := target.Rollback("operator"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := runBinary(t, target.Layout); got != versionOf(legacy) {
		t.Errorf("after rollback the host runs %q", got)
	}
	recorded, _ := os.Readlink(target.Layout.GenerationBinary(result.Adopted))
	if recorded != launcher {
		t.Errorf("the adopted link records %q, not the absolute launcher %q", recorded, launcher)
	}
}

// Three generations are kept; current and previous never go, however old.
func TestRetentionKeepsThreeAndNeverTheActiveOnes(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	clock := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	target.Now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	programs := []program{}
	for index, letter := range []string{"a", "b", "c", "d", "e"} {
		p := newProgram("v0.1."+string(rune('0'+index)), letter)
		programs = append(programs, p)
		managedOn(t, store, target, p)
	}
	entries, _ := os.ReadDir(target.Layout.Generations())
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{programs[2].sha(), programs[3].sha(), programs[4].sha()}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("kept %v, want %v", names, want)
	}
	// Roll back to d, then install a sixth: d is current, e is previous, and
	// the oldest other generation goes even though d was activated long ago.
	if _, err := target.Rollback("test"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target.Layout.Hold()); err != nil {
		t.Fatalf("the rollback left no hold: %v", err)
	}
	sixth := newProgram("v0.1.9", "f")
	managedOn(t, store, target, sixth)
	if _, err := target.Rollback("test"); err != nil {
		t.Fatal(err)
	}
	if target.Layout.CurrentGeneration() != programs[3].sha() {
		t.Errorf("rollback after prune reached %q", target.Layout.CurrentGeneration())
	}
	entries, _ = os.ReadDir(target.Layout.Generations())
	if len(entries) != Keep {
		t.Errorf("%d generations kept", len(entries))
	}
}

// boundaries are every fsync and rename an apply onto a managed host crosses.
var boundaries = []string{
	"sync-staged", "sync-staged-directory", "rename-generation", "sync-generations",
	"rename-previous", "sync-previous", "rename-current", "sync-current",
}

// An apply interrupted at any boundary leaves a current that names a complete
// generation, a binary that runs, and an installation the next apply finishes.
func TestAnInterruptedSwitchRecoversToAValidCurrent(t *testing.T) {
	seen := []string{}
	{
		store, server := serve(t)
		target := installer(t, server)
		managedOn(t, store, target, newProgram("v0.1.30", "a"))
		target.Fault = func(step string) error { seen = append(seen, step); return nil }
		managedOn(t, store, target, newProgram("v0.1.31", "b"))
	}
	for _, step := range boundaries {
		if !slices.Contains(seen, step) {
			t.Fatalf("an apply never crosses %s; it crossed %v", step, seen)
		}
	}
	for _, step := range append(slices.Clone(boundaries), "rename-binary", "sync-binary") {
		t.Run(step, func(t *testing.T) {
			store, server := serve(t)
			target := installer(t, server)
			old, update := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
			managedOn(t, store, target, old)
			if step == "rename-binary" || step == "sync-binary" {
				// The binary link is only replaced when it is not already
				// the link to current, so break it first.
				if err := os.Remove(target.Layout.Binary()); err != nil {
					t.Fatal(err)
				}
			}
			store.publish(update)
			interrupted := errors.New("interrupted")
			target.Fault = func(at string) error {
				if at == step {
					return interrupted
				}
				return nil
			}
			if _, err := target.Apply(update.pin()); !errors.Is(err, interrupted) {
				t.Fatalf("the fault at %s was not reported: %v", step, err)
			}
			target.Fault = nil
			current := target.Layout.CurrentGeneration()
			if current != old.sha() && current != update.sha() {
				t.Fatalf("current names %q", current)
			}
			if !target.Layout.validGeneration(current) {
				t.Fatalf("current names an incomplete generation %q", current)
			}
			if err := target.Recover(); err != nil {
				t.Fatalf("recover: %v", err)
			}
			if _, err := target.Apply(update.pin()); err != nil {
				t.Fatalf("the next apply did not finish the switch: %v", err)
			}
			if got := runBinary(t, target.Layout); got != versionOf(update) {
				t.Errorf("after recovery the host runs %q", got)
			}
			if target.Layout.PreviousGeneration() != old.sha() {
				t.Errorf("previous is %q, not the generation the switch left",
					target.Layout.PreviousGeneration())
			}
		})
	}
}

// unmanagedHost gives a host the binary it ran before generations: a regular
// file, or a link to a launcher elsewhere, such as the Python checkout's.
func unmanagedHost(t *testing.T, target *Installer, kind string) program {
	t.Helper()
	legacy := newProgram("v0.1.30", "a")
	if err := os.MkdirAll(filepath.Dir(target.Layout.Binary()), 0o700); err != nil {
		t.Fatal(err)
	}
	if kind == "file" {
		if err := os.WriteFile(target.Layout.Binary(), legacy.body, 0o755); err != nil {
			t.Fatal(err)
		}
		return legacy
	}
	launcher := filepath.Join(target.Layout.Home, ".local/share/dev-fleet/scripts/upkeeper")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, legacy.body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../share/dev-fleet/scripts/upkeeper", target.Layout.Binary()); err != nil {
		t.Fatal(err)
	}
	return legacy
}

// The first apply on an unmanaged host installs two generations, the pinned
// one and the adopted one, and switches twice. Each generation's directory is
// flushed before it appears under its name, and an interruption at any of
// the apply's fsync and rename boundaries, in the adoption as much as in the
// install, leaves a binary that runs and an installation the next apply
// finishes with the adopted program as the one a rollback restores.
func TestEveryBoundaryOfAnAdoptingApplyIsRecoverable(t *testing.T) {
	update := newProgram("v0.1.31", "b")
	for _, kind := range []string{"file", "link"} {
		t.Run(kind, func(t *testing.T) {
			store, server := serve(t)
			target := installer(t, server)
			unmanagedHost(t, target, kind)
			store.publish(update)
			trace := []string{}
			target.Fault = func(step string) error { trace = append(trace, step); return nil }
			if _, err := target.Apply(update.pin()); err != nil {
				t.Fatalf("a clean apply: %v", err)
			}
			for _, step := range []string{"sync-staged", "sync-staged-directory", "rename-generation", "sync-generations"} {
				if count := len(slices.DeleteFunc(slices.Clone(trace), func(s string) bool { return s != step })); count != 2 {
					t.Fatalf("installing the pinned and the adopted generation crossed %s %d times: %v", step, count, trace)
				}
			}
			for index, step := range trace {
				t.Run(fmt.Sprintf("%02d-%s", index, step), func(t *testing.T) {
					store, server := serve(t)
					target := installer(t, server)
					legacy := unmanagedHost(t, target, kind)
					store.publish(update)
					interrupted := errors.New("interrupted")
					crossed := 0
					target.Fault = func(string) error {
						defer func() { crossed++ }()
						if crossed == index {
							return interrupted
						}
						return nil
					}
					if _, err := target.Apply(update.pin()); !errors.Is(err, interrupted) {
						t.Fatalf("the fault at %s was not reported: %v", step, err)
					}
					target.Fault = nil
					if got := runBinary(t, target.Layout); got != versionOf(legacy) && got != versionOf(update) {
						t.Fatalf("after the interruption the host runs %q", got)
					}
					if err := target.Recover(); err != nil {
						t.Fatalf("recover: %v", err)
					}
					if _, err := target.Apply(update.pin()); err != nil {
						t.Fatalf("the next apply did not finish: %v", err)
					}
					if got := runBinary(t, target.Layout); got != versionOf(update) {
						t.Fatalf("after recovery the host runs %q", got)
					}
					if _, err := target.Rollback("operator"); err != nil {
						t.Fatalf("rollback: %v", err)
					}
					if got := runBinary(t, target.Layout); got != versionOf(legacy) {
						t.Errorf("a rollback restores %q, not the adopted program", got)
					}
				})
			}
		})
	}
}

// A crash leaves debris that no error path cleaned up: a temporary link, a
// staging directory and a generation directory without its binary. Recovery
// removes the first two and the next apply replaces the third.
func TestCrashDebrisIsCleanedAndReplaced(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	old, update := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
	managedOn(t, store, target, old)
	temporary := filepath.Join(target.Layout.Root(), ".current.tmp-0123456789ab")
	if err := os.Symlink("generations/"+update.sha(), temporary); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(filepath.Dir(target.Layout.Binary()), ".other-tool.tmp-1")
	if err := os.Symlink("elsewhere", foreign); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target.Layout.Staging(), "stage-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target.Layout.Generation(update.sha()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := target.Recover(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if _, err := os.Lstat(temporary); err == nil {
		t.Error("the temporary link survived recovery")
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Error("recovery removed a link it did not make")
	}
	if _, err := os.Stat(target.Layout.Staging()); err == nil {
		t.Error("the staging directory survived recovery")
	}
	store.publish(update)
	if _, err := target.Apply(update.pin()); err != nil {
		t.Fatalf("apply over an incomplete generation: %v", err)
	}
	if got := runBinary(t, target.Layout); got != versionOf(update) {
		t.Errorf("the host runs %q", got)
	}
}

// A current whose generation no longer verifies is pointed back at previous.
func TestRecoverRestoresPreviousWhenCurrentIsBroken(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	old, update := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b")
	managedOn(t, store, target, old)
	managedOn(t, store, target, update)
	if err := removeGeneration(target.Layout.Generation(update.sha())); err != nil {
		t.Fatal(err)
	}
	if err := target.Recover(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if target.Layout.CurrentGeneration() != old.sha() {
		t.Errorf("current is %q", target.Layout.CurrentGeneration())
	}
	if got := runBinary(t, target.Layout); got != versionOf(old) {
		t.Errorf("the host runs %q", got)
	}
	// With nothing to fall back on, recovery refuses rather than guessing.
	if err := removeGeneration(target.Layout.Generation(old.sha())); err != nil {
		t.Fatal(err)
	}
	if err := target.Recover(); err == nil {
		t.Error("recovery without any valid generation reported success")
	}
}

// Two changes never race: the second is refused while the first holds the lock.
func TestAConcurrentChangeIsRefused(t *testing.T) {
	store, server := serve(t)
	target := installer(t, server)
	update := newProgram("v0.1.31", "b")
	store.publish(update)
	if err := os.MkdirAll(target.Layout.Root(), 0o700); err != nil {
		t.Fatal(err)
	}
	handle, err := os.OpenFile(target.Layout.Lock(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()
	if err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	_, err = target.Apply(update.pin())
	if !failure.Is(err, failure.KindLock) {
		t.Fatalf("a concurrent apply produced %v", err)
	}
	if target.Layout.CurrentGeneration() != "" {
		t.Error("a refused apply switched")
	}
}

// The bootstrap path installs a binary already on disk, checked against the
// digest the operator expects.
func TestInstallFileChecksTheExpectedDigestAndVersion(t *testing.T) {
	_, server := serve(t)
	target := installer(t, server)
	update := newProgram("v0.1.31", "b")
	staged := filepath.Join(t.TempDir(), "upkeeper")
	if err := os.WriteFile(staged, update.body, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := target.InstallFile(staged, strings.Repeat("0", 64), versionOf(update)); err == nil ||
		!strings.Contains(err.Error(), "does not match the expected") {
		t.Fatalf("a wrong digest produced %v", err)
	}
	if _, err := target.InstallFile(staged, "ABC", versionOf(update)); err == nil {
		t.Fatal("a malformed digest was accepted")
	}
	if _, err := target.InstallFile(staged, update.sha(), "upkeeper v9 (commit x)"); err == nil {
		t.Fatal("a binary reporting another version was accepted")
	}
	if target.Layout.CurrentGeneration() != "" {
		t.Fatal("a refused install switched")
	}
	result, err := target.InstallFile(staged, update.sha(), versionOf(update))
	if err != nil || result.Generation != update.sha() {
		t.Fatalf("install %+v, %v", result, err)
	}
	if got := runBinary(t, target.Layout); got != versionOf(update) {
		t.Errorf("the host runs %q", got)
	}
	status := target.Status()
	if status["current"] != update.sha() || status["binary"].(map[string]any)["managed"] != true {
		t.Errorf("status %v", status)
	}
}

// The re-executed binary is told which commit was gated, through the hidden
// option, and runs from its generation rather than through the current link.
func TestReexecPassesAfterUpdate(t *testing.T) {
	var gotPath string
	var gotArgv []string
	recorder := func(path string, argv []string, env []string) error {
		gotPath, gotArgv = path, argv
		return errors.New("recorded")
	}
	commit := strings.Repeat("b", 40)
	path := "/home/x/.local/lib/upkeeper/generations/" + strings.Repeat("1", 64) + "/upkeeper"
	transaction := strings.Repeat("7", 32)
	if err := Reexec(recorder, path, commit, transaction); err == nil {
		t.Fatal("a failed exec was not reported")
	}
	if gotPath != path || !slices.Equal(gotArgv,
		[]string{path, "self-pull", "--after-update", commit, "--update-transaction", transaction}) {
		t.Errorf("exec %s %v", gotPath, gotArgv)
	}
	// An update that found its generation current already switched nothing.
	if err := Reexec(recorder, path, commit, ""); err == nil ||
		!slices.Equal(gotArgv, []string{path, "self-pull", "--after-update", commit}) {
		t.Errorf("exec without a switch %v", gotArgv)
	}
}

// The rollback of a failed update undoes its own switch, and refuses once
// anything else has changed the installation: an operator's rollback, a
// second rollback that restores the same pair, or another install.
func TestUndoSwitchUndoesOnlyItsOwnSwitch(t *testing.T) {
	first, second, third := newProgram("v0.1.30", "a"), newProgram("v0.1.31", "b"), newProgram("v0.1.32", "c")
	cases := []struct {
		name    string
		between func(t *testing.T, target *Installer, store *artifacts)
		current program
	}{
		{"nothing in between", func(*testing.T, *Installer, *artifacts) {}, first},
		{"an operator rollback", func(t *testing.T, target *Installer, _ *artifacts) {
			if _, err := target.Rollback("operator"); err != nil {
				t.Fatal(err)
			}
		}, first},
		{"two operator rollbacks", func(t *testing.T, target *Installer, _ *artifacts) {
			for range 2 {
				if _, err := target.Rollback("operator"); err != nil {
					t.Fatal(err)
				}
			}
		}, second},
		{"another install", func(t *testing.T, target *Installer, store *artifacts) {
			managedOn(t, store, target, third)
		}, third},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store, server := serve(t)
			target := installer(t, server)
			managedOn(t, store, target, first)
			store.publish(second)
			update, err := target.Apply(second.pin())
			if err != nil || update.Transaction == "" {
				t.Fatalf("apply %+v, %v", update, err)
			}
			test.between(t, target, store)
			_, err = target.UndoSwitch(update.Transaction, "re-exec failed")
			if test.name == "nothing in between" {
				if err != nil {
					t.Fatalf("undo: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "changed after this update") {
				t.Fatalf("undo after %s produced %v", test.name, err)
			} else if !failure.IsExpected(err) {
				t.Errorf("the refusal would exit 1: %v", err)
			}
			if got := runBinary(t, target.Layout); got != versionOf(test.current) {
				t.Errorf("the host runs %q, want %q", got, versionOf(test.current))
			}
		})
	}
	if _, err := (&Installer{Layout: Layout{Home: t.TempDir()}}).UndoSwitch("", "x"); err == nil {
		t.Error("an update that switched nothing rolled something back")
	}
}
