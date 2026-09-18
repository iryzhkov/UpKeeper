package probe

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// The probe carries the host's own convergence evidence under
// components.upkeeper, so a controller can explain a remote host's drift from
// the same facts it reads locally for itself.
func TestCollectReportsTheUpkeeperEvidenceOfTheHost(t *testing.T) {
	home := collectFixture(t)
	holds := filepath.Join(home, ".local/state/dev-fleet/scheduled-holds")
	if err := os.MkdirAll(holds, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "Scheduled dispatch may have run (run 20260917T063000Z-0a1b2c3d). Observe the host before clearing.\n"
	if err := os.WriteFile(filepath.Join(holds, "normandy.hold"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := scriptedRunner(map[string]procx.Result{
		"systemctl --user show upkeeper-self.timer -p LoadState -p ActiveState -p UnitFileState -p NextElapseUSecRealtime": {
			Code: 0, Stdout: "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\nNextElapseUSecRealtime=Fri 2026-09-18 06:00:00 PDT\n",
		},
	})
	document := collect(home, runner)
	components := document["components"].(map[string]any)
	upkeeper, ok := components["upkeeper"].(map[string]any)
	if !ok {
		t.Fatalf("components has no upkeeper evidence: %v", components)
	}
	markers, _ := upkeeper["holds"].([]any)
	if len(markers) != 1 || markers[0].(map[string]any)["host"] != "normandy" ||
		markers[0].(map[string]any)["run_id"] != "20260917T063000Z-0a1b2c3d" {
		t.Errorf("holds = %v", markers)
	}
	if runs, _ := upkeeper["scheduled_runs"].([]any); len(runs) != 0 || upkeeper["last_scheduled_run"] != nil {
		t.Errorf("a host with no run store reports runs: %v", upkeeper)
	}
	timers, _ := upkeeper["timers"].(map[string]any)
	self, _ := timers["upkeeper-self.timer"].(map[string]any)
	if self["enabled"] != true || self["next_elapse"] != "Fri 2026-09-18 06:00:00 PDT" {
		t.Errorf("self timer = %v", self)
	}
	if pull, _ := timers["upkeeper-pull.timer"].(map[string]any); pull["enabled"] != nil {
		t.Errorf("a timer systemctl could not show reports enabled=%v", pull["enabled"])
	}
}
