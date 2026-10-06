package report

import (
	"reflect"
	"testing"
)

// A missing OS prerequisite is a blocker for a converge, named with the
// command that installs it. It does not regrade a host doctor finds healthy.
func TestDoctorNamesPrerequisiteBlockersWithoutRegradingTheHost(t *testing.T) {
	config := testConfig(t, map[string]any{"t3-steward": pinned(stewardCommit)})
	observed := mutated("normandy", func(document map[string]any) {
		document["prerequisites"] = map[string]any{
			"family": "debian",
			"missing": map[string]any{
				"huyang": []any{map[string]any{"name": "go", "install": "sudo apt-get install -y golang-go", "staging": true}},
				"agent-tooling": []any{
					map[string]any{"name": "python3-venv", "install": "sudo apt-get install -y python3-venv", "staging": true},
				},
			},
		}
	})
	document, code := DoctorWith(config, map[string]any{"normandy": observed},
		[]string{"normandy"}, gitRunner(manifestHead, 0))
	entry := hostEntries(t, document)[0]
	want := []any{
		map[string]any{"component": "agent-tooling", "name": "python3-venv", "install": "sudo apt-get install -y python3-venv"},
		map[string]any{"component": "huyang", "name": "go", "install": "sudo apt-get install -y golang-go"},
	}
	if !reflect.DeepEqual(entry["blockers"], want) {
		t.Fatalf("blockers = %#v", entry["blockers"])
	}
	healthyDocument, healthyCode := DoctorWith(config, map[string]any{"normandy": healthy("normandy")},
		[]string{"normandy"}, gitRunner(manifestHead, 0))
	if entry["result"] != hostEntries(t, healthyDocument)[0]["result"] || code != healthyCode {
		t.Fatalf("blockers regraded the host: %v (%d) against %d", entry["result"], code, healthyCode)
	}
}
