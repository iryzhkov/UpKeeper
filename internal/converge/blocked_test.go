package converge_test

import (
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
)

// A host blocked on something only its operator can provide is drift, not a
// deployment failure, and any failure still outranks it.
func TestABlockedHostIsDrift(t *testing.T) {
	if code := converge.ExitCodeFor([]any{host("a", true, "blocked")}); code != exitcode.Drift {
		t.Fatalf("blocked = %d, want %d", code, exitcode.Drift)
	}
	if code := converge.ExitCodeFor([]any{host("a", true, "blocked"), host("b", true, "failed")}); code != exitcode.Deployment {
		t.Fatalf("blocked and failed = %d, want %d", code, exitcode.Deployment)
	}
}

// The inventory's opt-in travels to the agent; without it the payload is what
// it always was.
func TestThePayloadCarriesTheOSPackageOptIn(t *testing.T) {
	value := fleetRelease(t)
	config := fleetConfig(t)
	spec := config.Hosts["normandy"]
	if spec.InstallOSPackages {
		t.Fatal("the fixture host already opted in")
	}
	for _, opted := range []bool{false, true} {
		spec.InstallOSPackages = opted
		config.Hosts["normandy"] = spec
		choices := options(t, []string{"normandy"})
		choices.DryRun = true
		var seen []agent.Payload
		choices.Runner = stateRunner("converged", &seen)
		if _, _, err := converge.Fleet(config, value, choices); err != nil {
			t.Fatal(err)
		}
		if len(seen) != 1 || seen[0].Host.InstallOSPackages != opted {
			t.Fatalf("opt-in %v produced %+v", opted, seen)
		}
	}
}
