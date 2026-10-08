package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// statusObservation is a fleet as status sees it: homelab's t3 drifts on
// purpose, normandy failed a component and laptop could not be observed.
func statusObservation() map[string]any {
	failed := statusTestHost("normandy", "t3-steward", "failed", "huyang", "converged")
	failed["result"] = "failed"
	converged := statusTestHost("omarchy-pc", "huyang", "converged")
	converged["result"] = "converged"
	return map[string]any{"controller": "omarchy-pc", "hosts": []any{
		converged,
		statusTestHost("homelab", "t3", "drift", "huyang", "ahead"),
		failed,
		map[string]any{"host": "laptop", "reachable": false, "result": "offline"},
	}}
}

func writeExpectations(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "expect.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var statusKnownHosts = map[string]bool{"omarchy-pc": true, "homelab": true, "normandy": true, "laptop": true}

// Without an expectations file status keeps the contract it always had.
func TestStatusWithoutExpectationsIsUnchanged(t *testing.T) {
	document := statusObservation()
	code, result := classifyStatus(document, nil)
	if code != 20 || result != "partial" {
		t.Fatalf("code=%d result=%s", code, result)
	}
	if _, present := document["accepted"]; present {
		t.Error("a run without --expect reports acceptances")
	}
	laptop := document["hosts"].([]any)[3].(map[string]any)
	if laptop["result"] != "unavailable" || laptop["name"] != "laptop" {
		t.Errorf("laptop = %#v", laptop)
	}
	drift := map[string]any{"hosts": []any{statusTestHost("homelab", "t3", "drift")}}
	if code, result := classifyStatus(drift, nil); code != 10 || result != "drift" {
		t.Errorf("drift only: code=%d result=%s", code, result)
	}
}

// Every known state accepted: status passes and says what it accepted.
func TestStatusAcceptsEveryListedStateAndGatesOnTheRest(t *testing.T) {
	path := writeExpectations(t, `{"schema_version": 1, "accepted": [
		{"host": "homelab", "component": "t3", "state": "drift", "reason": "T3 0.0.40 held back on homelab"},
		{"host": "normandy", "component": "t3-steward", "state": "failed", "reason": "known range refusal"},
		{"host": "laptop", "state": "unavailable", "reason": "asleep"},
		{"host": "omarchy-pc", "component": "t3", "state": "drift", "reason": "stale entry"}
	]}`)
	expectations, err := loadStatusExpectations(path, statusKnownHosts)
	if err != nil {
		t.Fatal(err)
	}
	document := statusObservation()
	code, result := classifyStatus(document, expectations)
	if code != 0 || result != "converged" {
		t.Fatalf("code=%d result=%s", code, result)
	}
	if accepted := document["accepted"].([]any); len(accepted) != 3 {
		t.Errorf("accepted = %#v", accepted)
	}
	unused := document["expectations_unused"].([]any)
	if len(unused) != 1 || unused[0].(map[string]any)["host"] != "omarchy-pc" {
		t.Errorf("unused = %#v", unused)
	}
	document["result"], document["exit_code"] = result, code
	var out bytes.Buffer
	printStatus(&out, document)
	for _, want := range []string{
		"t3: accepted: T3 0.0.40 held back on homelab",
		"accepted: asleep",
		"expectation matched nothing: omarchy-pc t3 drift (stale entry)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, out.String())
		}
	}
}

// What the file does not name still fails, and a state must match exactly:
// an expected drift does not excuse a failure of the same component.
func TestStatusFailsOnAnythingTheFileDoesNotAccept(t *testing.T) {
	path := writeExpectations(t, `{"schema_version": 1, "accepted": [
		{"host": "homelab", "component": "t3", "state": "drift", "reason": "held back"},
		{"host": "normandy", "component": "t3-steward", "state": "drift", "reason": "wrong state"}
	]}`)
	expectations, err := loadStatusExpectations(path, statusKnownHosts)
	if err != nil {
		t.Fatal(err)
	}
	document := statusObservation()
	if code, result := classifyStatus(document, expectations); code != 20 || result != "partial" {
		t.Fatalf("code=%d result=%s", code, result)
	}

	// A second unexpected drifting component on an otherwise accepted host
	// fails the host: acceptance is per state, never per host by accident.
	drift := map[string]any{"hosts": []any{statusTestHost("homelab", "t3", "drift", "huyang", "drift")}}
	expectations, _ = loadStatusExpectations(path, statusKnownHosts)
	if code, result := classifyStatus(drift, expectations); code != 10 || result != "drift" {
		t.Errorf("partly accepted host: code=%d result=%s", code, result)
	}

	// A failed host with no failed component is a host-level failure and
	// needs a host-level expectation.
	hostLevel := statusTestHost("normandy")
	hostLevel["result"] = "failed"
	expectations, _ = loadStatusExpectations(path, statusKnownHosts)
	if code, _ := classifyStatus(map[string]any{"hosts": []any{hostLevel}}, expectations); code != 20 {
		t.Errorf("host-level failure accepted: code=%d", code)
	}
}

func TestAnExpectationsFileIsCheckedBeforeItAcceptsAnything(t *testing.T) {
	for name, content := range map[string]string{
		"not json":          `{`,
		"wrong schema":      `{"schema_version": 2, "accepted": []}`,
		"unknown field":     `{"schema_version": 1, "accepted": [], "extra": true}`,
		"unknown host":      `{"schema_version": 1, "accepted": [{"host": "nosuch", "state": "drift", "reason": "x"}]}`,
		"unknown component": `{"schema_version": 1, "accepted": [{"host": "homelab", "component": "tea", "state": "drift", "reason": "x"}]}`,
		"converged state":   `{"schema_version": 1, "accepted": [{"host": "homelab", "state": "converged", "reason": "x"}]}`,
		"no reason":         `{"schema_version": 1, "accepted": [{"host": "homelab", "state": "drift", "reason": " "}]}`,
		"duplicate": `{"schema_version": 1, "accepted": [{"host": "homelab", "state": "drift", "reason": "x"},` +
			`{"host": "homelab", "state": "drift", "reason": "y"}]}`,
	} {
		if _, err := loadStatusExpectations(writeExpectations(t, content), statusKnownHosts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := loadStatusExpectations(filepath.Join(t.TempDir(), "missing.json"), statusKnownHosts); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestOnlyStatusTakesAnExpectationsFile(t *testing.T) {
	parsed, err := parse([]string{"status", "--expect", "/tmp/expect.json", "--json"}, nil)
	if err != nil || parsed.expectFile != "/tmp/expect.json" {
		t.Fatalf("status --expect = %+v %v", parsed, err)
	}
	if _, err := parse([]string{"doctor", "--expect", "/tmp/expect.json"}, nil); err == nil {
		t.Error("doctor accepted --expect")
	}
}
