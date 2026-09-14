package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func workerTestComponent() map[string]any {
	return map[string]any{
		"kind": "worker-configuration",
		"hosts": map[string]any{
			"testhost": map[string]any{
				"schema_version":  adapterTestNumber("1"),
				"worker_id":       "testhost",
				"coordinator_id":  "normandy",
				"transport":       "ssh",
				"capabilities":    []any{"git", "huyang"},
				"provider_routes": []any{},
				"credential_ref":  "secretref:f02-protocol/testhost",
			},
		},
	}
}

// workerTestSecret puts a credential in the target-local store. Its content is
// never read by the adapter, only its metadata.
func workerTestSecret(t *testing.T, context *Context, mode os.FileMode, content string) string {
	t.Helper()
	path := context.Path(workerSecretRoot, "f02-protocol/testhost")
	adapterTestWrite(t, path, content)
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkerProjectionWithoutAnEntryForThisHost(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	context.Host.Name = "another-host"
	spec := workerTestComponent()
	observed, err := workerObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if observed["state"] != "converged" || observed["configured"] != false {
		t.Fatalf("observation = %v", observed)
	}
	actions := []string{}
	state, err := workerApply(context, spec, observed, &actions)
	if err != nil || state != "converged" || len(actions) != 0 {
		t.Fatalf("state = %q, error = %v, actions = %v", state, err, actions)
	}
	if _, err := os.Stat(context.Path(workerDestination)); err == nil {
		t.Fatal("a host without an entry was given a bootstrap file")
	}
}

func TestWorkerProjectionWritesABackupAndVerifies(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	workerTestSecret(t, context, 0o600, "credential-not-for-records")
	spec := workerTestComponent()

	preview, err := workerObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if preview["state"] != "updated" || preview["configured"] != true {
		t.Fatalf("preview = %v", preview)
	}
	if _, err := os.Stat(context.Path(workerDestination)); err == nil {
		t.Fatal("an observation wrote the bootstrap file")
	}

	actions := []string{}
	state, err := workerApply(context, spec, preview, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	if !adapterTestMatches(actions, "worker bootstrap projected; enrollment belongs to S4") {
		t.Fatalf("actions %v do not report the projection", actions)
	}
	written, err := os.ReadFile(context.Path(workerDestination))
	if err != nil {
		t.Fatalf("bootstrap file: %v", err)
	}
	if strings.Contains(string(written), "credential-not-for-records") {
		t.Fatal("the projection carries the credential itself")
	}
	info, err := os.Stat(context.Path(workerDestination))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v (%v), want 0600", info.Mode().Perm(), err)
	}
	prior := context.Path(".local/state/dev-fleet/worker-config", context.RunID, "prior.json")
	record, err := os.ReadFile(prior)
	if err != nil || !strings.Contains(string(record), `"existed": false`) {
		t.Fatalf("prior record = %q (%v)", record, err)
	}

	// A second run over the same content changes nothing.
	context.RunID = "20260914T000001Z-second"
	second := []string{}
	state, err = workerApply(context, spec, preview, &second)
	if err != nil || state != "converged" {
		t.Fatalf("state = %q, error = %v, want converged", state, err)
	}
	if _, err := os.Stat(context.Path(
		".local/state/dev-fleet/worker-config", context.RunID)); err == nil {
		t.Fatal("an unchanged projection still took a backup")
	}
}

func TestWorkerProjectionRecordsTheContentItReplaced(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	workerTestSecret(t, context, 0o600, "credential")
	adapterTestWrite(t, context.Path(workerDestination), "{\"stale\": true}\n")
	actions := []string{}
	if _, err := workerApply(context, workerTestComponent(), nil, &actions); err != nil {
		t.Fatalf("apply: %v", err)
	}
	record, err := os.ReadFile(context.Path(
		".local/state/dev-fleet/worker-config", context.RunID, "prior.json"))
	if err != nil || !strings.Contains(string(record), `"existed": true`) ||
		!strings.Contains(string(record), "stale") {
		t.Fatalf("prior record = %q (%v)", record, err)
	}
}

func TestWorkerRefusesUnsafeSecretReferences(t *testing.T) {
	cases := []struct {
		name      string
		prepare   func(t *testing.T, context *Context)
		reference string
		errorText string
	}{
		{
			name:      "a reference outside the f02-protocol namespace is refused",
			prepare:   func(*testing.T, *Context) {},
			reference: "secretref:other/testhost",
			errorText: "invalid f02-protocol reference",
		},
		{
			name: "a world-readable secret is refused",
			prepare: func(t *testing.T, context *Context) {
				workerTestSecret(t, context, 0o644, "credential")
			},
			errorText: "nonempty owner-only 0600 file",
		},
		{
			name: "an empty secret is refused",
			prepare: func(t *testing.T, context *Context) {
				workerTestSecret(t, context, 0o600, "")
			},
			errorText: "nonempty owner-only 0600 file",
		},
		{
			name: "a symlinked secret is refused",
			prepare: func(t *testing.T, context *Context) {
				elsewhere := filepath.Join(context.Home, "elsewhere")
				adapterTestWrite(t, elsewhere, "credential")
				path := context.Path(workerSecretRoot, "f02-protocol/testhost")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, path); err != nil {
					t.Fatal(err)
				}
			},
			errorText: "escapes its target-local store",
		},
		{
			name:      "a secret that does not exist is reported rather than created",
			prepare:   func(*testing.T, *Context) {},
			errorText: "no such file or directory",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &adapterTestRunner{}
			context := adapterTestContext(t, runner)
			test.prepare(t, context)
			reference := test.reference
			if reference == "" {
				reference = "secretref:f02-protocol/testhost"
			}
			_, err := workerResolveSecretReference(context.Home, reference)
			if err == nil || !strings.Contains(err.Error(), test.errorText) {
				t.Fatalf("want error containing %q, got %v", test.errorText, err)
			}
			if test.reference == "" {
				actions := []string{}
				if _, err := workerApply(
					context, workerTestComponent(), nil, &actions); err == nil {
					t.Fatal("the projection accepted an unsafe secret reference")
				}
				if _, err := os.Stat(context.Path(workerDestination)); err == nil {
					t.Fatal("a refused projection still wrote the bootstrap file")
				}
			}
		})
	}
}

