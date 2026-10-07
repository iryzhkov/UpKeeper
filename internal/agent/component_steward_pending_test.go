package agent

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// stewardPendingNow is the clock every pending-restart test observes by.
var stewardPendingNow = time.Date(2026, 10, 5, 10, 30, 0, 0, time.UTC)

const stewardNeverIdleAction = "; this host has not been idle for 1d6h; drain the worker once Steward " +
	"supports it, or restart it by hand at a quiet moment after checking t3-steward worker inspect-journal"

// stewardPendingHost is a host whose worker still runs the binary an earlier
// pass replaced, at installedAt, with the journal answering journal.
func stewardPendingHost(t *testing.T, journal string, installedAt time.Time) (*Context, map[string]any) {
	t.Helper()
	context, _ := stewardTestPrepare(t, stewardTestHost{
		active: true, workerLoaded: true, workerPID: "4242", dispatched: journal,
	})
	context.Now = func() time.Time { return stewardPendingNow }
	spec := stewardTestSpec(false)
	context.Components["t3-steward"] = spec
	binary := context.Path(".local/bin/t3-steward")
	adapterTestWrite(t, binary, stewardTestBinary)
	if err := os.Chtimes(binary, installedAt, installedAt); err != nil {
		t.Fatal(err)
	}
	previous := context.Path("previous-t3-steward")
	adapterTestWrite(t, previous, "#!/bin/sh\necho previous steward\n")
	stewardTestRunningWorker(t, context, previous)
	return context, spec
}

// A pending restart says since when it has waited, why it still waits and,
// once it has waited longer than a day, what the operator can do about it.
func TestStewardObserveExplainsAPendingWorkerRestart(t *testing.T) {
	for _, test := range []struct {
		name        string
		journal     string
		installedAt time.Time
		wantSince   string
		wantNote    string
		wantCounts  [2]any
	}{
		{
			name:        "below the threshold",
			journal:     `{"dispatched":1,"settlePending":2}`,
			installedAt: stewardPendingNow.Add(-150 * time.Minute),
			wantSince:   "2026-10-05T08:00:00Z",
			wantNote: "worker restart pending since 2026-10-05T08:00:00Z (2h30m): 1 dispatched, " +
				"2 settle-pending; the restart waits for an idle worker",
			wantCounts: [2]any{1, 2},
		},
		{
			name:        "exactly at the threshold",
			journal:     `{"dispatched":0,"settlePending":1}`,
			installedAt: stewardPendingNow.Add(-24 * time.Hour),
			wantSince:   "2026-10-04T10:30:00Z",
			wantNote: "worker restart pending since 2026-10-04T10:30:00Z (1d0h): 0 dispatched, " +
				"1 settle-pending; the restart waits for an idle worker",
			wantCounts: [2]any{0, 1},
		},
		{
			name:        "above the threshold",
			journal:     `{"dispatched":3,"settlePending":0}`,
			installedAt: stewardPendingNow.Add(-30 * time.Hour),
			wantSince:   "2026-10-04T04:30:00Z",
			wantNote: "worker restart pending since 2026-10-04T04:30:00Z (1d6h): 3 dispatched, " +
				"0 settle-pending; the restart waits for an idle worker" + stewardNeverIdleAction,
			wantCounts: [2]any{3, 0},
		},
		{
			name:        "a journal that cannot be inspected",
			journal:     "",
			installedAt: stewardPendingNow.Add(-30 * time.Hour),
			wantSince:   "2026-10-04T04:30:00Z",
			wantNote: "worker restart pending since 2026-10-04T04:30:00Z (1d6h): the worker journal " +
				"could not be inspected; the restart waits for an idle worker" + stewardNeverIdleAction,
			wantCounts: [2]any{nil, nil},
		},
		{
			name:        "a journal summary that does not count settle-pending attempts",
			journal:     `{"dispatched":0}`,
			installedAt: stewardPendingNow.Add(-45 * time.Minute),
			wantSince:   "2026-10-05T09:45:00Z",
			wantNote: "worker restart pending since 2026-10-05T09:45:00Z (45m): 0 dispatched, " +
				"settle-pending not counted; the restart waits for an idle worker",
			wantCounts: [2]any{0, nil},
		},
		{
			name:        "a malformed count",
			journal:     `{"dispatched":-1,"settlePending":0}`,
			installedAt: stewardPendingNow.Add(-30 * time.Second),
			wantSince:   "2026-10-05T10:29:30Z",
			wantNote: "worker restart pending since 2026-10-05T10:29:30Z (under a minute): the worker " +
				"journal summary is malformed; the restart waits for an idle worker",
			wantCounts: [2]any{nil, nil},
		},
		{
			// The binary's time is this host's own clock, so a time ahead of
			// it is said to be, and never read as a long wait.
			name:        "a binary time ahead of this host's clock",
			journal:     `{"dispatched":1,"settlePending":0}`,
			installedAt: stewardPendingNow.Add(48 * time.Hour),
			wantSince:   "2026-10-07T10:30:00Z",
			wantNote: "worker restart pending since 2026-10-07T10:30:00Z (ahead of this host's clock): " +
				"1 dispatched, 0 settle-pending; the restart waits for an idle worker",
			wantCounts: [2]any{1, 0},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			context, spec := stewardPendingHost(t, test.journal, test.installedAt)
			observed, err := stewardObserve(context, spec)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			if observed["worker_restart_pending"] != true {
				t.Fatalf("worker_restart_pending = %v", observed["worker_restart_pending"])
			}
			if observed["worker_restart_pending_since"] != test.wantSince {
				t.Errorf("worker_restart_pending_since = %v, want %s",
					observed["worker_restart_pending_since"], test.wantSince)
			}
			waiting, _ := observed["worker_restart_waiting"].(map[string]any)
			if waiting["note"] != test.wantNote {
				t.Errorf("note\n got %q\nwant %q", waiting["note"], test.wantNote)
			}
			if got := [2]any{waiting["dispatched"], waiting["settle_pending"]}; got != test.wantCounts {
				t.Errorf("counts %v, want %v", got, test.wantCounts)
			}
			if waiting["since"] != test.wantSince {
				t.Errorf("waiting.since = %v", waiting["since"])
			}
			// Doctor's probe reads the same observation through the export.
			probed := ObserveWorkerRestart(context.Home, context.Runner, stewardPendingNow)
			if !reflect.DeepEqual(probed, waiting) {
				t.Errorf("ObserveWorkerRestart = %v, want %v", probed, waiting)
			}
			if matches := observationMatches(context, "t3-steward", spec, observed); matches {
				t.Errorf("a pending restart is reported converged")
			}
		})
	}
}

