package operatorhold_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/operatorhold"
)

var setAt = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

func logLines(t *testing.T, directory, host string) []string {
	t.Helper()
	raw, err := os.ReadFile(operatorhold.LogPath(directory, host))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// A hold is state on disk and nothing else: a fresh reader that knows only the
// directory, which is what a process started after a reboot is, finds it.
func TestAHoldSurvivesARestartOfEveryReader(t *testing.T) {
	home := t.TempDir()
	directory := operatorhold.Directory(home)
	if !strings.HasPrefix(directory, filepath.Join(home, ".local", "state")) {
		t.Fatalf("holds live outside persistent state: %s", directory)
	}
	if _, err := operatorhold.Set(directory, operatorhold.Hold{
		Host: "homelab", Reason: "rc120 canary on omarchy-pc first", SetBy: "igor", SetAt: setAt,
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(operatorhold.RecordPath(directory, "homelab"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("record mode %v", info.Mode().Perm())
	}

	// Simulated reboot: no value survives except the path of the home.
	afterReboot := operatorhold.Directory(home)
	hold, held := operatorhold.Check(afterReboot, "homelab", setAt.Add(72*time.Hour))
	if !held {
		t.Fatal("the hold did not survive a restart")
	}
	if hold.Reason != "rc120 canary on omarchy-pc first" || hold.SetBy != "igor" || !hold.SetAt.Equal(setAt) {
		t.Errorf("hold after restart = %#v", hold)
	}
	if _, held := operatorhold.Check(afterReboot, "laptop", setAt); held {
		t.Error("a host nobody held is held")
	}
}

func TestAHoldWithAnEndStopsApplyingAndIsStillListed(t *testing.T) {
	directory := operatorhold.Directory(t.TempDir())
	if _, err := operatorhold.Set(directory, operatorhold.Hold{
		Host: "normandy", Reason: "window", SetBy: "igor", SetAt: setAt, Until: setAt.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, held := operatorhold.Check(directory, "normandy", setAt.Add(59*time.Minute)); !held {
		t.Error("a hold inside its window does not apply")
	}
	if _, held := operatorhold.Check(directory, "normandy", setAt.Add(time.Hour)); held {
		t.Error("an expired hold still applies")
	}
	holds, err := operatorhold.List(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds[0].Active(setAt.Add(2*time.Hour)) {
		t.Fatalf("listing = %#v", holds)
	}
	document := holds[0].Document(setAt.Add(2 * time.Hour))
	if document["until"] != "2026-10-08T10:00:00Z" || document["active"] != false || document["set_by"] != "igor" {
		t.Errorf("document = %#v", document)
	}
}

func TestSetAndClearAreLoggedAndRefuseBadInput(t *testing.T) {
	directory := operatorhold.Directory(t.TempDir())
	for _, bad := range []operatorhold.Hold{
		{Host: "../etc", Reason: "x", SetAt: setAt},
		{Host: "laptop", Reason: "  ", SetAt: setAt},
		{Host: "laptop", Reason: "x"},
		{Host: "laptop", Reason: "x", SetAt: setAt, Until: setAt},
	} {
		if _, err := operatorhold.Set(directory, bad); err == nil {
			t.Errorf("accepted %#v", bad)
		}
	}
	if _, err := os.Stat(directory); err == nil {
		t.Error("a refused set created state")
	}
	if _, err := operatorhold.Set(directory, operatorhold.Hold{Host: "laptop", Reason: "first", SetBy: "igor", SetAt: setAt}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := operatorhold.Clear(directory, "laptop", "", "igor", setAt); err == nil {
		t.Error("a clear without a reason was accepted")
	}
	hold, cleared, err := operatorhold.Clear(directory, "laptop", "release done", "igor", setAt.Add(time.Hour))
	if err != nil || !cleared || hold.Reason != "first" {
		t.Fatalf("clear = %#v %v %v", hold, cleared, err)
	}
	if _, held := operatorhold.Check(directory, "laptop", setAt); held {
		t.Error("a cleared hold still applies")
	}
	if _, cleared, err := operatorhold.Clear(directory, "laptop", "again", "igor", setAt); err != nil || cleared {
		t.Errorf("clearing nothing = %v %v", cleared, err)
	}
	if err := operatorhold.RecordOverride(directory, hold, "igor", "pull", "run-1", setAt); err != nil {
		t.Fatal(err)
	}
	lines := logLines(t, directory, "laptop")
	if len(lines) != 3 || !strings.Contains(lines[0], `"event":"set"`) ||
		!strings.Contains(lines[1], `"event":"clear"`) || !strings.Contains(lines[1], `"reason":"release done"`) ||
		!strings.Contains(lines[2], `"event":"override"`) || !strings.Contains(lines[2], `"run_id":"run-1"`) {
		t.Errorf("log = %q", lines)
	}
}

// A record nobody can read is still an instruction to wait.
func TestAnUnreadableHoldFailsClosedAndCanBeCleared(t *testing.T) {
	directory := operatorhold.Directory(t.TempDir())
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operatorhold.RecordPath(directory, "homelab"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	hold, held := operatorhold.Check(directory, "homelab", setAt)
	if !held || !strings.Contains(hold.Reason, "not a hold record") || !strings.Contains(hold.Reason, "hold clear --hosts homelab") {
		t.Fatalf("unreadable hold = %#v %v", hold, held)
	}
	if _, cleared, err := operatorhold.Clear(directory, "homelab", "replaced by hand", "igor", setAt); err != nil || !cleared {
		t.Fatalf("clear = %v %v", cleared, err)
	}
	if _, held := operatorhold.Check(directory, "homelab", setAt); held {
		t.Error("the unreadable record survived its clear")
	}
}
