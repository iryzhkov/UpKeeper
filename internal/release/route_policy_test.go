package release_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/policytest"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func TestReleasePolicyValidationUsesConfiguredCatalog(t *testing.T) {
	policytest.RequireCLI(t)
	acceptAnyEnvironment(t)
	var policy map[string]any
	if err := json.Unmarshal([]byte(policytest.Policy), &policy); err != nil {
		t.Fatal(err)
	}
	manifest := withFleetIntent(t, func(intent map[string]any) { intent["route_policy"] = policy })
	for _, tc := range []struct {
		name   string
		models []string
		want   string
	}{
		{"offline authorized", []string{"gpt-6.1-sol"}, ""},
		{"unauthorized", []string{"gpt-6-sol"}, "not catalog-authorized"},
		{"wildcard is not an exact authorization", []string{"*"}, "not catalog-authorized"},
		{"missing authority", nil, "configured route catalog unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policytest.Catalog(t, tc.models)
			_, err := release.Validate(manifest)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s: %v", tc.want, err)
			}
		})
	}
}
