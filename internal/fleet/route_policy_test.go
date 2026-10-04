package fleet_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/policytest"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func policyIntent(t *testing.T) map[string]any {
	t.Helper()
	value := intent(t)
	var policy map[string]any
	if err := json.Unmarshal([]byte(policytest.Policy), &policy); err != nil {
		t.Fatal(err)
	}
	value["route_policy"] = policy
	return value
}

func TestRoutePolicyDeterministicConsumerAndLegacyParity(t *testing.T) {
	policytest.RequireCLI(t)
	first, err := fleet.Parse(policyIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	second, err := fleet.Parse(policyIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.RoutePolicyBytes(), second.RoutePolicyBytes()) {
		t.Fatal("unstable policy bytes")
	}
	expected, _ := canonjson.MarshalFile(policyIntent(t)["route_policy"])
	if !bytes.Equal(first.RoutePolicyBytes(), expected) {
		t.Fatal("policy order/fields changed")
	}
	legacy := parsed(t)
	if legacy.RoutePolicyBytes() != nil {
		t.Fatal("legacy gained a policy")
	}
	for _, host := range legacy.HostNames() {
		a, _ := first.WorkerBytes(host)
		b, _ := legacy.WorkerBytes(host)
		if !bytes.Equal(a, b) {
			t.Fatal("policy changed worker bytes")
		}
		a, _ = first.ClientBytes(host)
		b, _ = legacy.ClientBytes(host)
		if !bytes.Equal(a, b) {
			t.Fatal("policy changed client bytes")
		}
	}
	a, _ := first.CoordinatorBytes()
	b, _ := legacy.CoordinatorBytes()
	if !bytes.Equal(a, b) {
		t.Fatal("policy changed catalog bytes")
	}
	// The real consumer parses the rendered document, including raw-byte provenance.
	err = fleet.CheckRoutePolicy(first.RoutePolicyBytes(), func(c procx.Command) procx.Result {
		r := procx.Run(c)
		var p struct {
			Digest string
			Roles  []struct {
				Name       string
				Candidates []struct{ Route, Effort, Tier string }
			}
		}
		if r.OK() {
			if e := json.Unmarshal([]byte(r.Stdout), &p); e != nil {
				t.Fatal(e)
			}
			if p.Digest != fleet.Digest(first.RoutePolicyBytes()) || len(p.Roles) != 1 || p.Roles[0].Candidates[0].Effort != "medium" {
				t.Fatalf("consumer: %s", r.Stdout)
			}
		}
		return r
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	copy := first.RoutePolicyBytes()
	copy[0] = 'x'
	if bytes.Equal(copy, first.RoutePolicyBytes()) {
		t.Fatal("mutable policy snapshot")
	}
}

func TestRoutePolicyAuthoritativeSchemaRefusals(t *testing.T) {
	policytest.RequireCLI(t)
	cases := []struct{ name, raw string }{
		{"version", strings.Replace(policytest.Policy, "route-policy/v1", "route-policy/v2", 1)},
		{"unknown", strings.Replace(policytest.Policy, `"schema":`, `"secret":"x","schema":`, 1)},
		{"max", strings.Replace(policytest.Policy, "medium", "max", 1)},
		{"missing effort", strings.Replace(policytest.Policy, `"effort":"medium",`, "", 1)},
		{"duplicate key", strings.Replace(policytest.Policy, `"schema":`, `"schema":"route-policy/v1","schema":`, 1)},
		{"documents", policytest.Policy + "\n---\n" + policytest.Policy},
		{"duplicate role", `{"schema":"route-policy/v1","roles":[{"name":"3d","candidates":[{"route":"codex/gpt-6-astra","effort":"medium","tier":"premium"}]},{"name":"3d","candidates":[{"route":"codex/gpt-6-astra","effort":"medium","tier":"premium"}]}]}`},
		{"duplicate routes", strings.Replace(policytest.Policy, `"tier":"standard"}]`, `"tier":"standard"},{"route":"codex/gpt-6.1-sol","effort":"medium","tier":"standard"}]`, 1)},
		{"contradiction", strings.Replace(policytest.Policy, `"name":"execute",`, `"name":"execute","constraints":{"provider_families":["codex"],"exclude_provider_families":["codex"]},`, 1)},
		{"duplicate constraint", strings.Replace(policytest.Policy, `"name":"execute",`, `"name":"execute","constraints":{"tiers":["standard","standard"]},`, 1)},
		{"tier", strings.Replace(policytest.Policy, "standard", "invented", 1)},
		{"constraint tier", strings.Replace(policytest.Policy, `"name":"execute",`, `"name":"execute","constraints":{"tiers":["premium"]},`, 1)},
		{"unsafe role", strings.Replace(policytest.Policy, "execute", "../bad", 1)},
		{"oversize", strings.Repeat(" ", fleet.RoutePolicyMaxBytes+1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := fleet.CheckRoutePolicy([]byte(c.raw), nil, false); err == nil {
				t.Fatal("accepted invalid policy")
			}
		})
	}
	for _, value := range []any{nil, "path", []any{}} {
		intent := intent(t)
		intent["route_policy"] = value
		if _, err := fleet.Parse(intent); err == nil {
			t.Fatal("accepted non-object policy")
		}
	}
	// A missing installed authority is a remedy-bearing refusal, never acceptance.
	err := fleet.CheckRoutePolicy([]byte(policytest.Policy), func(procx.Command) procx.Result { return procx.Result{Code: -1, Err: os.ErrNotExist} }, false)
	if err == nil || !strings.Contains(err.Error(), "rc107") || !strings.Contains(err.Error(), "ced8e299561920ad1b46059f354316467f10f655") {
		t.Fatal(err)
	}
}

func TestInitialPolicyFixtureIsConsumedInOrder(t *testing.T) {
	policytest.RequireCLI(t)
	raw := fixture(t, "route-policy.yaml")
	var shown map[string]any
	err := fleet.CheckRoutePolicy(raw, func(c procx.Command) procx.Result {
		r := procx.Run(c)
		if r.OK() {
			if err := json.Unmarshal([]byte(r.Stdout), &shown); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if shown["digest"] != fleet.Digest(raw) {
		t.Fatal("raw provenance lost")
	}
	roles := shown["roles"].([]any)
	names := []string{"plan", "execute", "review", "critical-review", "read", "3d"}
	if len(roles) != len(names) {
		t.Fatal("roles changed")
	}
	for i, name := range names {
		role := roles[i].(map[string]any)
		if role["name"] != name {
			t.Fatal("role order changed")
		}
		candidates := role["candidates"].([]any)
		if name == "3d" {
			if len(candidates) != 1 {
				t.Fatal("specialty expanded")
			}
		} else {
			if len(candidates) != 2 || !strings.HasPrefix(candidates[0].(map[string]any)["route"].(string), "claudeAgent/") || !strings.HasPrefix(candidates[1].(map[string]any)["route"].(string), "codex/") {
				t.Fatal("candidate order changed")
			}
		}
		if name == "execute" || name == "review" {
			for _, candidate := range candidates {
				if candidate.(map[string]any)["effort"] != "medium" {
					t.Fatal("execution/review effort changed")
				}
			}
		}
		if name == "critical-review" || name == "read" {
			tiers := role["constraints"].(map[string]any)["tiers"].([]any)
			want := "premium"
			if name == "read" {
				want = "economy"
			}
			if len(tiers) != 1 || tiers[0] != want {
				t.Fatal("constraints changed")
			}
		}
	}
	// Round-trip the semantic object through fleet rendering and the actual
	// consumer, rather than relying on the producer's JSON parser alone.
	delete(shown, "digest")
	value := intent(t)
	value["route_policy"] = shown
	d, err := fleet.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.CheckRoutePolicy(d.RoutePolicyBytes(), nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestRoutePolicyConfiguredAuthorizationIgnoresReadiness(t *testing.T) {
	policytest.RequireCLI(t)
	policytest.Catalog(t, []string{"gpt-6.1-sol"})
	d, err := fleet.Parse(policyIntent(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateRoutePolicy(); err != nil {
		t.Fatal(err)
	}
	t.Run("sole wildcard authorizes exact candidate", func(t *testing.T) {
		policytest.Catalog(t, []string{"*"})
		if err := d.ValidateRoutePolicy(); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		name   string
		models []string
		want   string
	}{
		{"unauthorized", []string{"gpt-6-sol"}, "not catalog-authorized"},
		{"mixed wildcard", []string{"*", "gpt-6-sol"}, "not catalog-authorized"},
		{"partial pattern", []string{"gpt-*"}, "not catalog-authorized"},
		{"missing catalog", nil, "configured route catalog unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policytest.Catalog(t, tc.models)
			if err := d.ValidateRoutePolicy(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s: %v", tc.want, err)
			}
		})
	}
}