// Once the worker runs the installed binary, nothing is pending and nothing
// about a pending restart is reported.
func TestStewardObserveClearsAPendingRestartOnceTheWorkerRestarted(t *testing.T) {
	context, spec := stewardPendingHost(t, `{"dispatched":1,"settlePending":0}`,
		stewardPendingNow.Add(-30*time.Hour))
	before, err := stewardObserve(context, spec)
	if err != nil || before["worker_restart_pending_since"] == nil {
		t.Fatalf("before the restart: %v, %v", before, err)
	}
	stewardTestRunningWorker(t, context, context.Path(".local/bin/t3-steward"))
	after, err := stewardObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if after["worker_restart_pending"] != false {
		t.Fatalf("worker_restart_pending = %v after the restart", after["worker_restart_pending"])
	}
	for _, key := range []string{"worker_restart_pending_since", "worker_restart_waiting"} {
		if _, present := after[key]; present {
			t.Errorf("%s is still reported after the restart: %v", key, after[key])
		}
	}
	if probed := ObserveWorkerRestart(context.Home, context.Runner, stewardPendingNow); probed != nil {
		t.Errorf("ObserveWorkerRestart = %v after the restart", probed)
	}
}

// Observing a pending restart only reads: it never restarts the worker, and
// the pass that does is still the guarded retry.
func TestStewardObservingAPendingRestartNeverRestarts(t *testing.T) {
	context, _ := stewardTestPrepare(t, stewardTestHost{
		active: true, workerLoaded: true, workerPID: "4242",
		dispatched: `{"dispatched":0,"settlePending":0}`,
	})
	spec := stewardTestSpec(false)
	context.Components["t3-steward"] = spec
	adapterTestWrite(t, context.Path(".local/bin/t3-steward"), stewardTestBinary)
	previous := context.Path("previous-t3-steward")
	adapterTestWrite(t, previous, "#!/bin/sh\necho previous steward\n")
	stewardTestRunningWorker(t, context, previous)
	runner := context.Runner
	restarts := 0
	context.Runner = func(command procx.Command) procx.Result {
		if strings.Contains(strings.Join(command.Argv, " "), "restart") {
			restarts++
		}
		return runner(command)
	}
	if _, err := stewardObserve(context, spec); err != nil {
		t.Fatalf("observe: %v", err)
	}
	ObserveWorkerRestart(context.Home, context.Runner, stewardPendingNow)
	if restarts != 0 {
		t.Fatalf("observing ran %d restart command(s)", restarts)
	}
}
