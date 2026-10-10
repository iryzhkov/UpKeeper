package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func TestFleetPolicyOverrideIntroducedDuringValidationRefuses(t *testing.T) {
	c, component := policyContext(t)
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "t3-steward", "config.yaml")
	calls := 0
	c.Runner = func(procx.Command) procx.Result {
		calls++
		if err := os.WriteFile(path, []byte("backlog_v2: {coordinator_client: {address: conflicting-endpoint}}"), 0600); err != nil {
			t.Fatal(err)
		}
		return procx.Result{Code: 0, Stdout: "valid output"}
	}
	if _, err := fleetProjection(c, component, fleetTestHost, "override-race", false); err == nil {
		t.Fatal("concurrent override accepted")
	}
	if calls != 1 {
		t.Fatalf("unexpected validation retry: %d", calls)
	}
	for _, destination := range []string{fleet.ClientDestination, fleet.WorkerDestination, fleet.RoutePolicyDestination, fleetPolicyReceiptPath, fleet.StateDirectory + "/override-race"} {
		if _, err := os.Stat(c.Path(destination)); !os.IsNotExist(err) {
			t.Fatalf("conflict wrote %s: %v", destination, err)
		}
	}
}
