package agent

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/prereq"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// These tests replay agent-a's pull on 2026-10-07: the agent-tooling health
// check failed, and every later component, steward-fleet-configuration
// included, was skipped although none of them needed the tooling.

// partialRun is a host whose adapters are stubs. Each adapter reports the
// outcome a test gives it and counts its calls. Without an outcome an adapter
// reports "updated" the first time it applies and "converged" after that.
type partialRun struct {
	outcomes map[string]func() (string, error)
	calls    map[string]int
	applied  map[string]bool
}

// newPartialRun stubs every named adapter, removes OS prerequisites and
// credentials so that nothing is blocked, and replaces the dependency table
// when dependencies is not nil.
func newPartialRun(t *testing.T, dependencies map[string][]string, names ...string) (*partialRun, *Context) {
	t.Helper()
	if dependencies != nil {
		priorDependencies := componentDependencies
		componentDependencies = dependencies
		t.Cleanup(func() { componentDependencies = priorDependencies })
	}
	priorPrerequisites, priorCredentials := componentPrerequisites, componentCredentials
	componentPrerequisites = func(string) []prereq.Prerequisite { return nil }
	componentCredentials = map[string][]credential{}
	t.Cleanup(func() {
		componentPrerequisites, componentCredentials = priorPrerequisites, priorCredentials
	})
	run := &partialRun{
		outcomes: map[string]func() (string, error){}, calls: map[string]int{}, applied: map[string]bool{},
	}
	components := map[string]any{}
	for _, name := range names {
		name := name
		components[name] = map[string]any{}
		replaceHandler(t, name, Handler{
			Observe: func(*Context, map[string]any) (map[string]any, error) {
				run.calls[name]++
				return map[string]any{}, nil
			},
			Apply: func(*Context, map[string]any, map[string]any, *[]string) (string, error) {
				run.calls[name]++
				if outcome, ok := run.outcomes[name]; ok {
					return outcome()
				}
				if run.applied[name] {
					return "converged", nil
				}
				run.applied[name] = true
				return "updated", nil
			},
		})
	}
	home := t.TempDir()
	bin := freshTestTools(t, home, freshTestCommands...)
	c := freshTestContext(t, home, bin, func(procx.Command) procx.Result { return procx.Result{} },
		slices.Clone(names), components)
	return run, c
}

func failWith(message string) func() (string, error) {
	return func() (string, error) { return "", Failf("%s", message) }
}

func entryText(entry map[string]any, key string) string {
	value, _ := entry[key].(string)
	return value
}

func TestAFailedComponentSkipsOnlyItsDependents(t *testing.T) {
	run, c := newPartialRun(t, map[string][]string{"d": {"b"}}, "b", "c", "d")
	run.outcomes["b"] = failWith("health check failed")
	result := Execute(c)
	if state := resultEntry(t, result, "b")["state"]; state != "failed" {
		t.Errorf("b = %v", state)
	}
	if state := resultEntry(t, result, "c")["state"]; state != "updated" {
		t.Errorf("c = %v, want the independent component to run", state)
	}
	d := resultEntry(t, result, "d")
	if d["state"] != "skipped" || d["skipped_dependency"] != "b" || d["skipped_root"] != "b" {
		t.Errorf("d = %v", d)
	}
	if message := entryText(d, "error"); message != "skipped: depends on failed b" {
		t.Errorf("d skip reason = %q", message)
	}
	if _, aborted := d["aborted_by"]; aborted {
		t.Errorf("a dependency skip claims an abort: %v", d)
	}
	if run.calls["d"] != 0 || run.calls["c"] == 0 {
		t.Errorf("calls = %v", run.calls)
	}
	if result["result"] != "failed" {
		t.Errorf("host result = %v", result["result"])
	}
}

func TestARerunAfterTheFixConverges(t *testing.T) {
	run, c := newPartialRun(t, map[string][]string{"d": {"b"}}, "b", "c", "d")
	run.outcomes["b"] = failWith("health check failed")
	if first := Execute(c); first["result"] != "failed" {
		t.Fatalf("first run = %v", first)
	}
	delete(run.outcomes, "b")
	second := Execute(c)
	// c already converged in the failed run; b and d converge now.
	want := map[string]string{"b": "updated", "c": "converged", "d": "updated"}
	for name, state := range want {
		if entry := resultEntry(t, second, name); entry["state"] != state {
			t.Errorf("%s = %v after the fix, want %s", name, entry, state)
		}
	}
	if second["result"] != "updated" {
		t.Errorf("host result after the fix = %v", second["result"])
	}
}

