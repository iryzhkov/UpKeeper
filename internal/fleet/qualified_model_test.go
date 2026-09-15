package fleet_test

import (
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

func TestProviderQualifiedModelAuthorization(t *testing.T) {
	const model = "opencode/muse-spark-1.3-contributor-free"
	document, err := fleet.Parse(edit(t, func(d map[string]any) {
		object(t, d, "profiles", "standard", "models")["opencode"] = []any{model}
	}))
	if err != nil {
		t.Fatal(err)
	}
	sets, err := document.Models("homelab")
	if err != nil {
		t.Fatal(err)
	}
	if len(sets.Desired["opencode"]) != 1 || sets.Desired["opencode"][0] != model {
		t.Fatal("qualified model changed")
	}
	raw, err := document.CoordinatorBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), model) {
		t.Fatal("qualified model missing from coordinator authorization")
	}
}

func TestQualifiedModelIdentifierRefusals(t *testing.T) {
	for _, model := range []string{"/model", "provider/", "provider//model", "provider/../model", "provider/./model", "provider/$(id)", "provider/model;id", "provider/model\nother", "provider/model value", "provider/--option", strings.Repeat("a", 129)} {
		t.Run(model, func(t *testing.T) {
			_, err := fleet.Parse(edit(t, func(d map[string]any) { object(t, d, "profiles", "standard", "models")["opencode"] = []any{model} }))
			if err == nil {
				t.Fatal("invalid qualified model accepted")
			}
		})
	}
}

func TestQualifiedModelDoesNotBroadenOtherIdentifiers(t *testing.T) {
	for _, field := range []string{"worker_id", "profile", "transport"} {
		t.Run(field, func(t *testing.T) {
			_, err := fleet.Parse(edit(t, func(d map[string]any) { object(t, d, "hosts", "homelab")[field] = "provider/model" }))
			if err == nil {
				t.Fatal("slash accepted outside typed model field")
			}
		})
	}
}
