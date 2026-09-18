package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/runstore"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
)

func statusTestHost(name string, states ...string) map[string]any {
	components := []any{}
	for index := 0; index+1 < len(states); index += 2 {
		components = append(components, map[string]any{"name": states[index], "state": states[index+1]})
	}
	return map[string]any{
		"host": name, "name": name, "reachable": true, "observe_only": true,
		"result": "drift", "components": components,
	}
}

func statusTestReason(t *testing.T, document map[string]any, host, component string) (string, bool) {
	t.Helper()
	for _, item := range list(document["hosts"]) {
		entry, _ := item.(map[string]any)
		if entry["host"] != host {
			continue
		}
		for _, raw := range list(entry["components"]) {
			candidate, _ := raw.(map[string]any)
			if candidate["name"] == component {
				reason, present := candidate["reason"]
				return text(reason), present
			}
		}
	}
	t.Fatalf("no component %s on %s", component, host)
	return "", false
}

// A drifting component gets the reason the controller can see for it: a hold
// marker it holds, the last run it recorded, its own timer. What it cannot
// see it says it cannot see, rather than guessing.
func TestStatusDriftReasonsComeFromWhatTheControllerCanSee(t *testing.T) {
	disabled := false
	document := map[string]any{"controller": "omarchy-pc", "hosts": []any{
		statusTestHost("omarchy-pc", "t3-steward", "drift", "huyang", "converged"),
		statusTestHost("laptop", "t3-steward", "drift"),
		statusTestHost("normandy", "t3-steward", "drift", "steward-fleet-configuration", "drift"),
		statusTestHost("homelab", "t3-steward", "drift"),
	}}
	evidence := driftEvidence{
		controller:       "omarchy-pc",
		selfTimerEnabled: &disabled,
		holds: map[string]selfconv.HoldMarker{"laptop": {
			Host: "laptop", Age: 26 * time.Hour, RunID: "20260917T140905Z-6b2c83ab",
		}},
		scheduledRuns: map[string][]lastRun{
			"omarchy-pc": {
				// The newest self-pull selected only huyang, so it says nothing
				// about the steward; the one before it does.
				{RunID: "20260917T170000Z-11111111", Command: "self-pull",
					CreatedAt: "2026-09-17T17:00:00Z", Selected: []string{"huyang"},
					HostResult: "converged", Components: map[string]componentOutcome{
						"huyang": {State: "converged"}}},
				{RunID: "20260917T160904Z-e81df506", Command: "self-pull",
					CreatedAt: "2026-09-17T16:09:04Z", Selected: []string{"huyang", "t3-steward", "t3"},
					HostResult: "updated", Components: map[string]componentOutcome{
						"t3-steward": {State: "updated"}}},
			},
			"normandy": {{RunID: "20260917T063000Z-0a1b2c3d", Command: "scheduled-pull",
				CreatedAt:  "2026-09-17T06:30:00Z",
				Selected:   []string{"t3-steward", "t3", "steward-fleet-configuration"},
				HostResult: "failed", Components: map[string]componentOutcome{
					"t3-steward":                  {State: "failed", Error: "running T3 0.0.40 is outside the candidate steward's T3 range 0.0.38..0.0.38; no safe pair transition"},
					"steward-fleet-configuration": {State: "deferred", Actions: []string{"coordinator busy; retry later"}},
				}}},
		},
	}
	annotateDriftReasons(document, evidence)

	expected := map[[2]string]string{
		{"omarchy-pc", "t3-steward"}: "upkeeper-self.timer is disabled on this host, so no scheduled run will converge it; " +
			"last self-pull run 20260917T160904Z-e81df506 (2026-09-17T16:09:04Z) reported t3-steward updated",
		{"laptop", "t3-steward"}: "scheduled hold marker present for 26h0m0s (left by run 20260917T140905Z-6b2c83ab); " +
			"observe the host, then clear it with: upkeeper hold clear laptop --reason TEXT; " +
			"no scheduled run record for laptop on this host",
		{"normandy", "t3-steward"}: "last scheduled-pull run 20260917T063000Z-0a1b2c3d (2026-09-17T06:30:00Z) failed t3-steward: " +
			"running T3 0.0.40 is outside the candidate steward's T3 range 0.0.38..0.0.38; no safe pair transition",
		{"normandy", "steward-fleet-configuration"}: "last scheduled-pull run 20260917T063000Z-0a1b2c3d (2026-09-17T06:30:00Z) deferred " +
			"steward-fleet-configuration: coordinator busy; retry later",
		{"homelab", "t3-steward"}: "reason unavailable from this host: no hold marker and no scheduled run record for homelab here; " +
			"inspect homelab's own run store and timer",
	}
	for key, want := range expected {
		reason, present := statusTestReason(t, document, key[0], key[1])
		if !present || reason != want {
			t.Errorf("%s %s reason is\n%q\nexpected\n%q", key[0], key[1], reason, want)
		}
	}
	if _, present := statusTestReason(t, document, "omarchy-pc", "huyang"); present {
		t.Error("a converged component was given a reason")
	}
}

