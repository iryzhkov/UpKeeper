package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// fleetTestHost is the host the shared fixture describes that these tests
// pretend to be.
const fleetTestHost = "homelab"

func fleetTestFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fleet", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return data
}

func fleetTestComponent(t *testing.T) map[string]any {
	t.Helper()
	document, err := canonjson.DecodeObject(fleetTestFixture(t, "fleet-intent.json"))
	if err != nil {
		t.Fatalf("decode the intent fixture: %v", err)
	}
	return document
}

// fleetTestPrepare gives the context this host's identity and both of its
// credentials. The values are never read by the adapter, only their metadata.
func fleetTestPrepare(t *testing.T) *Context {
	t.Helper()
	context := adapterTestContext(t, &adapterTestRunner{})
	context.Host.Name = fleetTestHost
	for _, reference := range []string{
		"f02-protocol/" + fleetTestHost,
		"f03-admin/" + fleetTestHost,
	} {
		path := context.Path(workerSecretRoot, reference)
		adapterTestWrite(t, path, "credential-not-for-records")
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return context
}

func fleetTestPriorPath(context *Context, runID string) string {
	return context.Path(fleet.StateDirectory, runID, "prior.json")
}

func TestFleetProjectionWithoutAnEntryForThisHost(t *testing.T) {
	context := fleetTestPrepare(t)
	context.Host.Name = "another-host"
	spec := fleetTestComponent(t)
	observed, err := fleetObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if observed["state"] != "converged" || observed["configured"] != false {
		t.Fatalf("observation = %v", observed)
	}
	actions := []string{}
	state, err := fleetApply(context, spec, observed, &actions)
	if err != nil || state != "converged" || len(actions) != 0 {
		t.Fatalf("state = %q, error = %v, actions = %v", state, err, actions)
	}
	if _, err := os.Stat(context.Path(fleet.WorkerDestination)); err == nil {
		t.Fatal("a host the fleet does not declare was given a bootstrap file")
	}
}

func TestFleetProjectionStagesActivatesAndRetainsThePrevious(t *testing.T) {
	context := fleetTestPrepare(t)
	spec := fleetTestComponent(t)

	preview, err := fleetObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if preview["state"] != "updated" || preview["enrolled"] != false {
		t.Fatalf("preview = %v", preview)
	}
	if _, err := os.Stat(context.Path(fleet.WorkerDestination)); err == nil {
		t.Fatal("an observation wrote the bootstrap file")
	}

	actions := []string{}
	state, err := fleetApply(context, spec, preview, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	written, err := os.ReadFile(context.Path(fleet.WorkerDestination))
	if err != nil {
		t.Fatalf("bootstrap file: %v", err)
	}
	expected := fleetTestFixture(t, "projection-worker-"+fleetTestHost+".json")
	if string(written) != string(expected) {
		t.Errorf("the activated document is not the shared projection:\n%s", written)
	}
	info, err := os.Stat(context.Path(fleet.WorkerDestination))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v (%v), want 0600", info.Mode().Perm(), err)
	}
	record, err := os.ReadFile(fleetTestPriorPath(context, context.RunID))
	if err != nil || !strings.Contains(string(record), `"existed": false`) {
		t.Fatalf("retained record = %q (%v)", record, err)
	}

	// Nothing is left staged, and a second run over the same intent changes
	// nothing and takes no second backup.
	entries, err := os.ReadDir(filepath.Dir(context.Path(fleet.WorkerDestination)))
	if err != nil {
		t.Fatalf("read the destination directory: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the destination directory holds %d entries, want only the document", len(entries))
	}
	context.RunID = "20260914T000001Z-second"
	second := []string{}
	state, err = fleetApply(context, spec, preview, &second)
	if err != nil || state != "converged" {
		t.Fatalf("state = %q, error = %v, want converged", state, err)
	}
	if _, err := os.Stat(context.Path(fleet.StateDirectory, context.RunID)); err == nil {
		t.Error("an unchanged projection still took a backup")
	}
}

// Convergence distributes a bootstrap and nothing else. The proposed catalog is
// reported by digest and never written, because an accidental enrollment
// invalidates in-flight offers on a live fleet.
func TestFleetConvergenceNeverEnrollsOrWritesACatalog(t *testing.T) {
	context := fleetTestPrepare(t)
	actions := []string{}
	result, err := fleetProjection(
		context, fleetTestComponent(t), fleetTestHost, context.RunID, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := fleetApply(context, fleetTestComponent(t), result, &actions); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result["enrolled"] != false {
		t.Errorf("the adapter reported enrolled=%v", result["enrolled"])
	}
	catalog := fleetTestFixture(t, "projection-coordinator.json")
	err = filepath.WalkDir(context.Home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(body), fleet.CoordinatorKind) {
			t.Errorf("a convergence wrote the proposed catalog to %s", path)
		}
		if strings.Contains(string(body), "credential-not-for-records") &&
			!strings.Contains(path, workerSecretRoot) {
			t.Errorf("a credential value reached %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if strings.Contains(string(catalog), "secretref:") {
		t.Error("the proposed catalog carries credential references")
	}
	for _, action := range actions {
		if strings.Contains(action, "enroll") &&
			!strings.Contains(action, "enrollment stays an explicit Steward operation") {
			t.Errorf("a convergence action claims an enrollment: %q", action)
		}
	}
}

// Both components project the same destination and the fleet one converges
// second, so a release whose two sources disagree is refused rather than
// silently rewriting the reviewed v1 entry.
func TestFleetRefusesToDisagreeWithWorkerConfiguration(t *testing.T) {
	context := fleetTestPrepare(t)
	workers, err := canonjson.DecodeObject(
		[]byte(`{"kind":"worker-configuration","hosts":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := canonjson.DecodeObject(
		fleetTestFixture(t, "projection-worker-"+fleetTestHost+".json"))
	if err != nil {
		t.Fatal(err)
	}
	hosts, _ := workers["hosts"].(map[string]any)
	hosts[fleetTestHost] = entry
	context.Components["worker-configuration"] = workers

	// Agreement is the normal case and converges.
	if _, err := fleetObserve(context, fleetTestComponent(t)); err != nil {
		t.Fatalf("an agreeing release was refused: %v", err)
	}

	entry["coordinator_id"] = "someone-else"
	_, err = fleetObserve(context, fleetTestComponent(t))
	if err == nil || !strings.Contains(err.Error(), "disagree about this host's bootstrap") {
		t.Fatalf("want a disagreement refusal, got %v", err)
	}
}

func TestFleetRefusesUnsafeCredentialReferences(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, context *Context)
	}{
		{
			name: "the admin credential is missing",
			prepare: func(t *testing.T, context *Context) {
				if err := os.Remove(
					context.Path(workerSecretRoot, "f03-admin/"+fleetTestHost)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "the admin credential is world readable",
			prepare: func(t *testing.T, context *Context) {
				if err := os.Chmod(
					context.Path(workerSecretRoot, "f03-admin/"+fleetTestHost), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "the worker credential is missing",
			prepare: func(t *testing.T, context *Context) {
				if err := os.Remove(
					context.Path(workerSecretRoot, "f02-protocol/"+fleetTestHost)); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context := fleetTestPrepare(t)
			test.prepare(t, context)
			if _, err := fleetObserve(context, fleetTestComponent(t)); err == nil {
				t.Fatal("an unusable credential reference was accepted")
			}
			if _, err := os.Stat(context.Path(fleet.WorkerDestination)); err == nil {
				t.Error("a refused projection still wrote the destination")
			}
		})
	}
}

// Rollback is exercised rather than assumed: the run that replaced a document
// is named, and the destination goes back to exactly what that run found.
func TestFleetRestoresTheRetainedDocument(t *testing.T) {
	context := fleetTestPrepare(t)
	firstRun := context.RunID
	actions := []string{}
	if _, err := fleetApply(context, fleetTestComponent(t), nil, &actions); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first, err := os.ReadFile(context.Path(fleet.WorkerDestination))
	if err != nil {
		t.Fatalf("bootstrap file: %v", err)
	}

	// The intent changes: this host moves to the build profile, which adds a
	// provider route, so the projected document changes too.
	changed := fleetTestComponent(t)
	profiles, _ := changed["profiles"].(map[string]any)
	standard, _ := profiles["standard"].(map[string]any)
	standard["provider_instances"] = []any{"claudeAgent", "codex", "gemini", "opencode"}
	const secondRun = "20260914T000002Z-changed"
	context.RunID = secondRun
	if _, err := fleetApply(context, changed, nil, &actions); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second, err := os.ReadFile(context.Path(fleet.WorkerDestination))
	if err != nil {
		t.Fatalf("bootstrap file: %v", err)
	}
	if string(second) == string(first) {
		t.Fatal("the changed intent did not change the document")
	}

	if err := FleetRestorePrior(context.Home, secondRun); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, err := os.ReadFile(context.Path(fleet.WorkerDestination))
	if err != nil || string(restored) != string(first) {
		t.Fatalf("restored document = %q (%v)", restored, err)
	}

	// Restoring the first run puts the host back to having no document at all,
	// which is what that run found.
	if err := FleetRestorePrior(context.Home, firstRun); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(context.Path(fleet.WorkerDestination)); err == nil {
		t.Error("restoring a run that found nothing left a document behind")
	}
}
