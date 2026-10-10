package fleet_test

import (
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

func TestHostRoutePolicy(t *testing.T) {
	for _, tc := range []struct {
		name          string
		value         any
		present, want bool
	}{
		{"default", nil, false, true}, {"true", true, true, true}, {"false", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := intent(t)
			if tc.present {
				object(t, raw, "hosts", "homelab")["route_policy"] = tc.value
			}
			d, err := fleet.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if d.Hosts["homelab"].RoutePolicy != tc.want {
				t.Fatalf("route policy = %v", d.Hosts["homelab"].RoutePolicy)
			}
		})
	}
	for _, value := range []any{nil, "false", 0, map[string]any{}, []any{false}} {
		raw := intent(t)
		object(t, raw, "hosts", "homelab")["route_policy"] = value
		if _, err := fleet.Parse(raw); err == nil || !strings.Contains(err.Error(), "route_policy must be a boolean") {
			t.Fatalf("invalid %v: %v", value, err)
		}
	}
	t.Run("coordinator", func(t *testing.T) {
		raw := intent(t)
		object(t, raw, "coordinator")["host"] = "homelab"
		object(t, raw, "coordinator", "client_endpoint")["address"] = "homelab"
		object(t, raw, "hosts", "homelab")["route_policy"] = false
		if _, err := fleet.Parse(raw); err == nil || !strings.Contains(err.Error(), "cannot set route_policy to false") {
			t.Fatalf("coordinator opt-out: %v", err)
		}
	})
	t.Run("client", func(t *testing.T) {
		raw := intent(t)
		raw["clients"] = map[string]any{"laptop": map[string]any{"admin_credential_ref": "secretref:f03-admin/laptop", "route_policy": false}}
		if _, err := fleet.Parse(raw); err == nil || !strings.Contains(err.Error(), "route_policy") {
			t.Fatalf("client opt-out: %v", err)
		}
	})
}
