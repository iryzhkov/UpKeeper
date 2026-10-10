package converge_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/operatorhold"
)

var holdSetAt = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

func holdHost(t *testing.T, directory, host string, until time.Time) {
	t.Helper()
	if _, err := operatorhold.Set(directory, operatorhold.Hold{
		Host: host, Reason: "rc120 rollout", SetBy: "igor", SetAt: holdSetAt, Until: until,
	}); err != nil {
		t.Fatal(err)
	}
}

// Scheduled and self-pull runs are automatic consumption: a held host is
// reported held and nothing is dispatched, locked or converged on it, while
// the other hosts of the same run carry on.
func TestAHeldHostIsNotDispatchedToByAnyRun(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	for _, command := range []string{"scheduled-pull", "self-pull", "pull"} {
		t.Run(command, func(t *testing.T) {
			choices := options(t, []string{"normandy", "homelab"})
			choices.Command = command
			choices.Now = holdSetAt.Add(time.Hour)
			holdHost(t, choices.HoldDirectory, "normandy", time.Time{})
			var seen []agent.Payload
			choices.Runner = stateRunner("converged", &seen)
			report, code, err := converge.Fleet(config, value, choices)
			if err != nil {
				t.Fatal(err)
			}
			if code != exitcode.Drift {
				t.Errorf("exit code %d, want %d", code, exitcode.Drift)
			}
			if len(seen) != 1 || seen[0].Host.Name != "homelab" || seen[0].OverrideHold {
				t.Fatalf("dispatched %+v", seen)
			}
			entry := report["hosts"].([]any)[0].(map[string]any)
			if entry["host"] != "normandy" || entry["result"] != "held" ||
				!strings.Contains(entry["error"].(string), "rc120 rollout") ||
				!strings.Contains(entry["error"].(string), "hold clear --hosts normandy") {
				t.Errorf("held entry %#v", entry)
			}
			if entry["completion"] != converge.CompletionNone {
				t.Errorf("held completion %#v", entry["completion"])
			}
		})
	}
}

// A dry run changes nothing, so a hold does not stop it, and an expired hold
// stops nothing at all.
func TestDryRunsAndExpiredHoldsAreNotHeld(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)

	dry := options(t, []string{"normandy"})
	dry.DryRun = true
	dry.Now = holdSetAt.Add(time.Hour)
	holdHost(t, dry.HoldDirectory, "normandy", time.Time{})
	var seen []agent.Payload
	dry.Runner = stateRunner("converged", &seen)
	if _, _, err := converge.Fleet(config, value, dry); err != nil || len(seen) != 1 {
		t.Fatalf("dry run: %v %+v", err, seen)
	}

	expired := options(t, []string{"normandy"})
	expired.Now = holdSetAt.Add(3 * time.Hour)
	holdHost(t, expired.HoldDirectory, "normandy", holdSetAt.Add(2*time.Hour))
	seen = nil
	expired.Runner = stateRunner("converged", &seen)
	if _, code, err := converge.Fleet(config, value, expired); err != nil || code != exitcode.Success || len(seen) != 1 {
		t.Fatalf("expired hold: code=%d err=%v seen=%+v", code, err, seen)
	}
}

// The explicit operator override dispatches, tells the agent so, says so in
// the report and logs it beside the hold.
func TestAnExplicitOverrideDispatchesAndIsLogged(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	choices := options(t, []string{"normandy"})
	choices.Command = "pull"
	choices.OverrideHold = true
	choices.Now = holdSetAt.Add(time.Hour)
	holdHost(t, choices.HoldDirectory, "normandy", time.Time{})
	var seen []agent.Payload
	choices.Runner = stateRunner("converged", &seen)
	report, code, err := converge.Fleet(config, value, choices)
	if err != nil || code != exitcode.Success {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(seen) != 1 || !seen[0].OverrideHold {
		t.Fatalf("dispatched %+v", seen)
	}
	entry := report["hosts"].([]any)[0].(map[string]any)
	overridden, _ := entry["hold_overridden"].(map[string]any)
	if overridden["reason"] != "rc120 rollout" {
		t.Errorf("report does not name the overridden hold: %#v", entry)
	}
	raw, err := os.ReadFile(operatorhold.LogPath(choices.HoldDirectory, "normandy"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"event":"override"`) || !strings.Contains(string(raw), choices.RunID) {
		t.Errorf("override not logged: %s", raw)
	}
	if _, held := operatorhold.Check(choices.HoldDirectory, "normandy", choices.Now); !held {
		t.Error("an override released the hold; it must stay until cleared")
	}
}
