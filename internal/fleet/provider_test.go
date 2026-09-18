package fleet_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// A quota binding is authorization: it says which pool the coordinator may
// charge an instance's work to. It is parsed from the profile, overridden by a
// host exception like every other profile key, and rendered into that worker's
// coordinator entry. A worker with no binding renders no quota_bindings key at
// all, so a document that never authored one projects the bytes it always did.
func TestQuotaBindingsAreParsedAndRenderedIntoTheCoordinatorProjection(t *testing.T) {
	document, err := fleet.Parse(edit(t, func(d map[string]any) {
		object(t, d, "profiles", "build")["quota_bindings"] = map[string]any{
			"claudeAgent": "build",
			"opencode":    "default",
		}
	}))
	if err != nil {
		t.Fatalf("a profile with quota bindings does not parse: %v", err)
	}
	want := map[string]string{"claudeAgent": "build", "opencode": "default"}
	if !reflect.DeepEqual(document.Profiles["build"].QuotaBindings, want) {
		t.Fatalf("build bindings = %v, want %v", document.Profiles["build"].QuotaBindings, want)
	}
	if len(document.Profiles["standard"].QuotaBindings) != 0 {
		t.Fatalf("standard has bindings it never authored: %v", document.Profiles["standard"].QuotaBindings)
	}
	projection, err := document.CoordinatorProjection()
	if err != nil {
		t.Fatal(err)
	}
	workers := projection["workers"].(map[string]any)
	pc := workers["omarchy-pc"].(map[string]any)
	if !reflect.DeepEqual(pc["quota_bindings"], map[string]any{"claudeAgent": "build", "opencode": "default"}) {
		t.Errorf("omarchy-pc renders quota_bindings %v", pc["quota_bindings"])
	}
	if _, present := workers["homelab"].(map[string]any)["quota_bindings"]; present {
		t.Error("homelab renders a quota_bindings key it never authored")
	}

	// A host exception overrides the binding, and may add one to a profile
	// that has none: the exception is the reviewed place a host differs.
	overridden, err := fleet.Parse(edit(t, func(d map[string]any) {
		object(t, d, "profiles", "build")["quota_bindings"] = map[string]any{"opencode": "default"}
		object(t, d, "hosts", "omarchy-pc", "exceptions")["quota_bindings"] = map[string]any{"opencode": "build"}
		object(t, d, "hosts", "homelab")["exceptions"] = map[string]any{
			"quota_bindings": map[string]any{"codex": "default"},
		}
	}))
	if err != nil {
		t.Fatalf("a host exception on quota_bindings does not parse: %v", err)
	}
	for host, want := range map[string]map[string]string{
		"omarchy-pc": {"opencode": "build"},
		"homelab":    {"codex": "default"},
	} {
		resolved, err := overridden.Resolved(host)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(resolved.QuotaBindings, want) {
			t.Errorf("%s resolves bindings %v, want %v", host, resolved.QuotaBindings, want)
		}
	}
}

// A binding is refused when it names an instance the profile does not run or a
// pool the profile does not join, because either is authorization nobody can
// act on and nobody can see is dead. The field is closed: the value is a
// string per instance and nothing else.
func TestQuotaBindingsAreRefusedWhenTheyDoNotMatchTheProfile(t *testing.T) {
	cases := []struct {
		name      string
		value     any
		errorText string
	}{
		{"an instance the profile does not run", map[string]any{"gemini": "default"}, "does not run"},
		{"a pool the profile does not join", map[string]any{"opencode": "free"}, "does not join"},
		{"a non-string pool", map[string]any{"opencode": 3}, "quota_bindings"},
		{"a non-object", []any{"opencode"}, "quota_bindings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fleet.Parse(edit(t, func(d map[string]any) {
				object(t, d, "profiles", "standard")["quota_bindings"] = tc.value
			}))
			if err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.errorText)
			}
		})
	}
}

// Registering a provider changes a worker's own catalog entry: the plan lists
// the model authorization, the pool it joins and the binding, tells the worker
// to re-enroll, and warns that the coordinator refuses the reload while that
// worker holds a retained assignment.
func TestEnrollmentPlanExplainsAnAddedProvider(t *testing.T) {
	previous := parsed(t)
	proposed, err := fleet.Parse(edit(t, func(d map[string]any) {
		profile := object(t, d, "profiles", "build")
		profile["provider_instances"] = []any{"claudeAgent", "codex", "ollama", "opencode"}
		object(t, profile, "models")["ollama"] = []any{"*"}
		profile["quota_pools"] = []any{"build", "default", "local"}
		profile["quota_bindings"] = map[string]any{"ollama": "local"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := proposed.EnrollmentPlan(previous)
	if err != nil {
		t.Fatal(err)
	}
	changes, _ := plan["authorization_changes"].([]any)
	lines := make([]string, 0, len(changes))
	for _, change := range changes {
		lines = append(lines, change.(string))
	}
	text := strings.Join(lines, "\n")
	for _, expected := range []string{
		"worker omarchy-pc is authorized for ollama model *",
		"worker omarchy-pc joins quota pool local",
		"worker omarchy-pc binds ollama to quota pool local",
		"t3-steward worker enroll omarchy-pc --current-catalog",
		fleet.ReloadRefusalAdvice,
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("the plan does not say %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "worker enroll homelab") {
		t.Errorf("the plan tells an untouched worker to re-enroll:\n%s", text)
	}
	// Removing the binding again is said in as many words.
	reversed, err := previous.EnrollmentPlan(proposed)
	if err != nil {
		t.Fatal(err)
	}
	back, _ := reversed["authorization_changes"].([]any)
	found := false
	for _, change := range back {
		found = found || change.(string) == "worker omarchy-pc unbinds ollama from quota pool local"
	}
	if !found {
		t.Errorf("the reverse plan does not say the binding is removed: %v", back)
	}
}
