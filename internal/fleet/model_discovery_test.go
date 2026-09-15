package fleet_test

import (
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"strings"
	"testing"
)

func TestAllDiscoveredModelsPolicyIsExplicitAndProviderScoped(t *testing.T) {
	document, err := fleet.Parse(edit(t, func(d map[string]any) { object(t, d, "profiles", "standard", "models")["codex"] = []any{"*"} }))
	if err != nil {
		t.Fatal(err)
	}
	sets, err := document.Models("homelab")
	if err != nil {
		t.Fatal(err)
	}
	if len(sets.Desired["codex"]) != 1 || sets.Desired["codex"][0] != "*" {
		t.Fatal("lost discovery policy")
	}
	raw, err := document.CoordinatorBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\"*\"") {
		t.Fatal("policy missing from projection")
	}
	for _, values := range [][]any{{"*", "gpt"}, {"gpt-*"}} {
		if _, err := fleet.Parse(edit(t, func(d map[string]any) { object(t, d, "profiles", "standard", "models")["codex"] = values })); err == nil {
			t.Fatal("ambiguous model pattern accepted")
		}
	}
}
