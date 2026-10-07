package converge_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/converge"
)

// The inventory's tool profile travels to the agent; a host without one gets
// the payload it always got, with no agent_tools key at all.
func TestThePayloadCarriesTheAgentToolProfile(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	spec := config.Hosts["normandy"]
	if spec.AgentTools != nil {
		t.Fatal("the fixture host already has a profile")
	}
	for _, tools := range [][]string{nil, {"homelab-cli", "ov-memory"}} {
		spec.AgentTools = tools
		config.Hosts["normandy"] = spec
		choices := options(t, []string{"normandy"})
		choices.DryRun = true
		var seen []agent.Payload
		choices.Runner = stateRunner("converged", &seen)
		if _, _, err := converge.Fleet(config, value, choices); err != nil {
			t.Fatal(err)
		}
		if len(seen) != 1 || !reflect.DeepEqual(seen[0].Host.AgentTools, tools) {
			t.Fatalf("profile %v produced %+v", tools, seen)
		}
		encoded, err := json.Marshal(seen[0].Host)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "agent_tools") != (tools != nil) {
			t.Fatalf("host reference %s", encoded)
		}
	}
}
