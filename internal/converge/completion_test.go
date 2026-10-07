package converge_test

import (
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
)

func components(states ...string) []any {
	entries := make([]any, 0, len(states))
	for index, state := range states {
		entries = append(entries, map[string]any{
			"name": "component-" + string(rune('a'+index)), "state": state,
		})
	}
	return entries
}

func hostWith(result string, states ...string) map[string]any {
	return map[string]any{
		"host": "normandy", "reachable": true, "result": result,
		"components": components(states...),
	}
}

func TestCompletionValues(t *testing.T) {
	cases := []struct {
		name string
		host map[string]any
		want string
	}{
		{"every component succeeded", hostWith("updated", "converged", "updated", "ahead"), "complete"},
		{"a host with zero components", hostWith("converged"), "complete"},
		{"a host without a component list", map[string]any{"host": "a", "reachable": true, "result": "converged"}, "complete"},
		{"one failed, one updated", hostWith("failed", "failed", "updated"), "partial"},
		{"a dependency skip", hostWith("failed", "converged", "failed", "skipped"), "partial"},
		{"an old agent that stopped", hostWith("failed", "converged", "failed", "skipped", "skipped"), "partial"},
		{"deferred beside converged", hostWith("deferred", "deferred", "converged"), "partial"},
		{"blocked beside converged", hostWith("blocked", "converged", "blocked"), "partial"},
		{"rolled back beside updated", hostWith("rolled-back", "updated", "rolled-back"), "partial"},
		{"rollback failed beside converged", hostWith("rollback-failed", "converged", "rollback-failed"), "partial"},
		{"everything skipped after an abort", hostWith("failed", "skipped", "skipped"), "none"},
		{"the only component failed", hostWith("failed", "failed"), "none"},
		{"failed before its components ran", hostWith("failed"), "none"},
		{"locked", map[string]any{"host": "a", "reachable": true, "result": "lock-contention"}, "none"},
		{"unreachable", map[string]any{"host": "a", "reachable": false, "error": "no route"}, "none"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := converge.HostCompletion(testCase.host); got != testCase.want {
				t.Errorf("HostCompletion = %q, want %q", got, testCase.want)
			}
		})
	}

	complete := hostWith("converged", "converged")
	partial := hostWith("failed", "failed", "converged")
	none := hostWith("failed", "failed")
	selfManaged := map[string]any{"host": "laptop", "reachable": false, "result": "skipped", "self_managed": true}
	skippedUnreachable := map[string]any{
		"host": "homelab", "reachable": false, "result": "skipped", "skipped_unreachable": true,
	}
	reports := []struct {
		name  string
		hosts []any
		want  string
	}{
		{"every host complete", []any{complete, complete}, "complete"},
		{"every host none", []any{none, none}, "none"},
		{"one partial host", []any{complete, partial}, "partial"},
		{"complete beside none", []any{complete, none}, "partial"},
		// A host the run deliberately did not dispatch to is not counted,
		// just as it does not change the exit code.
		{"complete beside a self-managed host", []any{selfManaged, complete}, "complete"},
		{"complete beside a skipped unreachable host", []any{skippedUnreachable, complete}, "complete"},
		{"no hosts", []any{}, "complete"},
	}
	for _, testCase := range reports {
		t.Run("report/"+testCase.name, func(t *testing.T) {
			for _, item := range testCase.hosts {
				host := item.(map[string]any)
				host["completion"] = converge.HostCompletion(host)
			}
			if got := converge.ReportCompletion(testCase.hosts); got != testCase.want {
				t.Errorf("ReportCompletion = %q, want %q", got, testCase.want)
			}
		})
	}
}

func componentRunner(result string, entries []any) converge.HostRunner {
	return func(spec inventory.HostSpec, _ agent.Payload, _ time.Duration, _ bool) map[string]any {
		return map[string]any{
			"host": spec.Name, "reachable": true, "result": result, "components": entries,
		}
	}
}

func TestFleetReportsPartialCompletion(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	cases := []struct {
		name    string
		result  string
		entries []any
		code    int
	}{
		{"a failure beside an update", "failed", components("failed", "updated"), exitcode.Deployment},
		{"a failed rollback beside a convergence", "rollback-failed",
			components("converged", "rollback-failed", "skipped"), exitcode.Rollback},
		// An agent from before partial runs stops at the failure and reports
		// every later component as a bare skip.
		{"an old agent that stopped", "failed",
			components("converged", "failed", "skipped", "skipped"), exitcode.Deployment},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			choices := options(t, []string{"normandy"})
			choices.Runner = componentRunner(testCase.result, testCase.entries)
			report, code, err := converge.Fleet(config, value, choices)
			if err != nil {
				t.Fatalf("Fleet: %v", err)
			}
			if code != testCase.code {
				t.Errorf("exit code is %d, want %d", code, testCase.code)
			}
			if report["completion"] != "partial" {
				t.Errorf("report completion is %v", report["completion"])
			}
			hosts, _ := report["hosts"].([]any)
			if entry, _ := hosts[0].(map[string]any); entry["completion"] != "partial" || entry["result"] != testCase.result {
				t.Errorf("host entry is %#v", entry)
			}
			record, err := choices.RecordStore.Read(choices.RunID)
			if err != nil {
				t.Fatalf("the run record is missing: %v", err)
			}
			details, _ := record["details"].(map[string]any)
			if details["completion"] != "partial" {
				t.Errorf("record details are %#v", details)
			}
		})
	}

	t.Run("status reports no completion", func(t *testing.T) {
		choices := options(t, []string{"normandy"})
		choices.ObserveOnly = true
		choices.Runner = componentRunner("ahead", components("ahead", "converged"))
		report, _, err := converge.Fleet(config, value, choices)
		if err != nil {
			t.Fatalf("Fleet: %v", err)
		}
		if _, present := report["completion"]; present {
			t.Errorf("an observation reports completion: %#v", report)
		}
		hosts, _ := report["hosts"].([]any)
		if entry, _ := hosts[0].(map[string]any); entry["completion"] != nil {
			t.Errorf("an observed host reports completion: %#v", entry)
		}
	})
}
