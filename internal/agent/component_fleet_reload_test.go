package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// fleetReloadFake plays the coordinator on the other side of the signal. Under
// a fake clock the convergence never waits for real: every poll advances the
// clock by the poll interval, so a receipt that never comes times out at once.
type fleetReloadFake struct {
	clock time.Time
	// receipt is written when the unit is signalled, stamped at the clock
	// unless stale is set, in which case it is stamped before the signal.
	receipt *fleetReloadReceipt
	stale   bool
	// delay is how many polls the receipt takes to appear after the signal.
	delay int
}

// fleetTestCoordinatorContext pretends to be the coordinator host of the shared
// fixture, with a coordinator unit that systemd reports as active or not, and
// a fake coordinator that answers a HUP with the receipt the test chose. The
// default answer is an accepted receipt.
func fleetTestCoordinatorContext(t *testing.T, unitActive bool) (*Context, *adapterTestRunner, *fleetReloadFake) {
	t.Helper()
	fake := &fleetReloadFake{
		clock:   time.Date(2026, 9, 18, 5, 0, 0, 0, time.UTC),
		receipt: &fleetReloadReceipt{Outcome: "accepted", ConfigurationDigest: "digest-after", PreviousDigest: "digest-before", Release: "rc.69"},
	}
	var context *Context
	pending := 0
	write := func() {
		if fake.receipt == nil {
			return
		}
		receipt := *fake.receipt
		requested := fake.clock
		if fake.stale {
			requested = fake.clock.Add(-time.Hour)
		}
		receipt.RequestedAt = requested.Format(time.RFC3339Nano)
		receipt.CompletedAt = requested.Add(20 * time.Millisecond).Format(time.RFC3339Nano)
		raw, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		path := context.Path(fleetReloadReceiptPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &adapterTestRunner{handle: func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "is-active", stewardCoordinatorUnit) {
			if unitActive {
				return procx.Result{Code: 0, Stdout: "active\n"}
			}
			return procx.Result{Code: 3, Stdout: "inactive\n"}
		}
		if adapterTestMatches(command.Argv, "kill", "-s", "HUP", stewardCoordinatorUnit) {
			if fake.delay == 0 {
				write()
			} else {
				pending = fake.delay
			}
		}
		return procx.Result{}
	}}
	context = adapterTestContext(t, runner)
	context.Now = func() time.Time { return fake.clock }
	context.Sleep = func(d time.Duration) {
		fake.clock = fake.clock.Add(d)
		if pending > 0 {
			pending--
			if pending == 0 {
				write()
			}
		}
	}
	document, err := fleet.Parse(fleetTestComponent(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	context.Host.Name = document.Coordinator.Host
	return context, runner, fake
}

// The coordinator only re-reads its catalog input on SIGHUP or at start, and the
// steward component restarts the unit before this component writes the file.
// A changed catalog therefore asks the running coordinator to reload, once, and
// converges on the receipt the coordinator writes for that signal.
func TestCoordinatorCatalogChangeReloadsTheRunningCoordinator(t *testing.T) {
	context, runner, fake := fleetTestCoordinatorContext(t, true)
	fake.delay = 3
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
	if !adapterTestMatches(actions, "coordinator reloaded its configuration (digest digest-after)") {
		t.Fatalf("actions %v do not report the reload with its digest", actions)
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
	context, runner, _ := fleetTestCoordinatorContext(t, false)
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

// F-3: the convergence used to report "signalled" and move on. Now it reads
// the receipt: accepted and unchanged converge and carry the digest.
func TestCoordinatorReloadReceiptAcceptedAndUnchangedConverge(t *testing.T) {
	for _, outcome := range []string{"accepted", "unchanged"} {
		t.Run(outcome, func(t *testing.T) {
			context, _, fake := fleetTestCoordinatorContext(t, true)
			fake.receipt.Outcome = outcome
			spec := fleetTestComponent(t)
			result, err := fleetProjection(context, spec, context.Host.Name, context.RunID, false)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			reload, _ := result["coordinator_reload"].(map[string]any)
			if reload["outcome"] != outcome || reload["digest"] != "digest-after" {
				t.Fatalf("coordinator_reload = %#v", result["coordinator_reload"])
			}
			if _, isString := result["coordinator_reload"].(string); isString {
				t.Fatal("the reload is still reported as a bare string")
			}
		})
	}
}

// A rejected receipt fails the component with the coordinator's error and its
// blockers, one per line, and says the effective digest is unchanged.
func TestCoordinatorReloadReceiptRejectedFailsTheComponent(t *testing.T) {
	context, _, fake := fleetTestCoordinatorContext(t, true)
	fake.receipt = &fleetReloadReceipt{
		Outcome: "rejected", Error: "2 retained assignment(s) on 1 worker(s) whose execution catalog would change",
		ConfigurationDigest: "digest-before", PreviousDigest: "digest-before",
		Blockers: []fleetReloadBlocker{
			{WorkerID: "omarchy-pc", AssignmentID: "assignment-1", AttemptID: "attempt-1", Progress: "active", Control: "paused", JournalPhase: "stopped", Unblock: "wait for the pause to lift and the attempt to settle, or t3-steward backlog cancel run-1/implement --reason TEXT"},
			{WorkerID: "omarchy-pc", AssignmentID: "assignment-2", AttemptID: "attempt-2", Progress: "waiting-external", Control: "waiting-external", Unblock: "wait for the task wait to settle and the attempt to finish, or t3-steward backlog cancel run-2/review --reason TEXT"},
		},
	}
	spec := fleetTestComponent(t)
	actions := []string{}
	state, err := fleetApply(context, spec, nil, &actions)
	if err == nil {
		t.Fatalf("a rejected reload converged with state %q", state)
	}
	if _, failed := err.(*StepFailed); !failed {
		t.Fatalf("error = %T %v, want a StepFailed", err, err)
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected the error, two blockers and the digest line, got %d lines:\n%s", len(lines), err)
	}
	for i, want := range []string{
		"coordinator rejected the reload: 2 retained assignment(s) on 1 worker(s) whose execution catalog would change",
		"  worker omarchy-pc assignment assignment-1 attempt attempt-1 (active/paused, worker journal stopped): wait for the pause to lift and the attempt to settle, or t3-steward backlog cancel run-1/implement --reason TEXT",
		"  worker omarchy-pc assignment assignment-2 attempt attempt-2 (waiting-external/waiting-external): wait for the task wait to settle and the attempt to finish, or t3-steward backlog cancel run-2/review --reason TEXT",
		"  the effective configuration and its digest are unchanged (digest-before)",
	} {
		if lines[i] != want {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want)
		}
	}
	for _, action := range actions {
		if strings.Contains(action, "reloaded") {
			t.Fatalf("a rejected reload was reported as done: %q", action)
		}
	}
}

// No receipt newer than the signal within 30 s fails the component as
// unverified, names the command that shows the verdict on the host, and says
// whether any receipt file existed: an old steward writes none at all.
func TestCoordinatorReloadWithoutAReceiptIsUnverified(t *testing.T) {
	t.Run("no receipt file", func(t *testing.T) {
		context, _, fake := fleetTestCoordinatorContext(t, true)
		fake.receipt = nil
		start := fake.clock
		_, err := fleetProjection(context, fleetTestComponent(t), context.Host.Name, context.RunID, false)
		if err == nil {
			t.Fatal("a reload nobody receipted converged")
		}
		if _, failed := err.(*StepFailed); !failed {
			t.Fatalf("error = %T, want a StepFailed", err)
		}
		for _, want := range []string{
			"coordinator reload unverified",
			"no receipt file exists",
			"run `t3-steward coordinator reload --json` on " + context.Host.Name,
			context.Path(fleetReloadReceiptPath),
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error lacks %q: %v", want, err)
			}
		}
		if waited := fake.clock.Sub(start); waited < fleetReloadWait || waited > fleetReloadWait+fleetReloadPoll {
			t.Fatalf("waited %s by the clock, want about %s", waited, fleetReloadWait)
		}
	})
	t.Run("receipt older than the signal", func(t *testing.T) {
		context, _, fake := fleetTestCoordinatorContext(t, true)
		fake.stale = true
		_, err := fleetProjection(context, fleetTestComponent(t), context.Host.Name, context.RunID, false)
		if err == nil {
			t.Fatal("a receipt from before the signal counted as this signal's verdict")
		}
		if !strings.Contains(err.Error(), "older than the signal") || !strings.Contains(err.Error(), "coordinator reload --json") {
			t.Fatalf("error = %v", err)
		}
	})
}

// A dry run neither signals nor waits.
func TestCoordinatorReloadDryRunNeitherSignalsNorWaits(t *testing.T) {
	context, runner, fake := fleetTestCoordinatorContext(t, true)
	fake.receipt = nil
	start := fake.clock
	result, err := fleetProjection(context, fleetTestComponent(t), context.Host.Name, context.RunID, true)
	if err != nil {
		t.Fatal(err)
	}
	if runner.ran("kill", "HUP") || fake.clock != start {
		t.Fatalf("a dry run signalled (%v) or waited (%s)", runner.calls, fake.clock.Sub(start))
	}
	if _, present := result["coordinator_reload"]; present {
		t.Fatalf("a dry run reported a reload: %#v", result["coordinator_reload"])
	}
}