// Lock contention and a hold refusal are host-level outcomes; the component
// never ran, and the reason says so.
func TestStatusDriftReasonUsesTheHostOutcomeWhenNoComponentRan(t *testing.T) {
	document := map[string]any{"controller": "normandy", "hosts": []any{
		statusTestHost("homelab", "t3-steward", "drift"),
	}}
	annotateDriftReasons(document, driftEvidence{
		controller: "normandy",
		scheduledRuns: map[string][]lastRun{"homelab": {{
			RunID: "20260917T063000Z-0a1b2c3d", Command: "scheduled-pull",
			CreatedAt: "2026-09-17T06:30:00Z", Selected: []string{"t3-steward", "t3"},
			HostResult: "failed",
			HostError:  "scheduled hold requires operator observation: /home/igor/.local/state/dev-fleet/scheduled-holds/homelab.hold",
		}}},
	})
	reason, _ := statusTestReason(t, document, "homelab", "t3-steward")
	want := "last scheduled-pull run 20260917T063000Z-0a1b2c3d (2026-09-17T06:30:00Z) did not reach t3-steward: host failed: " +
		"scheduled hold requires operator observation: /home/igor/.local/state/dev-fleet/scheduled-holds/homelab.hold"
	if reason != want {
		t.Errorf("reason is\n%q\nexpected\n%q", reason, want)
	}
}

// Only scheduled runs are evidence about the scheduled pull: a manual pull
// recorded after the last self-pull is passed over, and old records written
// before runs named their command are not scheduled evidence either.
func TestDriftEvidenceReadsOnlyScheduledRunRecords(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := &runstore.Store{Root: filepath.Join(home, ".local", "state", "dev-fleet", "runs")}
	write := func(runID, command, result string, components []string) {
		t.Helper()
		record, err := runstore.New(runID, command, "omarchy-pc", nil, components, []string{"omarchy-pc"})
		if err != nil {
			t.Fatal(err)
		}
		record.Phase, record.Result = "finished", result
		record.Details = map[string]any{"hosts": []any{map[string]any{
			"host": "omarchy-pc", "result": "failed",
			"components": []any{map[string]any{"name": "t3-steward", "state": "failed", "error": command + " failure"}},
		}}}
		if _, err := store.Write(record); err != nil {
			t.Fatal(err)
		}
	}
	write("20260917T060000Z-00000001", "pull", "exit-40", []string{"t3-steward"})
	write("20260917T090000Z-00000002", "self-pull", "exit-40", []string{"t3-steward", "t3"})
	write("20260917T120000Z-00000003", "pull", "exit-40", []string{"t3-steward"})
	write("20260917T130000Z-00000004", "self-pull", "exit-40", []string{"huyang"})

	evidence := gatherDriftEvidence("omarchy-pc", func(procx.Command) procx.Result {
		return procx.Result{Stdout: "enabled\n"}
	})
	runs := evidence.scheduledRuns["omarchy-pc"]
	if len(runs) != 2 || runs[0].RunID != "20260917T130000Z-00000004" || runs[1].RunID != "20260917T090000Z-00000002" {
		t.Fatalf("scheduled runs are %#v", runs)
	}
	if evidence.selfTimerEnabled == nil || !*evidence.selfTimerEnabled {
		t.Errorf("the enabled timer was not observed: %#v", evidence.selfTimerEnabled)
	}
	document := map[string]any{"controller": "omarchy-pc", "hosts": []any{
		statusTestHost("omarchy-pc", "t3-steward", "drift"),
	}}
	annotateDriftReasons(document, evidence)
	reason, _ := statusTestReason(t, document, "omarchy-pc", "t3-steward")
	if !strings.HasPrefix(reason, "last self-pull run 20260917T090000Z-00000002 (") ||
		!strings.HasSuffix(reason, ") failed t3-steward: self-pull failure") {
		t.Errorf("reason is %q", reason)
	}
}
