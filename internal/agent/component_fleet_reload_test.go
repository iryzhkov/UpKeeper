package agent

import (
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// fleetTestCoordinatorContext pretends to be the coordinator host of the shared
// fixture, with a coordinator unit that systemd reports as active or not.
func fleetTestCoordinatorContext(t *testing.T, unitActive bool) (*Context, *adapterTestRunner) {
	t.Helper()
	runner := &adapterTestRunner{handle: func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "is-active", stewardCoordinatorUnit) {
			if unitActive {
				return procx.Result{Code: 0, Stdout: "active\n"}
			}
			return procx.Result{Code: 3, Stdout: "inactive\n"}
		}
		return procx.Result{}
	}}
	context := adapterTestContext(t, runner)
	document, err := fleet.Parse(fleetTestComponent(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	context.Host.Name = document.Coordinator.Host
	return context, runner
}

// The coordinator only re-reads its catalog input on SIGHUP or at start, and the
// steward component restarts the unit before this component writes the file.
// A changed catalog therefore asks the running coordinator to reload, once.
func TestCoordinatorCatalogChangeReloadsTheRunningCoordinator(t *testing.T) {
	context, runner := fleetTestCoordinatorContext(t, true)
	spec := fleetTestComponent(t)

	preview, err := fleetObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if runner.ran("kill", "HUP") {
		t.Fatal("an observation signalled the coordinator")
	}
	actions := []string{}
	state, err := fleetApply(context, spec, preview, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	if !runner.ran("systemctl", "--user", "kill", "-s", "HUP", stewardCoordinatorUnit) {
		t.Fatalf("the coordinator was not asked to reload: %v", runner.calls)
	}
	if !adapterTestMatches(actions, "reload its configuration") {
		t.Fatalf("actions %v do not report the reload", actions)
	}
	for _, argv := range runner.calls {
		if adapterTestMatches(argv, "restart") {
			t.Fatalf("a catalog change restarted a unit: %v", argv)
		}
	}

	// An unchanged catalog on the next run neither rewrites nor reloads.
	runner.calls = nil
	context.RunID = "20260914T000001Z-second"
	second := []string{}
	state, err = fleetApply(context, spec, preview, &second)
	if err != nil || state != "converged" {
		t.Fatalf("state = %q, error = %v, want converged", state, err)
	}
	if runner.ran("kill", "HUP") {
		t.Fatalf("an unchanged catalog signalled the coordinator: %v", runner.calls)
	}
	for _, action := range second {
		if strings.Contains(action, "reload") {
			t.Fatalf("an unchanged catalog reported a reload: %q", action)
		}
	}
}

// A coordinator that is not running reads the projection when it starts, so
// there is nothing to signal and the result says so instead of failing.
func TestCoordinatorCatalogChangeWithInactiveUnitDoesNotSignal(t *testing.T) {
	context, runner := fleetTestCoordinatorContext(t, false)
	spec := fleetTestComponent(t)
	result, err := fleetProjection(context, spec, context.Host.Name, context.RunID, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if runner.ran("kill", "HUP") {
		t.Fatalf("an inactive coordinator was signalled: %v", runner.calls)
	}
	reload, _ := result["coordinator_reload"].(string)
	if !strings.Contains(reload, "inactive") {
		t.Fatalf("coordinator_reload = %q, want the inactive explanation", reload)
	}
}
