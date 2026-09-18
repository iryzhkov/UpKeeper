package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
)

func runMain(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	var out, errors bytes.Buffer
	code := Main("upkeeper", argv, Streams{In: strings.NewReader(""), Out: &out, Err: &errors})
	return code, out.String(), errors.String()
}

func TestAMalformedCommandLineExitsTwo(t *testing.T) {
	code, out, errors := runMain(t, "doctor", "--verbose")
	if code != exitcode.Usage {
		t.Errorf("exit code is %d, expected %d", code, exitcode.Usage)
	}
	if out != "" {
		t.Errorf("a usage error wrote to standard output: %q", out)
	}
	if !strings.Contains(errors, "unrecognized arguments: --verbose") {
		t.Errorf("standard error is %q", errors)
	}
}

func TestVersionNamesItsSourceCommit(t *testing.T) {
	code, out, _ := runMain(t, "version")
	if code != exitcode.Success {
		t.Errorf("exit code is %d", code)
	}
	if !strings.HasPrefix(out, "upkeeper ") || !strings.Contains(out, "commit ") {
		// A release artifact that cannot name its own commit is not auditable.
		t.Errorf("version output is %q", out)
	}
}

func TestAnAbsentConfigurationIsAPreflightRefusal(t *testing.T) {
	empty := t.TempDir()
	t.Setenv(ConfigRootEnvironment, empty)
	t.Setenv("HOME", empty)
	t.Chdir(empty)
	code, _, errors := runMain(t, "doctor")
	if code != exitcode.Preflight {
		t.Errorf("exit code is %d, expected %d", code, exitcode.Preflight)
	}
	if !strings.Contains(errors, "fleet configuration unavailable") {
		t.Errorf("standard error is %q", errors)
	}
}

func TestARefusalIsReportedAsJSONWhenJSONWasAsked(t *testing.T) {
	empty := t.TempDir()
	t.Setenv(ConfigRootEnvironment, empty)
	t.Setenv("HOME", empty)
	t.Chdir(empty)
	code, out, _ := runMain(t, "doctor", "--json")
	if code != exitcode.Preflight {
		t.Errorf("exit code is %d", code)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("the refusal was not a JSON document: %v\n%s", err, out)
	}
	if document["result"] != "invalid-input" ||
		document["command"] != "doctor" ||
		document["exit_code"] != float64(exitcode.Preflight) ||
		document["schema_version"] != float64(exitcode.SchemaVersion) {
		t.Errorf("the refusal document is %#v", document)
	}
}

func TestConfigRootPrefersTheOperatorOverrideAndRefusesARelativeOne(t *testing.T) {
	root := t.TempDir()
	writeConfiguration(t, root)
	t.Setenv(ConfigRootEnvironment, root)
	resolved, err := ConfigRoot()
	if err != nil {
		t.Fatalf("ConfigRoot: %v", err)
	}
	if resolved != root {
		t.Errorf("resolved %q, expected %q", resolved, root)
	}
	t.Setenv(ConfigRootEnvironment, "relative/path")
	if _, err := ConfigRoot(); err == nil ||
		!strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("produced %v", err)
	}
}

func TestCheckoutRootInsistsOnAGitCheckout(t *testing.T) {
	root := t.TempDir()
	writeConfiguration(t, root)
	t.Setenv(ConfigRootEnvironment, root)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(root)
	// Release history is what --self and rollback read, so a directory that
	// merely holds the files is not enough.
	if _, err := CheckoutRoot(); err == nil {
		t.Fatal("a checkout-less directory was accepted")
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := CheckoutRoot(); err != nil {
		t.Fatalf("CheckoutRoot: %v", err)
	}
}

// An unattended run's stdout stays one JSON document; the failures it found
// are repeated on stderr, one line each, untruncated, with the run record.
func TestScheduledFailureSummaryNamesEachFailureOnStderr(t *testing.T) {
	var errs bytes.Buffer
	document := map[string]any{
		"run_id": "20260917T140905Z-6b2c83ab", "exit_code": 40,
		"hosts": []any{
			map[string]any{
				"host": "laptop", "reachable": true, "result": "failed",
				"components": []any{
					map[string]any{"name": "huyang", "state": "converged"},
					map[string]any{
						"name": "t3-steward", "state": "failed",
						"error": "running T3 binary missing on disk: /home/igor/.local/share/mise/shims/t3 " +
							"(installed 0.0.38 from the manifest); restart T3 after reinstalling, then retry\n" +
							"second line of detail",
					},
				},
			},
			map[string]any{
				"host": "homelab", "reachable": true, "result": "failed",
				"error": "scheduled hold requires operator observation: /home/igor/.local/state/dev-fleet/scheduled-holds/homelab.hold",
			},
			map[string]any{"host": "normandy", "reachable": true, "result": "converged",
				"components": []any{map[string]any{"name": "huyang", "state": "converged"}}},
		},
	}
	printScheduledSummary(&errs, "self-pull", document,
		"/home/igor/.local/state/dev-fleet/runs/20260917T140905Z-6b2c83ab.json", 40)
	expected := strings.Join([]string{
		"upkeeper self-pull: laptop t3-steward failed: running T3 binary missing on disk: " +
			"/home/igor/.local/share/mise/shims/t3 (installed 0.0.38 from the manifest); " +
			"restart T3 after reinstalling, then retry",
		"upkeeper self-pull: homelab failed: scheduled hold requires operator observation: " +
			"/home/igor/.local/state/dev-fleet/scheduled-holds/homelab.hold",
		"upkeeper self-pull: run 20260917T140905Z-6b2c83ab recorded at " +
			"/home/igor/.local/state/dev-fleet/runs/20260917T140905Z-6b2c83ab.json; exit 40",
		"",
	}, "\n")
	if errs.String() != expected {
		t.Errorf("produced\n%s\nexpected\n%s", errs.String(), expected)
	}

	// A quiet run is not news on stderr either.
	errs.Reset()
	printScheduledSummary(&errs, "scheduled-pull", map[string]any{
		"run_id": "20260917T140905Z-6b2c83ab", "exit_code": 0,
		"hosts": []any{map[string]any{"host": "normandy", "reachable": true, "result": "converged"}},
	}, "/runs/x.json", 0)
	if errs.String() != "" {
		t.Errorf("a clean run wrote %q to stderr", errs.String())
	}
}

// The real self-pull path: a refusal before dispatch keeps stdout as exactly
// one JSON document and explains itself on stderr.
func TestSelfPullRefusalKeepsStdoutJSONAndSummarisesOnStderr(t *testing.T) {
	empty := t.TempDir()
	t.Setenv(ConfigRootEnvironment, empty)
	t.Setenv("HOME", empty)
	t.Chdir(empty)
	code, out, errs := runMain(t, "self-pull", "--dry-run")
	if code != exitcode.Preflight {
		t.Fatalf("exit %d", code)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	if document["command"] != "self-schedule" || document["exit_code"] != float64(exitcode.Preflight) {
		t.Errorf("document is %#v", document)
	}
	if !strings.HasPrefix(errs, "upkeeper self-pull: failed before dispatch: no UpKeeper release checkout found") ||
		!strings.Contains(errs, "exit 30") {
		t.Errorf("stderr is %q", errs)
	}
}

func writeConfiguration(t *testing.T, root string) {
	t.Helper()
	for _, entry := range []struct{ path, content string }{
		{filepath.Join(root, "inventory", "hosts.yml"), "{}"},
		{filepath.Join(root, "desired", "components.yml"), "{}"},
	} {
		if err := os.MkdirAll(filepath.Dir(entry.path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(entry.path, []byte(entry.content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
}
