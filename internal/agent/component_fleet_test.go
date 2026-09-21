package agent

import (
	"errors"
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
	for _, destination := range []string{fleet.WorkerDestination, fleet.ClientDestination} {
		if _, err := os.Stat(context.Path(destination)); err == nil {
			t.Fatalf("a host the fleet does not declare was given %s", destination)
		}
	}
}

func TestClientOnlyFleetConvergenceWritesNoWorkerAndNeverEnrolls(t *testing.T) {
	context := adapterTestContext(t, &adapterTestRunner{})
	context.Host.Name = "laptop"
	path := context.Path(workerSecretRoot, "f03-admin/laptop")
	adapterTestWrite(t, path, "credential-not-for-records")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	component := fleetTestComponent(t)
	component["clients"] = map[string]any{
		"laptop": map[string]any{"admin_credential_ref": "secretref:f03-admin/laptop"},
	}
	result, err := fleetProjection(context, component, "laptop", context.RunID, false)
	if err != nil {
		t.Fatalf("client-only projection: %v", err)
	}
	if result["enrolled"] != false || result["admin_credential_ref"] != "secretref:f03-admin/laptop" {
		t.Fatalf("result = %v", result)
	}
	if _, err := os.Stat(context.Path(fleet.WorkerDestination)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("client-only host has a worker projection: %v", err)
	}
	if _, err := os.Stat(context.Path(fleet.CoordinatorDestination)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("client-only host has a coordinator projection: %v", err)
	}
	written, err := os.ReadFile(context.Path(fleet.ClientDestination))
	if err != nil {
		t.Fatalf("read client projection: %v", err)
	}
	document, _ := fleet.Parse(component)
	want, _ := document.ClientBytes("laptop")
	if string(written) != string(want) {
		t.Fatalf("client projection differs:\n%s", written)
	}
	if _, err := os.Stat(context.Path(workerSecretRoot, "f02-protocol/laptop")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("test unexpectedly provisioned a worker credential: %v", err)
	}
}

func TestRetiringClientOnlyHostRemovesOnlyOwnedProjection(t *testing.T) {
	context := adapterTestContext(t, &adapterTestRunner{})
	context.Host.Name = "laptop"
	secret := context.Path(workerSecretRoot, "f03-admin/laptop")
	adapterTestWrite(t, secret, "credential-not-for-records")
	if err := os.Chmod(secret, 0o600); err != nil {
		t.Fatal(err)
	}
	component := fleetTestComponent(t)
	component["clients"] = map[string]any{
		"laptop": map[string]any{"admin_credential_ref": "secretref:f03-admin/laptop"},
	}
	if _, err := fleetProjection(context, component, "laptop", context.RunID, false); err != nil {
		t.Fatalf("configure client: %v", err)
	}
	unrelated := context.Path(".config/t3-steward/unrelated.json")
	adapterTestWrite(t, unrelated, "keep")
	delete(component, "clients")
	context.RunID = "20260914T000002Z-retire-client"
	result, err := fleetProjection(context, component, "laptop", context.RunID, false)
	if err != nil {
		t.Fatalf("retire client: %v", err)
	}
	if result["configured"] != false || result["enrolled"] != false {
		t.Fatalf("retirement result = %v", result)
	}
	if _, err := os.Stat(context.Path(fleet.ClientDestination)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("client projection survived retirement: %v", err)
	}
	if body, err := os.ReadFile(unrelated); err != nil || string(body) != "keep" {
		t.Fatalf("retirement changed unrelated evidence: %q (%v)", body, err)
	}
}

