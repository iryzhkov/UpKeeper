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
			// The guard fails rather than waits on a journal it cannot read,
			// so the note never blames a busy worker for it.
			wantNote: "worker restart pending since 2026-10-04T04:30:00Z (1d6h): the worker journal " +
				"could not be inspected; each pull fails the restart until t3-steward worker " +
				"inspect-journal answers",
			wantCounts: [2]any{nil, nil},
		},
		{
			// An idle worker is restarted by the next pull; it is not waited
			// for, however long the restart has been pending.
			name:        "an idle journal",
			journal:     `{"dispatched":0,"settlePending":0}`,
			installedAt: stewardPendingNow.Add(-30 * time.Hour),
			wantSince:   "2026-10-04T04:30:00Z",
			wantNote: "worker restart pending since 2026-10-04T04:30:00Z (1d6h): 0 dispatched, " +
				"0 settle-pending; the worker is idle, so the next pull restarts it",
			wantCounts: [2]any{0, 0},
		},
		{
			name:        "dispatched attempts and no settle-pending count",
			journal:     `{"dispatched":2}`,
			installedAt: stewardPendingNow.Add(-30 * time.Hour),
			wantSince:   "2026-10-04T04:30:00Z",
			wantNote: "worker restart pending since 2026-10-04T04:30:00Z (1d6h): 2 dispatched, " +
				"settle-pending not counted; the restart waits for an idle worker" + stewardNeverIdleAction,
			wantCounts: [2]any{2, nil},
		},
		{
			name:        "a journal summary that does not count settle-pending attempts",
			journal:     `{"dispatched":0}`,
			installedAt: stewardPendingNow.Add(-45 * time.Minute),
			wantSince:   "2026-10-05T09:45:00Z",
			wantNote: "worker restart pending since 2026-10-05T09:45:00Z (45m): 0 dispatched, " +
				"settle-pending not counted; the restart waits for a t3-steward whose journal " +
				"summary counts settle-pending attempts",
			wantCounts: [2]any{0, nil},
		},
		{
			name:        "a malformed count",
			journal:     `{"dispatched":-1,"settlePending":0}`,
			installedAt: stewardPendingNow.Add(-30 * time.Second),
			wantSince:   "2026-10-05T10:29:30Z",
			wantNote: "worker restart pending since 2026-10-05T10:29:30Z (under a minute): the worker " +
				"journal summary is malformed; each pull fails the restart until t3-steward worker " +
				"inspect-journal answers",
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

// The note follows the guard's own order: a usable positive dispatched count
// defers the restart before the guard ever reads the settle-pending count, so
// a malformed settle-pending count beside it is a busy worker with one count
// unknown, not a journal that fails every pull. With no dispatched attempts
// the guard does read it, and fails, and so does the note.
func TestStewardObserveFollowsTheGuardOnAMalformedSettlePendingCount(t *testing.T) {
	for _, test := range []struct {
		name       string
		journal    string
		wantNote   string
		wantCounts [2]any
		wantDefer  string
	}{
		{
			name:    "dispatched attempts and a negative settle-pending count",
			journal: `{"dispatched":2,"settlePending":-1}`,
			wantNote: "worker restart pending since 2026-10-04T04:30:00Z (1d6h): 2 dispatched, " +
				"settle-pending count malformed; the restart waits for an idle worker" + stewardNeverIdleAction,
			wantCounts: [2]any{2, nil},
			wantDefer:  "worker restart deferred: 2 dispatched attempt(s) in the journal",
		},
		{
			name:    "dispatched attempts and a settle-pending count that is not a number",
			journal: `{"dispatched":2,"settlePending":"bad"}`,
			wantNote: "worker restart pending since 2026-10-04T04:30:00Z (1d6h): 2 dispatched, " +
				"settle-pending count malformed; the restart waits for an idle worker" + stewardNeverIdleAction,
			wantCounts: [2]any{2, nil},
			wantDefer:  "worker restart deferred: 2 dispatched attempt(s) in the journal",
		},
		{
			name:    "no dispatched attempts and a malformed settle-pending count",
			journal: `{"dispatched":0,"settlePending":-1}`,
			wantNote: "worker restart pending since 2026-10-04T04:30:00Z (1d6h): the worker journal " +
				"summary is malformed; each pull fails the restart until t3-steward worker " +
				"inspect-journal answers",
			wantCounts: [2]any{nil, nil},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			context, spec := stewardPendingHost(t, test.journal, stewardPendingNow.Add(-30*time.Hour))
			observed, err := stewardObserve(context, spec)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			waiting, _ := observed["worker_restart_waiting"].(map[string]any)
			if waiting["note"] != test.wantNote {
				t.Errorf("note\n got %q\nwant %q", waiting["note"], test.wantNote)
			}
			if got := [2]any{waiting["dispatched"], waiting["settle_pending"]}; got != test.wantCounts {
				t.Errorf("counts %v, want %v", got, test.wantCounts)
			}
			actions := []string{}
			restarted, err := stewardRestartPersistentWorker(context,
				context.Path(".local/bin/t3-steward"), &actions)
			if restarted {
				t.Fatalf("the guard restarted the worker")
			}
			if test.wantDefer == "" {
				if err == nil {
					t.Errorf("the guard did not fail: actions %v", actions)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(actions, []string{test.wantDefer}) {
				t.Errorf("guard: err %v, actions %v, want %q", err, actions, test.wantDefer)
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
	if _, err := os.Stat(context.Path(stewardWorkerRestartRecord)); err == nil {
		t.Fatalf("observing wrote %s", stewardWorkerRestartRecord)
	}
}

// A second install while the worker is still busy rewrites the binary, and so
// its modification time, but the worker has been waiting since the first:
// the pass that defers records when the wait began for that worker process,
// and a later install does not move it. Another worker process starts its
// own wait.
func TestStewardPendingSinceSurvivesASecondInstall(t *testing.T) {
	context, spec := stewardPendingHost(t, `{"dispatched":1,"settlePending":0}`,
		stewardPendingNow.Add(-30*time.Hour))
	stewardTestServe(t, nil, "")
	before, err := stewardObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	actions := []string{}
	if state, err := stewardApply(context, spec, before, &actions); err != nil || state != "converged" {
		t.Fatalf("deferring pass: %q, %v (actions %v)", state, err, actions)
	}
	binary := context.Path(".local/bin/t3-steward")
	second := stewardPendingNow.Add(-time.Minute)
	if err := os.Chtimes(binary, second, second); err != nil {
		t.Fatal(err)
	}
	after, err := stewardObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if after["worker_restart_pending_since"] != "2026-10-04T04:30:00Z" {
		t.Errorf("a second install moved pending since to %v", after["worker_restart_pending_since"])
	}
	waiting, _ := after["worker_restart_waiting"].(map[string]any)
	if note, _ := waiting["note"].(string); !strings.HasSuffix(note, stewardNeverIdleAction) {
		t.Errorf("the never-idle action disappeared after a second install: %q", note)
	}
	if probed := ObserveWorkerRestart(context.Home, context.Runner, stewardPendingNow); probed["since"] != "2026-10-04T04:30:00Z" {
		t.Errorf("doctor's observation says since %v", probed["since"])
	}
	// A worker process that is not the one the record names, here one
	// running another replaced binary, waits from the installed binary's time.
	other := context.Path("other-t3-steward")
	adapterTestWrite(t, other, "#!/bin/sh\necho other steward\n")
	older := stewardPendingNow.Add(-90 * time.Hour)
	if err := os.Chtimes(other, older, older); err != nil {
		t.Fatal(err)
	}
	stewardTestRunningWorker(t, context, other)
	another, err := stewardObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if another["worker_restart_pending_since"] != "2026-10-05T10:29:00Z" {
		t.Errorf("another worker inherited the record: since %v", another["worker_restart_pending_since"])
	}
}

// A dry run describes; it records nothing.
func TestStewardDryRunRecordsNoPendingRestart(t *testing.T) {
	context, spec := stewardPendingHost(t, `{"dispatched":1,"settlePending":0}`,
		stewardPendingNow.Add(-30*time.Hour))
	stewardTestServe(t, nil, "")
	before, err := stewardObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	context.DryRun = true
	actions := []string{}
	if _, err := stewardApply(context, spec, before, &actions); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := os.Stat(context.Path(stewardWorkerRestartRecord)); err == nil {
		t.Fatalf("a dry run wrote %s", stewardWorkerRestartRecord)
	}
}

// A record that cannot be read, or that names nothing usable, is ignored.
func TestStewardIgnoresAnUnusablePendingRestartRecord(t *testing.T) {
	for _, content := range []string{"not json", `{"worker_pid":4242}`, `{"worker_pid":4242,"since":"yesterday"}`, ""} {
		context, spec := stewardPendingHost(t, `{"dispatched":1,"settlePending":0}`,
			stewardPendingNow.Add(-2*time.Hour))
		adapterTestWrite(t, context.Path(stewardWorkerRestartRecord), content)
		observed, err := stewardObserve(context, spec)
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		if observed["worker_restart_pending_since"] != "2026-10-05T08:30:00Z" {
			t.Errorf("record %q gave since %v", content, observed["worker_restart_pending_since"])
		}
	}
}
