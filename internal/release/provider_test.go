package release_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// providerRoot is projectRoot with a third host, laptop, on the standard
// profile beside homelab, so that a provider edit naming only one host of a
// profile has somewhere to land other than the profile itself.
func providerRoot(t *testing.T) string {
	t.Helper()
	root := projectRoot(t)
	raw := manifestBytes(t, root)
	manifest, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	components, _ := manifest["components"].(map[string]any)
	intent, _ := components[fleet.Component].(map[string]any)
	hosts, _ := intent["hosts"].(map[string]any)
	hosts["laptop"] = map[string]any{
		"admin_credential_ref": "secretref:f03-admin/laptop",
		"credential_ref":       "secretref:f02-protocol/laptop",
		"profile":              "standard",
		"transport":            "ssh",
		"worker_id":            "laptop",
	}
	encoded, err := canonjson.MarshalFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, release.File), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func resolvedProfile(t *testing.T, value release.Release, host string) fleet.Profile {
	t.Helper()
	document, present, err := value.FleetIntent()
	if err != nil || !present {
		t.Fatalf("returned release has no parseable fleet intent: %v", err)
	}
	profile, err := document.Resolved(host)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// Naming every host of a profile edits the profile; naming one host of a
// profile that has others edits that host's exceptions and leaves the rest of
// the profile's hosts exactly as they were. Either way the instance, its
// models ("*" unless named), the pool and the binding land together and the
// written manifest is canonical and parses.
func TestAddProviderEditsTheProfileOrTheHostExceptions(t *testing.T) {
	root := providerRoot(t)
	value, edit, err := release.AddProvider(root, release.ProviderSpec{
		Instance: "ollama", QuotaPool: "local", Hosts: []string{"omarchy-pc", "laptop"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if strings.Join(edit.Profiles, ",") != "build" || strings.Join(edit.Hosts, ",") != "laptop" {
		t.Fatalf("edit landed on profiles %v and hosts %v; want build and laptop", edit.Profiles, edit.Hosts)
	}
	for _, host := range []string{"omarchy-pc", "laptop"} {
		profile := resolvedProfile(t, value, host)
		if !contains(profile.ProviderInstances, "ollama") || !contains(profile.QuotaPools, "local") {
			t.Errorf("%s: instances %v, pools %v", host, profile.ProviderInstances, profile.QuotaPools)
		}
		if !reflect.DeepEqual(profile.Models["ollama"], []string{"*"}) {
			t.Errorf("%s: ollama models %v, want [*]", host, profile.Models["ollama"])
		}
		if profile.QuotaBindings["ollama"] != "local" {
			t.Errorf("%s: bindings %v", host, profile.QuotaBindings)
		}
	}
	homelab := resolvedProfile(t, value, "homelab")
	if contains(homelab.ProviderInstances, "ollama") || contains(homelab.QuotaPools, "local") {
		t.Errorf("homelab was not named and yet changed: %+v", homelab)
	}
	document, _, _ := value.FleetIntent()
	if document.Hosts["homelab"].Exceptions != nil {
		t.Errorf("homelab gained exceptions: %v", document.Hosts["homelab"].Exceptions)
	}
	if document.Profiles["standard"].QuotaBindings["ollama"] != "" {
		t.Error("the standard profile was edited for a one-host change")
	}
	// The written file is what was returned, canonical, and parses.
	raw := manifestBytes(t, root)
	loaded, err := release.Load(root)
	if err != nil {
		t.Fatalf("the written manifest does not load: %v", err)
	}
	if loaded.Digest() != value.Digest() {
		t.Fatal("the returned release differs from the written one")
	}
	decoded, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := canonjson.MarshalFile(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, raw) {
		t.Fatal("the written manifest is not canonical")
	}
	// The coordinator projection of the edited release carries the binding.
	projection, err := document.CoordinatorProjection()
	if err != nil {
		t.Fatal(err)
	}
	workers := projection["workers"].(map[string]any)
	for _, host := range []string{"omarchy-pc", "laptop"} {
		bindings, _ := workers[host].(map[string]any)["quota_bindings"].(map[string]any)
		if bindings["ollama"] != "local" {
			t.Errorf("%s projects bindings %v", host, bindings)
		}
	}
}

// The instance the fleet already carries without a binding is the real case:
// opencode is authorized on every profile and bound nowhere. Adding it binds
// it and keeps its authored models unless --models says otherwise.
func TestAddProviderBindsAnInstanceTheProfileAlreadyRuns(t *testing.T) {
	root := providerRoot(t)
	value, edit, err := release.AddProvider(root, release.ProviderSpec{
		Instance: "opencode", QuotaPool: "opencode-free",
		Hosts: []string{"homelab", "laptop", "omarchy-pc"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if strings.Join(edit.Profiles, ",") != "build,standard" || len(edit.Hosts) != 0 {
		t.Fatalf("edit landed on profiles %v and hosts %v; want both profiles", edit.Profiles, edit.Hosts)
	}
	for _, host := range []string{"homelab", "laptop", "omarchy-pc"} {
		profile := resolvedProfile(t, value, host)
		if profile.QuotaBindings["opencode"] != "opencode-free" || !contains(profile.QuotaPools, "opencode-free") {
			t.Errorf("%s: bindings %v pools %v", host, profile.QuotaBindings, profile.QuotaPools)
		}
		if !reflect.DeepEqual(profile.Models["opencode"], []string{"glm-4.6"}) {
			t.Errorf("%s: the authored models were replaced: %v", host, profile.Models["opencode"])
		}
	}
	// Named models replace the allowlist.
	value, _, err = release.AddProvider(root, release.ProviderSpec{
		Instance: "codex", QuotaPool: "default", Hosts: []string{"homelab", "laptop"},
		Models: []string{"gpt-5-codex", "gpt-5"},
	})
	if err != nil {
		t.Fatalf("add with models: %v", err)
	}
	if models := resolvedProfile(t, value, "homelab").Models["codex"]; !reflect.DeepEqual(models, []string{"gpt-5", "gpt-5-codex"}) {
		t.Errorf("codex models %v, want the sorted named list", models)
	}
}

func TestAddProviderRefusesAndLeavesTheManifestUntouched(t *testing.T) {
	cases := []struct {
		name     string
		prepare  func(t *testing.T, root string)
		spec     release.ProviderSpec
		expected string
	}{
		{"an unknown host", nil, release.ProviderSpec{Instance: "ollama", QuotaPool: "local", Hosts: []string{"nowhere"}}, "nowhere"},
		{"no hosts", nil, release.ProviderSpec{Instance: "ollama", QuotaPool: "local"}, "at least one host"},
		{"a bad instance name", nil, release.ProviderSpec{Instance: "ol lama", QuotaPool: "local", Hosts: []string{"homelab"}}, "instance"},
		{"a bad pool name", nil, release.ProviderSpec{Instance: "ollama", QuotaPool: "lo cal", Hosts: []string{"homelab"}}, "pool"},
		{"a bad model", nil, release.ProviderSpec{Instance: "ollama", QuotaPool: "local", Hosts: []string{"homelab"}, Models: []string{"bad model"}}, "model"},
		{"an instance bound to another pool", func(t *testing.T, root string) {
			if _, _, err := release.AddProvider(root, release.ProviderSpec{Instance: "opencode", QuotaPool: "default", Hosts: []string{"homelab", "laptop"}}); err != nil {
				t.Fatal(err)
			}
		}, release.ProviderSpec{Instance: "opencode", QuotaPool: "opencode-free", Hosts: []string{"homelab"}}, "two pools"},
		{"an instance already bound to that pool", func(t *testing.T, root string) {
			if _, _, err := release.AddProvider(root, release.ProviderSpec{Instance: "opencode", QuotaPool: "default", Hosts: []string{"homelab", "laptop"}}); err != nil {
				t.Fatal(err)
			}
		}, release.ProviderSpec{Instance: "opencode", QuotaPool: "default", Hosts: []string{"laptop"}}, "already bound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := providerRoot(t)
			if tc.prepare != nil {
				tc.prepare(t, root)
			}
			before := manifestBytes(t, root)
			_, _, err := release.AddProvider(root, tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.expected) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.expected)
			}
			if !bytes.Equal(before, manifestBytes(t, root)) {
				t.Fatal("a refused add changed the manifest")
			}
		})
	}
}

// Removing an instance takes it, its models and its binding out of every
// named host (or of every profile and host when none is named). Pools are
// membership, not a binding, and are left alone. An instance nothing runs is
// refused by name and nothing is written.
func TestRemoveProviderRemovesItAndRefusesAnUnknownOne(t *testing.T) {
	root := providerRoot(t)
	if _, _, err := release.AddProvider(root, release.ProviderSpec{
		Instance: "ollama", QuotaPool: "local", Hosts: []string{"omarchy-pc", "laptop"},
	}); err != nil {
		t.Fatal(err)
	}
	value, edit, err := release.RemoveProvider(root, "ollama", []string{"laptop"})
	if err != nil {
		t.Fatalf("remove from one host: %v", err)
	}
	if strings.Join(edit.Hosts, ",") != "laptop" || len(edit.Profiles) != 0 {
		t.Fatalf("edit landed on profiles %v and hosts %v; want laptop", edit.Profiles, edit.Hosts)
	}
	laptop := resolvedProfile(t, value, "laptop")
	if contains(laptop.ProviderInstances, "ollama") || laptop.Models["ollama"] != nil || laptop.QuotaBindings["ollama"] != "" {
		t.Errorf("laptop still runs ollama: %+v", laptop)
	}
	if !contains(laptop.QuotaPools, "local") {
		t.Errorf("removing an instance dropped the pool: %v", laptop.QuotaPools)
	}
	if pc := resolvedProfile(t, value, "omarchy-pc"); !contains(pc.ProviderInstances, "ollama") {
		t.Errorf("omarchy-pc was not named and yet lost ollama: %v", pc.ProviderInstances)
	}
	value, edit, err = release.RemoveProvider(root, "ollama", nil)
	if err != nil {
		t.Fatalf("remove everywhere: %v", err)
	}
	if strings.Join(edit.Profiles, ",") != "build" {
		t.Fatalf("edit landed on profiles %v and hosts %v; want build", edit.Profiles, edit.Hosts)
	}
	if pc := resolvedProfile(t, value, "omarchy-pc"); contains(pc.ProviderInstances, "ollama") {
		t.Errorf("omarchy-pc still runs ollama: %v", pc.ProviderInstances)
	}
	before := manifestBytes(t, root)
	if _, _, err := release.RemoveProvider(root, "ollama", nil); err == nil || !strings.Contains(err.Error(), "ollama") {
		t.Fatalf("removing an absent instance: err = %v", err)
	}
	if _, _, err := release.RemoveProvider(root, "codex", []string{"nowhere"}); err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Fatalf("removing from an unknown host: err = %v", err)
	}
	if !bytes.Equal(before, manifestBytes(t, root)) {
		t.Fatal("a refused remove changed the manifest")
	}
}
