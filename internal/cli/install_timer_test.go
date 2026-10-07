package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/units"
)

// An operator reconciling a host with a legacy canary drop-in sees in the
// output that the drop-in is gone, in text and in JSON.
func TestInstallTimerReportsTheRemovedLegacyDropIn(t *testing.T) {
	done := units.Installation{
		Written: []string{"/h/.config/systemd/user/upkeeper-self.service",
			"/h/.config/systemd/user/upkeeper-self.timer"},
		Removed: []string{"/h/.config/systemd/user/upkeeper-self.service.d/10-upkeeper-binary.conf",
			"/h/.config/systemd/user/upkeeper-self.service.d"},
	}

	var text bytes.Buffer
	if err := printInstallation(&text, "install-self-timer", done, false); err != nil {
		t.Fatal(err)
	}
	want := "installed /h/.config/systemd/user/upkeeper-self.service\n" +
		"installed /h/.config/systemd/user/upkeeper-self.timer\n" +
		"removed legacy drop-in /h/.config/systemd/user/upkeeper-self.service.d/10-upkeeper-binary.conf\n" +
		"removed legacy drop-in /h/.config/systemd/user/upkeeper-self.service.d\n"
	if text.String() != want {
		t.Errorf("text output:\n%s\nwant:\n%s", text.String(), want)
	}

	var encoded bytes.Buffer
	if err := printInstallation(&encoded, "install-self-timer", done, true); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &report); err != nil {
		t.Fatalf("not JSON: %v: %s", err, encoded.String())
	}
	if report["command"] != "install-self-timer" || report["exit_code"] != float64(0) {
		t.Errorf("unexpected report: %v", report)
	}
	if !reflect.DeepEqual(report["removed"], []any{done.Removed[0], done.Removed[1]}) {
		t.Errorf("removed = %v, want %q", report["removed"], done.Removed)
	}
	if !reflect.DeepEqual(report["installed"], []any{done.Written[0], done.Written[1]}) {
		t.Errorf("installed = %v, want %q", report["installed"], done.Written)
	}
}

// Nothing removed is still an empty list, so a reader can tell "nothing" from
// "not reported".
func TestInstallTimerJSONKeepsAnEmptyRemovedList(t *testing.T) {
	var encoded bytes.Buffer
	if err := printInstallation(&encoded, "install-pull-timer",
		units.Installation{Written: []string{"/u"}}, true); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &report); err != nil {
		t.Fatalf("not JSON: %v: %s", err, encoded.String())
	}
	if removed, ok := report["removed"].([]any); !ok || len(removed) != 0 {
		t.Errorf("removed is not an empty list: %s", encoded.String())
	}
}

func TestInstallTimerCommandsAcceptOnlyJSON(t *testing.T) {
	for _, command := range []string{"install-self-timer", "install-pull-timer"} {
		parsed, err := parse([]string{command, "--json"}, release.ComponentOrder)
		if err != nil || !parsed.asJSON {
			t.Errorf("%s --json: parsed=%v err=%v", command, parsed, err)
		}
		if _, err := parse([]string{command, "--dry-run"}, release.ComponentOrder); err == nil ||
			!strings.Contains(err.Error(), "unrecognized arguments: --dry-run") {
			t.Errorf("%s --dry-run was not refused: %v", command, err)
		}
	}
}
