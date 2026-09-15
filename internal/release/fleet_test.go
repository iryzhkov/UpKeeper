package release_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func fleetIntent(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(
		filepath.Join("..", "..", "testdata", "fleet", "fleet-intent.json"))
	if err != nil {
		t.Fatalf("intent fixture: %v", err)
	}
	document, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return document
}

func withFleetIntent(t *testing.T, change func(intent map[string]any)) map[string]any {
	t.Helper()
	manifest := pairedManifestFixture(t)
	intent := fleetIntent(t)
	if change != nil {
		change(intent)
	}
	components, _ := manifest["components"].(map[string]any)
	components[fleet.Component] = intent
	return manifest
}

// The component is appended after worker-configuration, which is both the
// convergence order and the order a refusal is reported in.
func TestTheFleetComponentComesAfterWorkerConfiguration(t *testing.T) {
	order := release.ComponentOrder
	if order[len(order)-1] != fleet.Component ||
		order[len(order)-2] != "worker-configuration" {
		t.Fatalf("component order is %v", order)
	}
	if !release.OptionalComponents[fleet.Component] {
		t.Error("the fleet component must be optional, or older releases stop loading")
	}
}

func TestAReleaseWithAndWithoutTheFleetComponentBothValidate(t *testing.T) {
	acceptAnyEnvironment(t)
	without, err := release.Validate(pairedManifestFixture(t))
	if err != nil {
		t.Fatalf("a release without the fleet component was refused: %v", err)
	}
	if _, present, err := without.FleetIntent(); present || err != nil {
		t.Errorf("FleetIntent reports present=%v, error=%v", present, err)
	}

	with, err := release.Validate(withFleetIntent(t, nil))
	if err != nil {
		t.Fatalf("a release with the fleet component was refused: %v", err)
	}
	document, present, err := with.FleetIntent()
	if err != nil || !present {
		t.Fatalf("FleetIntent reports present=%v, error=%v", present, err)
	}
	if names := document.HostNames(); len(names) != 2 || names[0] != "homelab" {
		t.Errorf("fleet hosts are %v", names)
	}
	// Adding a component changes the release identity, which is exactly what
	// makes it reviewable; it must not change the identity of a release that
	// does not carry it.
	if with.Digest() == without.Digest() {
		t.Error("adding the fleet component did not change the release identity")
	}
	if len(release.Describe(with)) != len(release.Describe(without))+1 {
		t.Error("the capture summary does not describe the fleet intent")
	}
}

// A release captured before any of the current components existed still loads,
// because a rollback to it has to work.
func TestALegacyReleaseStillValidates(t *testing.T) {
	acceptAnyEnvironment(t)
	manifest := pairedManifestFixture(t)
	components, _ := manifest["components"].(map[string]any)
	kept := map[string]any{}
	for _, name := range release.LegacyComponentOrder {
		kept[name] = components[name]
	}
	steward, _ := kept["t3-steward"].(map[string]any)
	steward["installer"] = "t3-update"
	agentConfig, _ := kept["agent-config"].(map[string]any)
	agentConfig["files"] = []any{map[string]any{
		"path": ".codex/AGENTS.md", "store": "codex/AGENTS.md",
		"sha256": strings.Repeat("0", 64),
	}}
	manifest["components"] = kept
	if _, err := release.Validate(manifest); err != nil {
		t.Fatalf("a legacy release was refused: %v", err)
	}
}

func TestFleetComponentRefusals(t *testing.T) {
	acceptAnyEnvironment(t)
	cases := []struct {
		name      string
		change    func(intent map[string]any)
		errorText string
	}{
		{
			name:      "an unknown schema version fails closed",
			change:    func(intent map[string]any) { intent["schema_version"] = json.Number("2") },
			errorText: "unsupported steward-fleet-configuration schema_version",
		},
		{
			name:      "the wrong kind is refused",
			change:    func(intent map[string]any) { intent["kind"] = "worker-configuration" },
			errorText: "wrong kind",
		},
		{
			name: "a catalog is not smuggled in as an extra field",
			change: func(intent map[string]any) {
				intent["catalog"] = map[string]any{"workers": []any{}}
			},
			errorText: "unknown top-level field",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := release.Validate(withFleetIntent(t, test.change))
			if err == nil || !strings.Contains(err.Error(), test.errorText) {
				t.Fatalf("want an error containing %q, got %v", test.errorText, err)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "invalid steward fleet configuration") {
				t.Errorf("the refusal does not name the component: %v", err)
			}
		})
	}
}

// A release whose component set is neither the legacy one nor the current one
// is still refused: optional does not mean anything goes.
func TestAnUnknownComponentIsStillRefused(t *testing.T) {
	acceptAnyEnvironment(t)
	manifest := pairedManifestFixture(t)
	components, _ := manifest["components"].(map[string]any)
	components["invented"] = map[string]any{"kind": "files"}
	_, err := release.Validate(manifest)
	if err == nil || !strings.Contains(err.Error(), "release components must be exactly") {
		t.Fatalf("produced %v", err)
	}
}
