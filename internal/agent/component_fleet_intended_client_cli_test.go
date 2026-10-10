package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// Exercise the real installed command's config decoder and secret resolver.
// This HOME has no admin secret store, so the remote carrier cannot start.
// A policy transport mock alone would miss using projection credential_ref
// where the CLI requires the exact same secretref under credential.
func TestFleetPolicyIntendedClientRealCLIRequiresSecret(t *testing.T) {
	c, component := policyContext(t)
	d, err := fleet.Parse(component)
	if err != nil {
		t.Fatal(err)
	}
	client, err := d.ClientBytes(fleetTestHost)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the actual CLI's HOME/state empty, independently of the agent's
	// fixture credential metadata. Nothing can contact a live coordinator.
	isolated := t.TempDir()
	c.Env = append(os.Environ(), "HOME="+isolated,
		"XDG_CONFIG_HOME="+isolated+"/config", "XDG_STATE_HOME="+isolated+"/state")
	var result procx.Result
	c.Runner = func(cmd procx.Command) procx.Result {
		result = procx.Run(cmd)
		return result
	}
	// Baseline's actual clientless command must reproduce rc121's refusal.
	_, err = fleetPlanPolicy(c, d.RoutePolicyBytes())
	if err == nil || result.Code != 3 || !strings.Contains(result.Stderr, "has no coordinator client") {
		t.Fatalf("baseline clientless CLI boundary: %v; %+v", err, result)
	}
	_, err = fleetPlanPolicy(c, d.RoutePolicyBytes(), client)
	if err == nil || result.Code != 3 || !strings.Contains(result.Stderr, "admin secret store unavailable") {
		t.Fatalf("real CLI must parse intended client and refuse missing secret before transport: %v; %+v", err, result)
	}
	for _, path := range []string{fleet.ClientDestination, fleet.WorkerDestination, fleetPolicyReceiptPath} {
		if _, err := os.Stat(c.Path(path)); !os.IsNotExist(err) {
			t.Fatalf("real CLI refusal wrote %s: %v", path, err)
		}
	}
}
