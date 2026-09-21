package release

import (
	"reflect"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

func TestPreserveAuthoredFleetIntentKeepsClientOnlyHosts(t *testing.T) {
	authored := map[string]any{
		"kind": fleet.Component,
		"clients": map[string]any{
			"laptop": map[string]any{
				"admin_credential_ref": "secretref:f03-admin/laptop",
			},
		},
	}
	components := map[string]any{
		fleet.Component: map[string]any{"kind": "observed-local-state"},
	}
	if err := preserveAuthoredFleetIntent(components, authored, true); err != nil {
		t.Fatalf("preserve: %v", err)
	}
	if !reflect.DeepEqual(components[fleet.Component], authored) {
		t.Fatalf("preserved component = %#v, want %#v", components[fleet.Component], authored)
	}
}
