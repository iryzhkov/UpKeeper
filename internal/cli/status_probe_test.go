package cli

import (
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/selfconv"
)

// probeEvidence is what a target's own probe reports under "upkeeper": its
// hold marker, its last scheduled run and its timer, in the document shape
// hostevidence.Collect renders.
func probeEvidence() map[string]any {
	return map[string]any{
		"holds": []any{map[string]any{
			"host": "homelab", "path": "/home/x/.local/state/dev-fleet/scheduled-holds/homelab.hold",
			"run_id": "20260917T063000Z-0a1b2c3d", "age_seconds": 26 * 3600,
		}},
		"scheduled_runs": []any{map[string]any{
			"run_id": "20260917T063000Z-0a1b2c3d", "command": "self-pull",
			"created_at": "2026-09-17T06:30:00Z", "result": "exit-40",
			"components": []any{"t3-steward", "t3"},
			"hosts": []any{map[string]any{
				"host": "homelab", "result": "failed",
				"components": []any{map[string]any{
					"name": "t3-steward", "state": "failed",
					"error": "running T3 0.0.40 is outside the candidate steward's T3 range 0.0.38..0.0.38; no safe pair transition",
				}},
			}},
		}},
		"timers": map[string]any{
			"upkeeper-self.timer": map[string]any{"enabled": false, "active": false},
			"upkeeper-pull.timer": map[string]any{"enabled": nil, "active": nil},
		},
	}
}

// A remote host's drift is explained from what its own probe reported: the
// hold marker it holds, its timer, the last scheduled run it recorded. The
// "reason unavailable" sentence is kept only for a host that reported no
// probe evidence at all.
func TestStatusDriftReasonsComeFromTheTargetProbe(t *testing.T) {
	homelab := statusTestHost("homelab", "t3-steward", "drift", "huyang", "drift")
	homelab["upkeeper"] = probeEvidence()
	document := map[string]any{"controller": "omarchy-pc", "hosts": []any{
		homelab,
		statusTestHost("laptop", "t3-steward", "drift"),
	}}
	annotateDriftReasons(document, driftEvidence{controller: "omarchy-pc"})

	expected := map[[2]string]string{
		{"homelab", "t3-steward"}: "scheduled hold marker present for 26h0m0s (left by run 20260917T063000Z-0a1b2c3d); " +
			"observe the host, then clear it with: upkeeper hold clear homelab --reason TEXT; " +
			"upkeeper-self.timer is disabled on homelab, so no scheduled run will converge it; " +
			"last self-pull run 20260917T063000Z-0a1b2c3d (2026-09-17T06:30:00Z) failed t3-steward: " +
			"running T3 0.0.40 is outside the candidate steward's T3 range 0.0.38..0.0.38; no safe pair transition",
		// The run never selected huyang, so the probe says nothing about it
		// beyond the marker and the timer.
		{"homelab", "huyang"}: "scheduled hold marker present for 26h0m0s (left by run 20260917T063000Z-0a1b2c3d); " +
			"observe the host, then clear it with: upkeeper hold clear homelab --reason TEXT; " +
			"upkeeper-self.timer is disabled on homelab, so no scheduled run will converge it; " +
			"no scheduled run selecting huyang recorded on homelab",
		{"laptop", "t3-steward"}: "reason unavailable from this host: no hold marker and no scheduled run record for laptop here; " +
			"inspect laptop's own run store and timer",
	}
	for key, want := range expected {
		reason, present := statusTestReason(t, document, key[0], key[1])
		if !present || reason != want {
			t.Errorf("%s %s reason is\n%q\nexpected\n%q", key[0], key[1], reason, want)
		}
	}
}

// The controller's own evidence about a remote host (a marker it wrote when
// it dispatched there) is kept in front of what the probe reported, and a
// probe that reports nothing wrong still replaces the unavailable sentence.
func TestStatusDriftReasonPrefersLocalEvidenceAndAcceptsAQuietProbe(t *testing.T) {
	homelab := statusTestHost("homelab", "t3-steward", "drift")
	homelab["upkeeper"] = map[string]any{
		"holds": []any{}, "scheduled_runs": []any{},
		"timers": map[string]any{"upkeeper-self.timer": map[string]any{"enabled": true}},
	}
	document := map[string]any{"controller": "normandy", "hosts": []any{homelab}}
	annotateDriftReasons(document, driftEvidence{
		controller: "normandy",
		holds: map[string]selfconv.HoldMarker{"homelab": {
			Host: "homelab", Age: 3600 * 1e9, RunID: "20260917T140905Z-6b2c83ab",
		}},
	})
	reason, _ := statusTestReason(t, document, "homelab", "t3-steward")
	want := "scheduled hold marker present for 1h0m0s (left by run 20260917T140905Z-6b2c83ab); " +
		"observe the host, then clear it with: upkeeper hold clear homelab --reason TEXT; " +
		"no scheduled run selecting t3-steward recorded on homelab"
	if reason != want {
		t.Errorf("reason is\n%q\nexpected\n%q", reason, want)
	}
}
