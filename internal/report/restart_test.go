package report

import (
	"reflect"
	"testing"
)

// A pending t3-steward worker restart the probe observed is a doctor notice.
// It does not regrade the host: the restart waits on purpose.
func TestDoctorReportsAPendingWorkerRestartWithoutRegradingTheHost(t *testing.T) {
	config := testConfig(t, map[string]any{"t3-steward": pinned(stewardCommit)})
	note := "worker restart pending since 2026-10-05T08:00:00Z (2h30m): 1 dispatched, " +
		"2 settle-pending; the restart waits for an idle worker"
	observed := mutated("normandy", func(document map[string]any) {
		components, _ := document["components"].(map[string]any)
		if components == nil {
			components = map[string]any{}
			document["components"] = components
		}
		steward, _ := components["t3-steward"].(map[string]any)
		if steward == nil {
			steward = map[string]any{}
			components["t3-steward"] = steward
		}
		steward["worker_restart"] = map[string]any{"note": note, "since": "2026-10-05T08:00:00Z"}
	})
	document, code := DoctorWith(config, map[string]any{"normandy": observed},
		[]string{"normandy"}, gitRunner(manifestHead, 0))
	entry := hostEntries(t, document)[0]
	if want := []any{"t3-steward: " + note}; !reflect.DeepEqual(entry["notices"], want) {
		t.Fatalf("notices = %#v, want %#v", entry["notices"], want)
	}
	healthyDocument, healthyCode := DoctorWith(config, map[string]any{"normandy": healthy("normandy")},
		[]string{"normandy"}, gitRunner(manifestHead, 0))
	healthyEntry := hostEntries(t, healthyDocument)[0]
	if entry["result"] != healthyEntry["result"] || code != healthyCode {
		t.Fatalf("a pending restart regraded the host: %v (%d) against %d", entry["result"], code, healthyCode)
	}
	if notices, _ := healthyEntry["notices"].([]any); len(notices) != 0 {
		t.Fatalf("a host without a pending restart has notices %v", notices)
	}
}
