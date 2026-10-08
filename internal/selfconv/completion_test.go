package selfconv_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
)

func TestPostReportCarriesCompletion(t *testing.T) {
	var posted map[string]any
	runner := func(command procx.Command) procx.Result {
		if err := json.Unmarshal([]byte(command.Input), &posted); err != nil {
			t.Fatalf("the feed body is not JSON: %v", err)
		}
		return procx.Result{}
	}
	report := map[string]any{
		"exit_code": 40, "run_id": "20261007T040000Z-0a1b2c3d", "completion": "partial",
		"hosts": []any{map[string]any{
			"host": "agent-a", "result": "failed", "completion": "partial",
			"components": []any{
				map[string]any{"name": "agent-tooling", "state": "failed", "error": "health check failed"},
				map[string]any{"name": "steward-fleet-configuration", "state": "updated"},
			},
		}},
	}
	if err := selfconv.PostReport(report, runner); err != nil {
		t.Fatalf("PostReport: %v", err)
	}
	if posted["completion"] != "partial" {
		t.Errorf("the summary is %#v", posted)
	}
	hosts, _ := posted["hosts"].([]any)
	if len(hosts) != 1 {
		t.Fatalf("the summary hosts are %#v", posted["hosts"])
	}
	if host, _ := hosts[0].(map[string]any); host["completion"] != "partial" {
		t.Errorf("the summary host is %#v", host)
	}
}

// A partial run is still a failure: the hold marker stays until a human has
// looked at the host, exactly as for a run that stopped at the failure.
func TestAPartialRunWithAFailureKeepsTheHoldMarker(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "scheduled-holds")
	holds := &selfconv.HoldRunner{
		Directory: directory,
		Inner: func(inventory.HostSpec, agent.Payload, time.Duration, bool) map[string]any {
			return map[string]any{
				"host": "agent-a", "reachable": true, "result": "failed",
				"components": []any{
					map[string]any{"name": "agent-tooling", "state": "failed"},
					map[string]any{"name": "steward-fleet-configuration", "state": "updated"},
				},
			}
		},
	}
	holds.Run(inventory.HostSpec{Name: "agent-a"}, agent.Payload{RunID: "20261007T040000Z-0a1b2c3d"}, time.Minute, true)
	if _, err := os.Stat(selfconv.HoldPath(directory, "agent-a")); err != nil {
		t.Fatalf("a partial run with a failure cleared the hold: %v", err)
	}
}