func TestTransitiveSkipNamesTheRootCause(t *testing.T) {
	run, c := newPartialRun(t, map[string][]string{"d": {"b"}, "e": {"d"}}, "b", "c", "d", "e")
	run.outcomes["b"] = failWith("health check failed")
	result := Execute(c)
	e := resultEntry(t, result, "e")
	if e["state"] != "skipped" || e["skipped_dependency"] != "d" || e["skipped_root"] != "b" {
		t.Errorf("e = %v", e)
	}
	if message := entryText(e, "error"); message != "skipped: depends on skipped d; root cause failed b" {
		t.Errorf("e skip reason = %q", message)
	}
	if run.calls["d"] != 0 || run.calls["e"] != 0 {
		t.Errorf("calls = %v", run.calls)
	}
}

func TestRolledBackAndDeferredSkipOnlyDependents(t *testing.T) {
	for _, testCase := range []struct {
		state   string
		outcome func() (string, error)
	}{
		{"rolled-back", func() (string, error) { return "", RolledBackf("restored the previous build") }},
		{"deferred", func() (string, error) { return "deferred", nil }},
	} {
		t.Run(testCase.state, func(t *testing.T) {
			run, c := newPartialRun(t, map[string][]string{"d": {"b"}}, "b", "c", "d")
			run.outcomes["b"] = testCase.outcome
			result := Execute(c)
			if state := resultEntry(t, result, "b")["state"]; state != testCase.state {
				t.Errorf("b = %v", state)
			}
			if state := resultEntry(t, result, "c")["state"]; state != "updated" {
				t.Errorf("c = %v", state)
			}
			d := resultEntry(t, result, "d")
			want := "skipped: depends on " + testCase.state + " b"
			if d["state"] != "skipped" || d["skipped_dependency"] != "b" || entryText(d, "error") != want {
				t.Errorf("d = %v, want error %q", d, want)
			}
			if run.calls["d"] != 0 {
				t.Errorf("calls = %v", run.calls)
			}
			if result["result"] != testCase.state {
				t.Errorf("host result = %v", result["result"])
			}
		})
	}
}

// legacyUpdaterRunner answers systemctl for a host whose legacy updater
// retirement ends in state.
func legacyUpdaterRunner(state string) procx.Runner {
	return func(command procx.Command) procx.Result {
		if state == "failed" {
			return procx.Result{Code: 1, Stderr: "systemctl is not available in this test"}
		}
		if slices.Contains(command.Argv, "show") {
			return procx.Result{Stdout: "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\nFragmentPath=\n"}
		}
		return procx.Result{}
	}
}

