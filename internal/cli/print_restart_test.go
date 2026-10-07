package cli

import (
	"bytes"
	"strings"
	"testing"
)

const testRestartNote = "worker restart pending since 2026-10-04T04:30:00Z (1d6h): 3 dispatched, " +
	"0 settle-pending; the restart waits for an idle worker; this host has not been idle for 1d6h; " +
	"drain the worker once Steward supports it, or restart it by hand at a quiet moment after " +
	"checking t3-steward worker inspect-journal"

// Status puts a pending worker restart's own account first in the drift
// reason, ahead of what the run records say.
func TestStatusExplainsAPendingWorkerRestart(t *testing.T) {
	document := map[string]any{
		"run_id": "r", "controller": "normandy", "result": "drift", "exit_code": 10,
		"hosts": []any{map[string]any{
			"host": "omarchy-pc", "name": "omarchy-pc", "reachable": true, "result": "drift",
			"components": []any{
				map[string]any{"name": "t3-steward", "state": "drift", "observed": map[string]any{
					"worker_restart_pending":       true,
					"worker_restart_pending_since": "2026-10-04T04:30:00Z",
					"worker_restart_waiting":       map[string]any{"note": testRestartNote},
				}},
				map[string]any{"name": "huyang", "state": "drift", "observed": map[string]any{}},
			},
		}},
	}
	annotateDriftReasons(document, driftEvidence{controller: "normandy"})
	var out bytes.Buffer
	printStatus(&out, document)
	text := out.String()
	if !strings.Contains(text, "  t3-steward: "+testRestartNote+"; ") {
		t.Errorf("status does not lead with the pending restart:\n%s", text)
	}
	if strings.Contains(text, "  huyang: worker restart") {
		t.Errorf("another component carries the restart note:\n%s", text)
	}
}

// Doctor shows a pending worker restart as a notice beside the host.
func TestDoctorTextShowsAPendingWorkerRestart(t *testing.T) {
	var out bytes.Buffer
	printDoctor(&out, map[string]any{
		"run_id": "r", "controller": "normandy", "result": "healthy", "exit_code": 0,
		"hosts": []any{map[string]any{
			"name": "omarchy-pc", "reachable": true, "result": "healthy",
			"notices": []any{"t3-steward: " + testRestartNote},
		}},
	})
	if !strings.Contains(out.String(), "omarchy-pc: healthy\n  notice: t3-steward: "+testRestartNote+"\n") {
		t.Errorf("doctor text:\n%s", out.String())
	}
}
