package agent

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/policytest"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func TestFleetWorkerRoutePolicyOptOut(t *testing.T) {
	for _, prior := range []string{"unowned", "absent", "existing"} {
		t.Run(prior, func(t *testing.T) {
			c, component := policyContext(t)
			if prior != "absent" {
				adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), "unmanaged prior bytes")
			}
			if prior != "unowned" {
				if _, err := fleetProjection(c, component, fleetTestHost, "before-opt-out", false); err != nil {
					t.Fatal(err)
				}
			}
			component["hosts"].(map[string]any)[fleetTestHost].(map[string]any)["route_policy"] = false
			calls := 0
			c.Runner = func(cmd procx.Command) procx.Result {
				calls++
				return procx.Result{Code: 3, Stderr: "no coordinator client"}
			}
			observed, err := fleetObserve(c, component)
			if err != nil {
				t.Fatal(err)
			}
			if prior != "unowned" && !bytes.Equal(policyRead(t, c), mustPolicyBytes(t, component)) {
				t.Fatal("observe changed owned policy")
			}
			c.RunID = "opt-out"
			actions := []string{}
			if _, err := fleetApply(c, component, observed, &actions); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("opt-out ran %d commands", calls)
			}
			if prior == "absent" {
				if _, err := os.Stat(c.Path(fleet.RoutePolicyDestination)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("owned policy survived: %v", err)
				}
			} else if string(policyRead(t, c)) != "unmanaged prior bytes" {
				t.Fatal("prior policy lost")
			}
			observed, err = fleetObserve(c, component)
			if err != nil || observed["state"] != "converged" {
				t.Fatalf("after opt-out: %v %v", observed, err)
			}
			if _, ok := observed["route_policy_projection"]; ok {
				t.Fatal("retired policy still projected")
			}
			if calls != 0 {
				t.Fatalf("observe ran %d commands", calls)
			}
		})
	}
}

func mustPolicyBytes(t *testing.T, component map[string]any) []byte {
	t.Helper()
	d, err := fleet.Parse(component)
	if err != nil {
		t.Fatal(err)
	}
	return d.RoutePolicyBytes()
}

func TestFleetWorkerRoutePolicyDefaultStillValidates(t *testing.T) {
	c, component := policyContext(t)
	calls := 0
	c.Runner = func(cmd procx.Command) procx.Result {
		calls++
		return procx.Result{Code: 3, Stderr: "no coordinator client"}
	}
	if _, err := fleetObserve(c, component); err == nil || calls != 1 {
		t.Fatalf("default validation: calls=%d err=%v", calls, err)
	}
}

func TestAuthoredReleaseWorkersOmitPolicyValidation(t *testing.T) {
	policytest.RequireCLI(t)
	raw, err := os.ReadFile("../../desired/release.json")
	if err != nil {
		t.Fatal(err)
	}
	release, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	component := release["components"].(map[string]any)["steward-fleet-configuration"].(map[string]any)
	d, err := fleet.Parse(component)
	if err != nil {
		t.Fatal(err)
	}
	for name, host := range d.Hosts {
		want := name != "agent-a" && name != "agent-b"
		if host.RoutePolicy != want {
			t.Fatalf("%s route_policy=%v", name, host.RoutePolicy)
		}
	}
	for _, name := range []string{"agent-a", "agent-b"} {
		t.Run(name, func(t *testing.T) {
			c := adapterTestContext(t, &adapterTestRunner{})
			c.Host.Name = name
			for _, prefix := range []string{"f02-protocol/", "f03-admin/"} {
				path := c.Path(workerSecretRoot, prefix+name)
				adapterTestWrite(t, path, "test-only")
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			c.Runner = func(cmd procx.Command) procx.Result {
				t.Fatalf("unexpected validation: %v", cmd.Argv)
				return procx.Result{}
			}
			observed, err := fleetObserve(c, component)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := observed["route_policy_projection"]; ok {
				t.Fatalf("opt-out policy plan: %v", observed)
			}
			if _, err := os.Stat(c.Path(fleet.RoutePolicyDestination)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("policy projected: %v", err)
			}
		})
	}
}
