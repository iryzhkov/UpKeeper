package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
)

func holdTestHome(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home, filepath.Join(home, ".local", "state", "dev-fleet", "scheduled-holds")
}

func TestHoldShowListsNothingWhenNoMarkerExists(t *testing.T) {
	holdTestHome(t)
	code, out, errs := runMain(t, "hold", "show", "--json")
	if code != exitcode.Success {
		t.Fatalf("exit %d, stderr=%q", code, errs)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("not a JSON document: %v\n%s", err, out)
	}
	holds, ok := document["holds"].([]any)
	if !ok || len(holds) != 0 || document["command"] != "hold" {
		t.Fatalf("document is %#v", document)
	}
	code, out, _ = runMain(t, "hold", "show")
	if code != exitcode.Success || !strings.Contains(out, "no scheduled hold markers under") {
		t.Fatalf("exit %d, stdout=%q", code, out)
	}
}

func TestHoldShowDescribesEachMarker(t *testing.T) {
	_, directory := holdTestHome(t)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "Scheduled dispatch may have run (run 20260917T140905Z-6b2c83ab). Observe the host before clearing.\n"
	if err := os.WriteFile(filepath.Join(directory, "laptop.hold"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runMain(t, "hold", "show", "--json")
	if code != exitcode.Success {
		t.Fatalf("exit %d, stderr=%q", code, errs)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("not a JSON document: %v\n%s", err, out)
	}
	holds, _ := document["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("document is %#v", document)
	}
	marker, _ := holds[0].(map[string]any)
	if marker["host"] != "laptop" || marker["path"] != filepath.Join(directory, "laptop.hold") ||
		marker["content"] != content || marker["run_id"] != "20260917T140905Z-6b2c83ab" ||
		marker["age_seconds"] == nil {
		t.Fatalf("marker is %#v", marker)
	}
	code, out, _ = runMain(t, "hold", "show")
	if code != exitcode.Success || !strings.Contains(out, "laptop: ") ||
		!strings.Contains(out, "run 20260917T140905Z-6b2c83ab") ||
		!strings.Contains(out, "upkeeper hold clear laptop --reason") {
		t.Fatalf("exit %d, stdout=%q", code, out)
	}
}

func TestHoldClearNeedsAReasonAndAMarker(t *testing.T) {
	_, directory := holdTestHome(t)

	code, out, errs := runMain(t, "hold", "clear", "laptop")
	if code != exitcode.Usage || out != "" || !strings.Contains(errs, "--reason") {
		t.Fatalf("a clear without a reason: exit %d, stdout=%q, stderr=%q", code, out, errs)
	}

	code, _, errs = runMain(t, "hold", "clear", "laptop", "--reason", "looked at it")
	if code != exitcode.Usage || !strings.Contains(errs, "no hold marker for laptop") {
		t.Fatalf("a clear with nothing to do: exit %d, stderr=%q", code, errs)
	}

	code, _, errs = runMain(t, "hold", "clear", "../etc", "--reason", "looked at it")
	if code != exitcode.Usage || !strings.Contains(errs, "invalid host") {
		t.Fatalf("a clear of a non-host: exit %d, stderr=%q", code, errs)
	}

	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "laptop.hold")
	if err := os.WriteFile(marker, []byte("Scheduled dispatch may have run (run 20260917T140905Z-6b2c83ab).\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = runMain(t, "hold", "clear", "laptop", "--reason", "T3 reinstalled", "--json")
	if code != exitcode.Success {
		t.Fatalf("exit %d, stderr=%q", code, errs)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("not a JSON document: %v\n%s", err, out)
	}
	if document["host"] != "laptop" || document["reason"] != "T3 reinstalled" ||
		document["run_id"] != "20260917T140905Z-6b2c83ab" || document["cleared_log"] != marker+".cleared" {
		t.Fatalf("document is %#v", document)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the marker survived the clear")
	}
	logged, err := os.ReadFile(marker + ".cleared")
	if err != nil || !strings.Contains(string(logged), "T3 reinstalled") {
		t.Fatalf("the reason was not recorded: %v %q", err, logged)
	}
}