func TestClientOnlyRollbackRestoresExactPriorProjection(t *testing.T) {
	context := adapterTestContext(t, &adapterTestRunner{})
	context.Host.Name = "laptop"
	secret := context.Path(workerSecretRoot, "f03-admin/laptop")
	adapterTestWrite(t, secret, "credential-not-for-records")
	if err := os.Chmod(secret, 0o600); err != nil {
		t.Fatal(err)
	}
	component := fleetTestComponent(t)
	component["clients"] = map[string]any{
		"laptop": map[string]any{"admin_credential_ref": "secretref:f03-admin/laptop"},
	}
	if _, err := fleetProjection(context, component, "laptop", context.RunID, false); err != nil {
		t.Fatalf("configure client: %v", err)
	}
	prior, err := os.ReadFile(context.Path(fleet.ClientDestination))
	if err != nil {
		t.Fatal(err)
	}
	endpoint := component["coordinator"].(map[string]any)["client_endpoint"].(map[string]any)
	endpoint["address"] = "changed-coordinator"
	context.RunID = "20260914T000001Z-change-client"
	if _, err := fleetProjection(context, component, "laptop", context.RunID, false); err != nil {
		t.Fatalf("change client: %v", err)
	}
	if err := FleetRestorePrior(context.Home, context.RunID); err != nil {
		t.Fatalf("restore changed client: %v", err)
	}
	if restored, err := os.ReadFile(context.Path(fleet.ClientDestination)); err != nil ||
		string(restored) != string(prior) {
		t.Fatalf("restored client = %q (%v), want exact prior %q", restored, err, prior)
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
	// Both owned documents are activated, and each is the shared fixture.
	for destination, name := range map[string]string{
		fleet.WorkerDestination: "projection-worker-" + fleetTestHost + ".json",
		fleet.ClientDestination: "projection-client-" + fleetTestHost + ".json",
	} {
		written, err := os.ReadFile(context.Path(destination))
		if err != nil {
			t.Fatalf("%s: %v", destination, err)
		}
		if string(written) != string(fleetTestFixture(t, name)) {
			t.Errorf("%s is not the shared projection:\n%s", destination, written)
		}
		info, err := os.Stat(context.Path(destination))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v (%v), want 0600", destination, info.Mode().Perm(), err)
		}
	}
	record, err := os.ReadFile(fleetTestPriorPath(context, context.RunID))
	if err != nil || strings.Count(string(record), `"existed": false`) != 3 {
		t.Fatalf("retained record = %q (%v)", record, err)
	}
	for _, destination := range []string{fleet.WorkerDestination, fleet.ClientDestination} {
		if !strings.Contains(string(record), destination) {
			t.Errorf("the retained record does not name %s", destination)
		}
	}

	// Nothing is left staged, and a second run over the same intent changes
	// nothing and takes no second backup.
	entries, err := os.ReadDir(filepath.Dir(context.Path(fleet.WorkerDestination)))
	if err != nil {
		t.Fatalf("read the destination directory: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("the destination directory holds %d entries, want the two documents",
			len(entries))
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

// A worker host receives no coordinator catalog, and convergence never enrolls.
func TestWorkerFleetConvergenceNeverEnrollsOrWritesACatalog(t *testing.T) {
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
	// The catalog is checked as the renderer produces it, not as a fixture
	// file happens to read: the renderer is what a convergence would hand to
	// an enrollment, and a fixture can drift from it.
	document, err := fleet.Parse(fleetTestComponent(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	catalog, err := document.CoordinatorBytes()
	if err != nil {
		t.Fatalf("coordinator projection: %v", err)
	}
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
	actions := []string{}
	if _, err := fleetApply(context, fleetTestComponent(t), nil, &actions); err == nil {
		t.Fatal("an apply went ahead despite the disagreement")
	}
	// The refusal happens before anything on the host is touched, which is what
	// makes the adoption hazard safe to run into.
	for _, destination := range []string{fleet.WorkerDestination, fleet.ClientDestination} {
		if _, err := os.Stat(context.Path(destination)); err == nil {
			t.Errorf("a refused projection still wrote %s", destination)
		}
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

	// The intent changes: this host's profile gains a provider instance, so its
	// projected provider routes change and the document does too.
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
	// The client document was retained by that run too, and restoring it puts
	// back exactly what the run found rather than deleting a file it never
	// changed.
	client, err := os.ReadFile(context.Path(fleet.ClientDestination))
	if err != nil ||
		string(client) != string(fleetTestFixture(t, "projection-client-"+fleetTestHost+".json")) {
		t.Fatalf("restored client document = %q (%v)", client, err)
	}

	// Restoring the first run puts the host back to having no documents at
	// all, which is what that run found.
	if err := FleetRestorePrior(context.Home, firstRun); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, destination := range []string{fleet.WorkerDestination, fleet.ClientDestination} {
		if _, err := os.Stat(context.Path(destination)); err == nil {
			t.Errorf("restoring a run that found nothing left %s behind", destination)
		}
	}
}

// The coordinator administers itself over its own local socket and refuses to
// start while a remote client endpoint is configured. Writing the client
// document there would produce advice the operator cannot follow, because the
// file it tells them to remove is one UpKeeper puts back.
func TestTheCoordinatorHostIsGivenNoClientDocument(t *testing.T) {
	context := fleetTestPrepare(t)
	component := fleetTestComponent(t)
	coordinator, _ := component["coordinator"].(map[string]any)
	coordinator["host"] = fleetTestHost
	endpoint, _ := coordinator["client_endpoint"].(map[string]any)
	endpoint["address"] = fleetTestHost

	// A document left over from when this host was only a worker is removed.
	adapterTestWrite(t, context.Path(fleet.ClientDestination), "{\"stale\": true}\n")
	if err := os.Chmod(context.Path(fleet.ClientDestination), 0o600); err != nil {
		t.Fatal(err)
	}
	actions := []string{}
	state, err := fleetApply(context, component, nil, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	if _, err := os.Stat(context.Path(fleet.ClientDestination)); err == nil {
		t.Error("the coordinator was given a client document")
	}
	if _, err := os.ReadFile(context.Path(fleet.WorkerDestination)); err != nil {
		t.Errorf("the coordinator lost its worker bootstrap: %v", err)
	}
	// The removal is retained, so restoring the run puts the file back.
	if err := FleetRestorePrior(context.Home, context.RunID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if body, err := os.ReadFile(context.Path(fleet.ClientDestination)); err != nil ||
		string(body) != "{\"stale\": true}\n" {
		t.Errorf("the removed document was not retained: %q (%v)", body, err)
	}

	// A second run finds nothing to do.
	context.RunID = "20260914T000004Z-coordinator"
	if _, err := fleetApply(context, component, nil, &actions); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if _, err := os.Stat(context.Path(fleet.ClientDestination)); err == nil {
		t.Error("the coordinator was given a client document on the second run")
	}
	context.RunID = "20260914T000005Z-coordinator"
	state, err = fleetApply(context, component, nil, &actions)
	if err != nil || state != "converged" {
		t.Fatalf("state = %q, error = %v, want converged", state, err)
	}
}

// A stat that follows links approves the target's mode and owner while the
// runtime, which opens with O_NOFOLLOW, refuses the link. The host would
// converge clean and fail at first use, so the link is refused here too.
func TestFleetRefusesASymlinkedSecret(t *testing.T) {
	for _, reference := range []string{
		"f02-protocol/" + fleetTestHost,
		"f03-admin/" + fleetTestHost,
	} {
		t.Run(reference, func(t *testing.T) {
			context := fleetTestPrepare(t)
			path := context.Path(workerSecretRoot, reference)
			elsewhere := filepath.Join(context.Home, "real-secret")
			adapterTestWrite(t, elsewhere, "credential-not-for-records")
			if err := os.Chmod(elsewhere, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, path); err != nil {
				t.Fatal(err)
			}
			_, err := fleetObserve(context, fleetTestComponent(t))
			if err == nil {
				t.Fatal("a symlinked secret was accepted")
			}
			if !strings.Contains(err.Error(), "symlink") &&
				!strings.Contains(err.Error(), "escapes") {
				t.Errorf("the refusal does not say why: %v", err)
			}
			for _, destination := range []string{
				fleet.WorkerDestination, fleet.ClientDestination,
			} {
				if _, err := os.Stat(context.Path(destination)); err == nil {
					t.Errorf("a refused projection still wrote %s", destination)
				}
			}
		})
	}
}

// A run that owns two documents must not be able to leave one of them applied
// and the other not. The failure is injected on the second document, after the
// first has already been activated, which is the shape every real cause has:
// a full disk, a read-only filesystem, an exhausted quota.
func TestAFailureOnTheSecondDocumentRestoresTheFirst(t *testing.T) {
	context := fleetTestPrepare(t)
	document, err := fleet.Parse(fleetTestComponent(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	client, err := document.ClientBytes(fleetTestHost)
	if err != nil {
		t.Fatalf("client projection: %v", err)
	}
	const stale = "{\"stale\": true}\n"
	for _, destination := range []string{fleet.WorkerDestination, fleet.ClientDestination} {
		adapterTestWrite(t, context.Path(destination), stale)
		if err := os.Chmod(context.Path(destination), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The second document is handed a payload that does not satisfy the
	// contract of its own destination, so its activation fails after the first
	// document has been renamed into place.
	documents := []*fleetDocument{
		{
			destination: fleet.ClientDestination,
			target:      context.Path(fleet.ClientDestination),
			payload:     client,
			before:      []byte(stale),
			changed:     true,
		},
		{
			destination: fleet.WorkerDestination,
			target:      context.Path(fleet.WorkerDestination),
			payload:     client,
			before:      []byte(stale),
			changed:     true,
		},
	}
	err = fleetWriteProjection(context, context.RunID, documents)
	if err == nil {
		t.Fatal("an unactivatable document was reported as applied")
	}
	var rolled *RolledBack
	if !errors.As(err, &rolled) {
		t.Fatalf("a half-applied run reported %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), fleet.WorkerDestination) ||
		!strings.Contains(err.Error(), "left consistent") {
		t.Errorf("the refusal does not say what state the host is in: %v", err)
	}

	// Both documents are back to what the run found, including the one that
	// was successfully activated before the failure.
	for _, destination := range []string{fleet.WorkerDestination, fleet.ClientDestination} {
		body, err := os.ReadFile(context.Path(destination))
		if err != nil || string(body) != stale {
			t.Errorf("%s is %q (%v), want what the run found", destination, body, err)
		}
	}
	// Nothing is left staged in the destination directory either.
	entries, err := os.ReadDir(filepath.Dir(context.Path(fleet.WorkerDestination)))
	if err != nil {
		t.Fatalf("read the destination directory: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("the destination directory holds %d entries, want the two documents",
			len(entries))
	}
}

// A convergence is retried after a transport failure often enough that the
// retention must not refuse the second attempt, and the record the first
// attempt took is the one that says what the run found.
func TestRetainingTwiceUnderOneRunIDIsIdempotent(t *testing.T) {
	context := fleetTestPrepare(t)
	actions := []string{}
	if _, err := fleetApply(context, fleetTestComponent(t), nil, &actions); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first, err := os.ReadFile(fleetTestPriorPath(context, context.RunID))
	if err != nil {
		t.Fatalf("retained record: %v", err)
	}

	// The same run retried: the documents now exist, so a second retention
	// under this run ID must neither refuse nor overwrite the record that says
	// the run found nothing.
	documents, err := fleetPlanDocuments(context, []byte("{}\n"), []byte("{}\n"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := fleetRetainPrior(context, context.RunID, documents); err != nil {
		t.Fatalf("a retried retention was refused: %v", err)
	}
	second, err := os.ReadFile(fleetTestPriorPath(context, context.RunID))
	if err != nil || string(second) != string(first) {
		t.Error("a retry overwrote the record of what the run originally found")
	}

	// A directory that exists without a record is a genuine collision and is
	// diagnosed rather than silently reused.
	const other = "20260914T000009Z-collision"
	if err := os.MkdirAll(context.Path(fleet.StateDirectory, other), 0o700); err != nil {
		t.Fatal(err)
	}
	err = fleetRetainPrior(context, other, documents)
	if err == nil || !strings.Contains(err.Error(), "without a retained record") {
		t.Fatalf("a collision produced %v", err)
	}
}

// The client document has its own reason to change, and changing it must not
// touch the worker bootstrap: rewriting a host's worker identity because its
// coordinator timeout moved is exactly the coupling separate files avoid.
func TestFleetRewritesOnlyTheDocumentThatChanged(t *testing.T) {
	context := fleetTestPrepare(t)
	actions := []string{}
	if _, err := fleetApply(context, fleetTestComponent(t), nil, &actions); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	workerBefore, err := os.ReadFile(context.Path(fleet.WorkerDestination))
	if err != nil {
		t.Fatalf("bootstrap file: %v", err)
	}

	changed := fleetTestComponent(t)
	coordinator, _ := changed["coordinator"].(map[string]any)
	endpoint, _ := coordinator["client_endpoint"].(map[string]any)
	endpoint["request_timeout"] = "45s"
	context.RunID = "20260914T000003Z-client"
	state, err := fleetApply(context, changed, nil, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	client, err := os.ReadFile(context.Path(fleet.ClientDestination))
	if err != nil || !strings.Contains(string(client), `"request_timeout": "45s"`) {
		t.Fatalf("client document = %q (%v)", client, err)
	}
	workerAfter, err := os.ReadFile(context.Path(fleet.WorkerDestination))
	if err != nil {
		t.Fatalf("bootstrap file: %v", err)
	}
	if string(workerAfter) != string(workerBefore) {
		t.Errorf("a client-only change rewrote the worker bootstrap:\n%s", workerAfter)
	}
	// Both documents are still retained, so restoring the run is whole.
	record, err := os.ReadFile(fleetTestPriorPath(context, context.RunID))
	if err != nil || strings.Count(string(record), `"existed": true`) != 2 {
		t.Fatalf("retained record = %q (%v)", record, err)
	}
}