func TestAbortClassOutcomesStillStopTheHost(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		prepare   func(run *partialRun, c *Context)
		selected  []string
		later     []string
		abortedBy string
		reason    string
		result    string
	}{
		{
			name: "rollback-failed",
			prepare: func(run *partialRun, _ *Context) {
				run.outcomes["b"] = failWith("rollback failed: the previous build is gone")
			},
			selected: []string{"a", "b", "c", "d"}, later: []string{"c", "d"},
			abortedBy: "b", reason: "skipped: run aborted after rollback-failed b",
			result: "rollback-failed",
		},
		// Adapters that name what they restored before saying it failed.
		{
			name: "jocasta rollback failed",
			prepare: func(run *partialRun, _ *Context) {
				run.outcomes["b"] = failWith("Jocasta rollback failed; inspect /home/x/.local/share/jocasta.backup")
			},
			selected: []string{"a", "b", "c", "d"}, later: []string{"c", "d"},
			abortedBy: "b", reason: "skipped: run aborted after rollback-failed b",
			result: "rollback-failed",
		},
		{
			name: "controlled codex rollback failed",
			prepare: func(run *partialRun, _ *Context) {
				run.outcomes["b"] = func() (string, error) {
					return "", errors.New("controlled Codex tree rollback failed")
				}
			},
			selected: []string{"a", "b", "c", "d"}, later: []string{"c", "d"},
			abortedBy: "b", reason: "skipped: run aborted after rollback-failed b",
			result: "rollback-failed",
		},
		{
			name: "t3 rollback incomplete",
			prepare: func(run *partialRun, _ *Context) {
				run.outcomes["b"] = failWith("rollback incomplete; backup /home/x/t3-tx; cause restart failed")
			},
			selected: []string{"a", "b", "c", "d"}, later: []string{"c", "d"},
			abortedBy: "b", reason: "skipped: run aborted after rollback-failed b",
			result: "rollback-failed",
		},
		{
			name:     "unknown adapter",
			prepare:  func(*partialRun, *Context) {},
			selected: []string{"a", "no-such-adapter", "c", "d"}, later: []string{"c", "d"},
			abortedBy: "no-such-adapter", reason: "skipped: run aborted after failed no-such-adapter",
			result: "failed",
		},
		{
			name: "legacy updater failed",
			prepare: func(_ *partialRun, c *Context) {
				c.Payload.RetireUpdater = true
				c.Runner = legacyUpdaterRunner("failed")
			},
			selected: []string{"a", "c", "d"}, later: []string{"a", "c", "d"},
			abortedBy: "legacy-updater", reason: "skipped: run aborted after failed legacy-updater",
			result: "failed",
		},
		{
			name: "legacy updater deferred",
			prepare: func(_ *partialRun, c *Context) {
				c.Payload.RetireUpdater = true
				c.Runner = legacyUpdaterRunner("deferred")
			},
			selected: []string{"a", "c", "d"}, later: []string{"a", "c", "d"},
			abortedBy: "legacy-updater", reason: "skipped: run aborted after deferred legacy-updater",
			result: "deferred",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stubbed := []string{}
			for _, name := range testCase.selected {
				if name != "no-such-adapter" {
					stubbed = append(stubbed, name)
				}
			}
			run, c := newPartialRun(t, map[string][]string{}, stubbed...)
			c.Selected = testCase.selected
			testCase.prepare(run, c)
			result := Execute(c)
			for _, name := range testCase.later {
				entry := resultEntry(t, result, name)
				if entry["state"] != "skipped" || entry["aborted_by"] != testCase.abortedBy ||
					entryText(entry, "error") != testCase.reason {
					t.Errorf("%s = %v", name, entry)
				}
				if _, dependency := entry["skipped_dependency"]; dependency {
					t.Errorf("%s: an abort skip names a dependency: %v", name, entry)
				}
				if run.calls[name] != 0 {
					t.Errorf("%s adapter called after the abort: %v", name, run.calls)
				}
			}
			if result["result"] != testCase.result {
				t.Errorf("host result = %v", result["result"])
			}
		})
	}
}

func TestAToolingFailureStillConvergesTheFleetConfiguration(t *testing.T) {
	for _, testCase := range []struct {
		failing string
		skipped []string
	}{
		{release.ToolingComponent, nil},
		{"t3-steward", []string{"t3", "worker-configuration", fleet.Component}},
	} {
		t.Run(testCase.failing, func(t *testing.T) {
			run, c := newPartialRun(t, nil, release.ComponentOrder...)
			run.outcomes[testCase.failing] = failWith("health check failed")
			result := Execute(c)
			for _, name := range release.ComponentOrder {
				entry := resultEntry(t, result, name)
				switch {
				case name == testCase.failing:
					if entry["state"] != "failed" {
						t.Errorf("%s = %v", name, entry)
					}
				case slices.Contains(testCase.skipped, name):
					message := entryText(entry, "error")
					if entry["state"] != "skipped" || entry["skipped_root"] != testCase.failing ||
						!strings.Contains(message, "failed "+testCase.failing) || run.calls[name] != 0 {
						t.Errorf("%s = %v, calls %d", name, entry, run.calls[name])
					}
				default:
					if entry["state"] != "updated" || run.calls[name] == 0 {
						t.Errorf("%s = %v, calls %d; want it applied", name, entry, run.calls[name])
					}
				}
			}
			if result["result"] != "failed" {
				t.Errorf("host result = %v", result["result"])
			}
		})
	}
}

func TestComponentDependenciesPointToEarlierComponents(t *testing.T) {
	for name, dependencies := range componentDependencies {
		position := slices.Index(release.ComponentOrder, name)
		if position < 0 {
			t.Errorf("%s is not a release component", name)
			continue
		}
		for _, dependency := range dependencies {
			earlier := slices.Index(release.ComponentOrder, dependency)
			if earlier < 0 || earlier >= position {
				t.Errorf("%s depends on %s, which is not an earlier release component", name, dependency)
			}
		}
	}
}
