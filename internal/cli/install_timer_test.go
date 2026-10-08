package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/units"
)

func reconciledInstallation() units.Installation {
	return units.Installation{
		Written: []string{"/h/.config/systemd/user/upkeeper-self.service",
			"/h/.config/systemd/user/upkeeper-self.timer"},
		Removed: []string{"/h/.config/systemd/user/upkeeper-self.service.d/10-upkeeper-binary.conf",
			"/h/.config/systemd/user/upkeeper-self.service.d"},
	}
}

func decodeReport(t *testing.T, encoded []byte) map[string]any {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatalf("not JSON: %v: %s", err, encoded)
	}
	return report
}

// An operator reconciling a host with a legacy canary drop-in sees in the
// output that the drop-in is gone, in text and in JSON.
func TestInstallTimerReportsTheRemovedLegacyDropIn(t *testing.T) {
	done := reconciledInstallation()

	var text, errs bytes.Buffer
	code, err := reportInstallation(Streams{Out: &text, Err: &errs}, "install-self-timer", done, false, nil)
	if err != nil || code != exitcode.Success {
		t.Fatalf("code=%d err=%v", code, err)
	}
	want := "installed /h/.config/systemd/user/upkeeper-self.service\n" +
		"installed /h/.config/systemd/user/upkeeper-self.timer\n" +
		"removed legacy drop-in /h/.config/systemd/user/upkeeper-self.service.d/10-upkeeper-binary.conf\n" +
		"removed legacy drop-in /h/.config/systemd/user/upkeeper-self.service.d\n"
	if text.String() != want {
		t.Errorf("text output:\n%s\nwant:\n%s", text.String(), want)
	}
	if errs.Len() != 0 {
		t.Errorf("a successful install wrote to stderr: %q", errs.String())
	}

	var encoded bytes.Buffer
	if _, err := reportInstallation(Streams{Out: &encoded, Err: &errs}, "install-self-timer", done, true, nil); err != nil {
		t.Fatal(err)
	}
	report := decodeReport(t, encoded.Bytes())
	if report["command"] != "install-self-timer" || report["exit_code"] != float64(0) {
		t.Errorf("unexpected report: %v", report)
	}
	if _, present := report["error"]; present {
		t.Errorf("a successful report carries an error: %v", report)
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
	var encoded, errs bytes.Buffer
	if _, err := reportInstallation(Streams{Out: &encoded, Err: &errs}, "install-pull-timer",
		units.Installation{Written: []string{"/u"}}, true, nil); err != nil {
		t.Fatal(err)
	}
	if removed, ok := decodeReport(t, encoded.Bytes())["removed"].([]any); !ok || len(removed) != 0 {
		t.Errorf("removed is not an empty list: %s", encoded.String())
	}
}

// A systemctl failure after the drop-in was removed is partial, and the report
// still names the removal: a rerun no longer finds the drop-in, so this run is
// the only place the operator can learn it went.
func TestAFailureAfterTheRemovalIsReportedAsPartial(t *testing.T) {
	done := reconciledInstallation()
	failed := errors.New("daemon-reload failed: Failed to connect to bus")

	var text, errs bytes.Buffer
	code, err := reportInstallation(Streams{Out: &text, Err: &errs}, "install-self-timer", done, false, failed)
	if err != nil || code != exitcode.Partial {
		t.Fatalf("code=%d err=%v, want partial", code, err)
	}
	if !strings.Contains(text.String(), "removed legacy drop-in "+done.Removed[0]) {
		t.Errorf("the removal is not reported: %q", text.String())
	}
	if !strings.Contains(errs.String(), "daemon-reload failed") {
		t.Errorf("the failure is not reported: %q", errs.String())
	}

	var encoded bytes.Buffer
	code, err = reportInstallation(Streams{Out: &encoded, Err: &errs}, "install-self-timer", done, true, failed)
	if err != nil || code != exitcode.Partial {
		t.Fatalf("code=%d err=%v, want partial", code, err)
	}
	report := decodeReport(t, encoded.Bytes())
	if report["result"] != "partial" || report["exit_code"] != float64(exitcode.Partial) ||
		!strings.Contains(report["error"].(string), "daemon-reload failed") {
		t.Errorf("unexpected report: %v", report)
	}
	if !reflect.DeepEqual(report["removed"], []any{done.Removed[0], done.Removed[1]}) {
		t.Errorf("removed = %v, want %q", report["removed"], done.Removed)
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
