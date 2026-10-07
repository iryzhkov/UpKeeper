package agent

import (
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// An adapter that tried to put back what the run found, and could not prove it
// did, leaves the host in a state nobody can vouch for, so the agent stops the
// host after it. The agent learns it from one place, rollbackFailed: every
// adapter says "rollback failed" or "rollback incomplete" for such an outcome.
// These tests drive the real adapters whose wording did not say so.

// abortsTheHost runs a host whose component b fails with err, followed by two
// independent components, and checks that the abort stopped both.
func abortsTheHost(t *testing.T, err error) {
	t.Helper()
	run, c := newPartialRun(t, map[string][]string{}, "a", "b", "worker-configuration", "steward-fleet-configuration")
	run.outcomes["b"] = func() (string, error) { return "", err }
	result := Execute(c)
	if state := resultEntry(t, result, "b")["state"]; state != "rollback-failed" {
		t.Errorf("b = %v, want rollback-failed for %v", state, err)
	}
	for _, name := range []string{"worker-configuration", "steward-fleet-configuration"} {
		entry := resultEntry(t, result, name)
		if entry["state"] != "skipped" || entry["aborted_by"] != "b" ||
			entryText(entry, "error") != "skipped: run aborted after rollback-failed b" {
			t.Errorf("%s ran on an unrestored host: %v", name, entry)
		}
		if run.calls[name] != 0 {
			t.Errorf("%s adapter called %d times after an incomplete restoration", name, run.calls[name])
		}
	}
}

// T3: the files came back, but the previous service never proved healthy
// ("files restored; previous service health not verified").
func TestAT3FilesRestoredOutcomeStopsTheHost(t *testing.T) {
	h := newT3CustodyHost(t)
	err, _ := h.filesRestored(t)
	if state, _ := classify(err); state != "rollback-failed" {
		t.Fatalf("files-restored T3 transaction classified %s: %v", state, err)
	}
	abortsTheHost(t, err)
}

// Steward unit: activation of the migrated unit failed, and restarting the
// restored original failed too, so the original is not back in force.
func TestAFailedStewardUnitRestoreStopsTheHost(t *testing.T) {
	c, runner := stewardTestPrepare(t, stewardTestHost{active: true, restartFailures: 2})
	originalHandle := runner.handle
	runner.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "--property=ProtectSystem") {
			return procx.Result{Stdout: "no\n"}
		}
		return originalHandle(command)
	}
	spec := stewardTestSpec(false)
	c.Components["t3-steward"] = spec
	adapterTestWrite(t, c.Path(".local/bin/t3-steward"), stewardTestBinary)
	adapterTestWrite(t, c.Path(stewardUnitPath), stewardLegacyUnitFixture)
	observed := map[string]any{"version": stewardTestVersion, "service_active": true}
	_, err := stewardApply(c, spec, observed, &[]string{})
	if err == nil {
		t.Fatal("a migration whose restart and restore restart both failed succeeded")
	}
	if state, _ := classify(err); state != "rollback-failed" {
		t.Fatalf("an unrestored Steward unit classified %s: %v", state, err)
	}
	abortsTheHost(t, err)
}

// The adapters whose one-line wording now says the rollback failed: each is a
// rollback that stopped at a state it could not put back.
func TestEveryIncompleteRestorationWordingStopsTheHost(t *testing.T) {
	for _, err := range []error{
		// component_steward.go: the binary came back, the session hook did not.
		Failf("rollback failed: steward restored but session hook restoration failed"),
		// component_jocasta.go and component_codex_controlled.go: a concurrent
		// change stopped the rollback and was left in place.
		Failf("rollback failed: Jocasta activation failed; concurrent state preserved for recovery at /fixture/backup"),
		Failf("rollback failed: controlled Codex activation failed; concurrent entry point preserved"),
		// internal/agentenv restore: an I/O error while restoring.
		environmentFailure(Failf("rollback failed: read /fixture/b: is a directory")),
	} {
		t.Run(err.Error(), func(t *testing.T) { abortsTheHost(t, err) })
	}
}

// A refusal before any effect leaves the host as the run found it, so it
// skips only the component's dependents, even when it names what it
// preserved or a record it could not restore from.
func TestARefusalBeforeAnyEffectDoesNotStopTheHost(t *testing.T) {
	for _, message := range []string{
		"local-file-llm health check: probe request 2 failed: connection refused",
		// component_tooling.go: a stage of unknown origin is set aside, not used.
		"incomplete tooling stage of unknown origin preserved at /fixture/stage; inspect it, then move it aside",
		// component_tooling.go: refused before the backup is written.
		"concurrent t3-backlog binding change; preserved /fixture/bin/t3-backlog",
		// component_fleet_guard.go: a retained record that fails validation.
		"retained fleet record has no content to restore",
		// component_steward_unit.go: a customised unit is left alone.
		"Steward unit is not the known generated legacy format; preserving customization",
	} {
		t.Run(message, func(t *testing.T) {
			run, c := newPartialRun(t, map[string][]string{}, "b", "c")
			run.outcomes["b"] = failWith(message)
			result := Execute(c)
			if state := resultEntry(t, result, "b")["state"]; state != "failed" {
				t.Errorf("b = %v, want failed", state)
			}
			if entry := resultEntry(t, result, "c"); entry["state"] != "updated" || run.calls["c"] == 0 {
				t.Errorf("c = %v, want it applied after a refusal that changed nothing", entry)
			}
		})
	}
}
