package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/cli"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func copyInto(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", destination, err)
	}
}

// fleetConfigRoot builds a configuration root out of this repository's own
// inventory and release, with the shared fleet intent authored into it.
func fleetConfigRoot(t *testing.T, authored bool) string {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join("..", "..")
	copyInto(t, filepath.Join(repository, "inventory", "hosts.yml"),
		filepath.Join(root, "inventory", "hosts.yml"))
	copyInto(t, filepath.Join(repository, "desired", "components.yml"),
		filepath.Join(root, "desired", "components.yml"))

	raw, err := os.ReadFile(filepath.Join(repository, release.File))
	if err != nil {
		t.Skipf("no published manifest in this checkout: %v", err)
	}
	manifest, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	if authored {
		intent, err := os.ReadFile(
			filepath.Join(repository, "testdata", "fleet", "fleet-intent.json"))
		if err != nil {
			t.Fatalf("intent fixture: %v", err)
		}
		document, err := canonjson.DecodeObject(intent)
		if err != nil {
			t.Fatalf("decode the intent: %v", err)
		}
		components, _ := manifest["components"].(map[string]any)
		components[fleet.Component] = document
	}
	encoded, err := canonjson.MarshalFile(manifest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, release.File), encoded, 0o644); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
	return root
}

// snapshot records every file under a tree with its content, so that a command
// documented as writing nothing can be held to it.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return files
}

func TestFleetPlanReportsAndWritesNothing(t *testing.T) {
	root := fleetConfigRoot(t, true)
	t.Setenv(cli.ConfigRootEnvironment, root)
	before := snapshot(t, root)

	var out, errs bytes.Buffer
	code := cli.Main("upkeeper", []string{"fleet-plan", "--json"},
		cli.Streams{Out: &out, Err: &errs})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, errs.String())
	}
	report, err := canonjson.DecodeObject(out.Bytes())
	if err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out.String())
	}
	if report["command"] != "fleet-plan" {
		t.Errorf("command is %v", report["command"])
	}
	plan, _ := report["enrollment_plan"].(map[string]any)
	if plan["applied"] != false {
		t.Errorf("the plan reports applied=%v", plan["applied"])
	}
	changes, ok := plan["authorization_changes"].([]any)
	if !ok || len(changes) == 0 {
		t.Fatalf("the rendered enrollment diff is not reviewable: %#v", plan["authorization_changes"])
	}
	var opus bool
	for _, change := range changes {
		value, ok := change.(string)
		if !ok {
			t.Fatalf("a rendered authorization change is not text: %#v", change)
		}
		opus = opus || strings.Contains(value, "claude-opus-4-5")
	}
	if !opus {
		t.Fatalf("the rendered diff hides the proposed Opus authorization: %#v", changes)
	}
	// A plan carries no digest that could be mistaken for the revision the
	// coordinator fences an enrollment against; it names the command that
	// yields the real one instead.
	if _, present := plan["proposed_catalog_sha256"]; present {
		t.Error("the plan carries a digest no coordinator would accept")
	}
	if advice, _ := plan["fence"].(string); !strings.Contains(
		advice, "worker enroll --catalog-revision") {
		t.Errorf("the plan does not name the command that yields the fence: %q", advice)
	}
	artifact, _ := report["coordinator_projection"].(map[string]any)
	if purpose, _ := artifact["purpose"].(string); !strings.Contains(purpose, "authored coordinator configuration") {
		t.Errorf("the coordinator projection is not labelled a review artifact: %q", purpose)
	}
	hosts, _ := report["hosts"].(map[string]any)
	homelab, _ := hosts["homelab"].(map[string]any)
	if homelab["desired_models"] == nil {
		t.Error("the report carries no desired model set")
	}
	if observed, present := homelab["observed_models"]; !present || observed != nil {
		t.Error("an unobserved host must report its observation as absent")
	}

	after := snapshot(t, root)
	if len(after) != len(before) {
		t.Fatalf("fleet-plan changed the file set: %d before, %d after", len(before), len(after))
	}
	for path, content := range before {
		if after[path] != content {
			t.Errorf("fleet-plan rewrote %s", path)
		}
	}
}

func TestFleetPlanRefusesAReleaseWithoutTheComponent(t *testing.T) {
	root := fleetConfigRoot(t, false)
	t.Setenv(cli.ConfigRootEnvironment, root)
	var out, errs bytes.Buffer
	code := cli.Main("upkeeper", []string{"fleet-plan"}, cli.Streams{Out: &out, Err: &errs})
	if code != 30 {
		t.Fatalf("exit %d, want 30; stderr=%q", code, errs.String())
	}
	if !strings.Contains(errs.String(), "carries no "+fleet.Component) {
		t.Errorf("stderr is %q", errs.String())
	}
}

func TestFleetPlanTextOutputSeparatesDesiredFromUnobserved(t *testing.T) {
	root := fleetConfigRoot(t, true)
	t.Setenv(cli.ConfigRootEnvironment, root)
	var out, errs bytes.Buffer
	if code := cli.Main("upkeeper", []string{"fleet-plan"},
		cli.Streams{Out: &out, Err: &errs}); code != 0 {
		t.Fatalf("exit code, stderr=%q", errs.String())
	}
	for _, expected := range []string{
		"coordinator normandy-coordinator on normandy",
		"desired  claudeAgent=",
		"observed not observed",
		"not enrolled; applying this is an explicit Steward operation",
	} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("the report does not say %q:\n%s", expected, out.String())
		}
	}
}
