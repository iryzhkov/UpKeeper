package hostevidence_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/hostevidence"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/runstore"
)

func writeRun(t *testing.T, store *runstore.Store, runID, command, result string, components []string, hosts []any) {
	t.Helper()
	record, err := runstore.New(runID, command, "homelab", nil, components, []string{"homelab"})
	if err != nil {
		t.Fatal(err)
	}
	record.Phase, record.Result = "finished", result
	record.CreatedAt = "2026-09-17T06:30:00Z"
	record.Details = map[string]any{"hosts": hosts}
	if _, err := store.Write(record); err != nil {
		t.Fatal(err)
	}
}

// The evidence a host keeps about its own scheduled convergence: the newest
// scheduled runs from its run store (a manual pull is not one), every hold
// marker it holds with the run that left it, and the state of both timers.
func TestCollectReportsTheHostsOwnRunsHoldsAndTimers(t *testing.T) {
	home := t.TempDir()
	store := &runstore.Store{Root: filepath.Join(home, ".local", "state", "dev-fleet", "runs")}
	writeRun(t, store, "20260917T050000Z-00000001", "pull", "exit-40", []string{"t3-steward"}, []any{})
	writeRun(t, store, "20260917T063000Z-0a1b2c3d", "self-pull", "exit-40", []string{"t3-steward", "t3"}, []any{
		map[string]any{"host": "homelab", "result": "failed", "components": []any{
			map[string]any{"name": "t3-steward", "state": "failed", "error": "no safe pair transition"},
		}},
	})
	holds := filepath.Join(home, ".local", "state", "dev-fleet", "scheduled-holds")
	if err := os.MkdirAll(holds, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(holds, "homelab.hold")
	if err := os.WriteFile(marker, []byte("Scheduled dispatch may have run (run 20260917T063000Z-0a1b2c3d). Observe the host before clearing.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	written := time.Now().Add(-26 * time.Hour)
	if err := os.Chtimes(marker, written, written); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(holds, "homelab.hold.cleared"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := func(command procx.Command) procx.Result {
		switch strings.Join(command.Argv, " ") {
		case "systemctl --user show upkeeper-self.timer -p LoadState -p ActiveState -p UnitFileState -p NextElapseUSecRealtime":
			return procx.Result{Code: 0, Stdout: "LoadState=loaded\nActiveState=active\nUnitFileState=disabled\nNextElapseUSecRealtime=Fri 2026-09-18 06:00:00 PDT\n"}
		case "systemctl --user show upkeeper-pull.timer -p LoadState -p ActiveState -p UnitFileState -p NextElapseUSecRealtime":
			return procx.Result{Code: 0, Stdout: "LoadState=not-found\nActiveState=inactive\nUnitFileState=\nNextElapseUSecRealtime=\n"}
		}
		return procx.Result{Code: 1}
	}
	document := hostevidence.Collect(home, runner, time.Now())

	runs, _ := document["scheduled_runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("scheduled_runs = %v, want only the self-pull", runs)
	}
	last, _ := document["last_scheduled_run"].(map[string]any)
	if last["run_id"] != "20260917T063000Z-0a1b2c3d" || last["command"] != "self-pull" ||
		last["created_at"] != "2026-09-17T06:30:00Z" || last["result"] != "exit-40" {
		t.Errorf("last_scheduled_run = %v", last)
	}
	if failed, _ := last["failed_components"].([]any); len(failed) != 1 || failed[0] != "homelab t3-steward failed" {
		t.Errorf("failed_components = %v", last["failed_components"])
	}
	hostEntries, _ := last["hosts"].([]any)
	if len(hostEntries) != 1 {
		t.Fatalf("hosts = %v", last["hosts"])
	}
	components, _ := hostEntries[0].(map[string]any)["components"].([]any)
	if len(components) != 1 || components[0].(map[string]any)["error"] != "no safe pair transition" {
		t.Errorf("the run's component outcomes were not carried: %v", hostEntries[0])
	}

	markers, _ := document["holds"].([]any)
	if len(markers) != 1 {
		t.Fatalf("holds = %v, want the one marker and not its cleared log", markers)
	}
	hold := markers[0].(map[string]any)
	if hold["host"] != "homelab" || hold["run_id"] != "20260917T063000Z-0a1b2c3d" || hold["path"] != marker {
		t.Errorf("hold = %v", hold)
	}
	if age, _ := hold["age_seconds"].(int); age < 26*3600-5 || age > 26*3600+5 {
		t.Errorf("age_seconds = %v, want about 26h", hold["age_seconds"])
	}

	timers, _ := document["timers"].(map[string]any)
	self, _ := timers["upkeeper-self.timer"].(map[string]any)
	if self["enabled"] != false || self["active"] != true || self["next_elapse"] != "Fri 2026-09-18 06:00:00 PDT" {
		t.Errorf("self timer = %v", self)
	}
	pull, _ := timers["upkeeper-pull.timer"].(map[string]any)
	if pull["enabled"] != false || pull["active"] != false || pull["next_elapse"] != nil || pull["load_state"] != "not-found" {
		t.Errorf("pull timer = %v", pull)
	}
}

// A host with no state directory and no runner reports empty evidence, never
// an error: the absence is itself what status reports.
func TestCollectOnAnEmptyHomeReportsNothing(t *testing.T) {
	document := hostevidence.Collect(t.TempDir(), nil, time.Now())
	if runs, _ := document["scheduled_runs"].([]any); len(runs) != 0 {
		t.Errorf("scheduled_runs = %v", runs)
	}
	if document["last_scheduled_run"] != nil {
		t.Errorf("last_scheduled_run = %v", document["last_scheduled_run"])
	}
	if holds, _ := document["holds"].([]any); len(holds) != 0 {
		t.Errorf("holds = %v", holds)
	}
	timers, _ := document["timers"].(map[string]any)
	if self, _ := timers["upkeeper-self.timer"].(map[string]any); self["enabled"] != nil {
		t.Errorf("an unobserved timer reports enabled=%v", self["enabled"])
	}
}
