package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/operatorhold"
)

// holdOnThisHost sets an operator hold in the target's own state, which is how
// a hold set from another controller, or on the host itself, reaches the agent.
func holdOnThisHost(t *testing.T, context *Context) string {
	t.Helper()
	directory := operatorhold.Directory(context.Home)
	if _, err := operatorhold.Set(directory, operatorhold.Hold{
		Host: context.Host.Name, Reason: "canary first", SetBy: "igor",
		SetAt: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	return directory
}

// The target honours its own hold whatever the controller knows: a payload
// without the override converges nothing and takes no lock.
func TestTheAgentHonoursItsOwnOperatorHold(t *testing.T) {
	commit := strings.Repeat("b", 40)
	context := fenceContext(t, Payload{
		RunID: "20260914T090000Z-0a1b2c3d", Host: HostRef{Name: "homelab"},
		Components: []string{"huyang"},
	}, commit)
	holdOnThisHost(t, context)
	result := Execute(context)
	if result["result"] != "held" || !strings.Contains(result["error"].(string), "canary first") {
		t.Fatalf("result %v", result)
	}
	if components, _ := result["components"].([]any); len(components) != 0 {
		t.Errorf("a held payload reached components: %v", components)
	}
	if _, err := os.Stat(filepath.Dir(context.lockDirectory)); err == nil {
		t.Error("a held payload created the lock directory")
	}

	// A dry run changes nothing and is not held.
	dry := fenceContext(t, Payload{
		RunID: "20260914T090000Z-0a1b2c3d", Host: HostRef{Name: "homelab"}, DryRun: true,
	}, commit)
	holdOnThisHost(t, dry)
	if result := Execute(dry); result["result"] == "held" {
		t.Errorf("a dry run was held: %v", result)
	}
}

func TestAnOverridePassesTheAgentsHoldAndIsLoggedThere(t *testing.T) {
	commit := strings.Repeat("b", 40)
	context := fenceContext(t, Payload{
		RunID: "20260914T090000Z-0a1b2c3d", Host: HostRef{Name: "homelab"}, OverrideHold: true,
	}, commit)
	directory := holdOnThisHost(t, context)
	result := Execute(context)
	if result["result"] != "converged" {
		t.Fatalf("result %v", result)
	}
	if overridden, _ := result["hold_overridden"].(map[string]any); overridden["reason"] != "canary first" {
		t.Errorf("the result does not name the overridden hold: %v", result)
	}
	raw, err := os.ReadFile(operatorhold.LogPath(directory, "homelab"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"event":"override"`) || !strings.Contains(string(raw), "20260914T090000Z-0a1b2c3d") {
		t.Errorf("override not logged on the target: %s", raw)
	}
}

// An ordinary payload is byte for byte what it was before holds existed, and
// the override survives the trip to the target.
func TestTheOverrideFlagIsOmittedFromAnOrdinaryPayload(t *testing.T) {
	encoded, err := json.Marshal(Payload{RunID: "r", Host: HostRef{Name: "homelab"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "override_hold") {
		t.Errorf("an ordinary payload carries the override: %s", encoded)
	}
	payload, err := DecodePayload([]byte(`{"run_id":"r","host":{"name":"homelab"},"override_hold":true}`))
	if err != nil || !payload.OverrideHold {
		t.Fatalf("override did not decode: %+v %v", payload, err)
	}
}
