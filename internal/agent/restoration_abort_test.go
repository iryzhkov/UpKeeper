package agent

import (
	"errors"
	"testing"
)

// An adapter that tried to put back what the run found, and could not prove it
// did, leaves the host in a state nobody can vouch for. Only some adapters say
// so with the words "rollback failed"; these are the others, each in the exact
// wording its adapter uses. Every one of them must stop the host, whatever the
// dependency table says, because a component applied after it builds on an
// unrestored host.
func TestAnIncompleteRestorationStopsTheHost(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		err     error
		state   string
		wording string
	}{
		// component_t3_artifact.go: settle after the files came back but the
		// previous service did not.
		{"t3 service restoration", Failf("files restored; previous service restoration failed: restart failed; backup /fixture/tx; cause health check failed"), "failed", "t3"},
		{"t3 service unverified", errors.New("files restored; service restoration unverified; journal persistence failed; backup /fixture/tx; cause x: disk full"), "failed", "t3"},
		// component_steward.go: the binary came back, the session hook did not.
		{"steward session hook", Failf("steward restored but session hook restoration failed"), "failed", "steward"},
		// component_steward_unit.go: errors.Join of the cause and the restore.
		{"steward unit", errors.Join(errors.New("pinned binary failed"), errors.New("restart restored Steward unit: exit 1")), "failed", "steward unit"},
		// component_jocasta.go and component_codex_controlled.go: a concurrent
		// change stopped the rollback and was left in place.
		{"jocasta concurrent state", Failf("Jocasta activation failed; concurrent state preserved for recovery at /fixture/backup"), "failed", "jocasta"},
		{"codex concurrent entry point", Failf("controlled Codex activation failed; concurrent entry point preserved"), "failed", "codex"},
		// internal/agentenv restore, through the environment adapter: an I/O
		// error while restoring is reported as a failed rollback.
		{"environment restore I/O", environmentFailure(errors.New("rollback failed: restoring .claude/settings.json: read /fixture/b: is a directory")), "rollback-failed", "agent-environment"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			run, c := newPartialRun(t, map[string][]string{}, "a", "b", "worker-configuration", "steward-fleet-configuration")
			run.outcomes["b"] = func() (string, error) { return "", testCase.err }
			result := Execute(c)
			if state := resultEntry(t, result, "b")["state"]; state != testCase.state {
				t.Errorf("b = %v, want %s", state, testCase.state)
			}
			for _, name := range []string{"worker-configuration", "steward-fleet-configuration"} {
				entry := resultEntry(t, result, name)
				want := "skipped: run aborted after " + testCase.state + " b"
				if entry["state"] != "skipped" || entry["aborted_by"] != "b" || entryText(entry, "error") != want {
					t.Errorf("%s ran on a host %s left unrestored: %v", name, testCase.wording, entry)
				}
				if run.calls[name] != 0 {
					t.Errorf("%s adapter called %d times after an incomplete restoration", name, run.calls[name])
				}
			}
			if state := resultEntry(t, result, "a")["state"]; state != "updated" {
				t.Errorf("a = %v, want the earlier component to have run", state)
			}
		})
	}
}

// A failure that never touched restoration keeps the partial-run behaviour:
// the guard above must not swallow ordinary failures.
func TestAnOrdinaryFailureDoesNotStopTheHost(t *testing.T) {
	for _, message := range []string{
		"health check failed for local-file-llm",
		"mise install node@24 failed: exit 1",
	} {
		t.Run(message, func(t *testing.T) {
			run, c := newPartialRun(t, map[string][]string{}, "b", "c")
			run.outcomes["b"] = failWith(message)
			result := Execute(c)
			if entry := resultEntry(t, result, "c"); entry["state"] != "updated" || run.calls["c"] == 0 {
				t.Errorf("c = %v, want it applied after an ordinary failure", entry)
			}
		})
	}
}