func TestWorkerValidationRefusesAnythingBeyondTheBootstrapContract(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(component map[string]any)
		errorText string
	}{
		{
			name: "an extra component field is refused",
			mutate: func(component map[string]any) {
				component["catalog"] = map[string]any{}
			},
			errorText: "must contain only kind and hosts",
		},
		{
			name: "an inline credential is refused",
			mutate: func(component map[string]any) {
				workerTestEntry(component)["credential"] = "never allowed"
			},
			errorText: "unknown worker bootstrap fields",
		},
		{
			name: "an identity that does not match its host is refused",
			mutate: func(component map[string]any) {
				workerTestEntry(component)["worker_id"] = "somebody-else"
			},
			errorText: "worker identity must match its inventory host",
		},
		{
			name: "a transport other than ssh is refused",
			mutate: func(component map[string]any) {
				workerTestEntry(component)["transport"] = "http"
			},
			errorText: "transport must be ssh",
		},
		{
			name: "an unsorted capability list is refused",
			mutate: func(component map[string]any) {
				workerTestEntry(component)["capabilities"] = []any{"huyang", "git"}
			},
			errorText: "must be sorted and unique",
		},
		{
			name: "a worker that does not declare the required capabilities is refused",
			mutate: func(component map[string]any) {
				workerTestEntry(component)["capabilities"] = []any{"git"}
			},
			errorText: "must declare git and huyang",
		},
		{
			name: "a credential reference for another host is refused",
			mutate: func(component map[string]any) {
				workerTestEntry(component)["credential_ref"] =
					"secretref:f02-protocol/normandy"
			},
			errorText: "its own opaque f02-protocol secret reference",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			component := workerTestComponent()
			test.mutate(component)
			err := workerValidate(component)
			if err == nil || !strings.Contains(err.Error(), test.errorText) {
				t.Fatalf("want error containing %q, got %v", test.errorText, err)
			}
		})
	}
}

func workerTestEntry(component map[string]any) map[string]any {
	hosts, _ := component["hosts"].(map[string]any)
	entry, _ := hosts["testhost"].(map[string]any)
	return entry
}
