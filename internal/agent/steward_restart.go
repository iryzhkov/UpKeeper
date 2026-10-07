package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/pytime"
)

// stewardWorkerRestartStaleAfter is how long a worker restart may wait for an
// idle worker before the report names what the operator can do about a host
// that is never idle.
const stewardWorkerRestartStaleAfter = 24 * time.Hour

// stewardDefaultPath is where the release installs t3-steward, relative to the
// home directory. Doctor's probe has no release spec and reads this path.
const stewardDefaultPath = ".local/bin/t3-steward"

// stewardWorkerRestartRecord, under the home directory, records since when the
// running worker process has waited for its restart. A pass that defers the
// restart writes it, once per worker process; observation only reads it.
const stewardWorkerRestartRecord = ".local/state/dev-fleet/steward-worker-restart.json"

// stewardPendingRestart is what the pending check found: whether the running
// worker predates the installed binary, which process it is, and the
// modification times of the file it executes and of the installed one.
type stewardPendingRestart struct {
	pending   bool
	pid       int
	running   time.Time
	installed time.Time
}

// stewardRestartRecord is the content of stewardWorkerRestartRecord. The
// worker process is named by its PID and the modification time of the file it
// executes, so that a reused PID running another binary is another worker.
type stewardRestartRecord struct {
	WorkerPID        int    `json:"worker_pid"`
	WorkerExecutable int64  `json:"worker_executable_mtime_ns"`
	Since            string `json:"since"`
}

// ObserveWorkerRestart reports a pending t3-steward worker restart on this
// host exactly as the t3-steward component's observation does, or nil when
// none is pending. Doctor's probe calls it. It only reads: it never restarts
// anything and never writes.
func ObserveWorkerRestart(home string, runner procx.Runner, now time.Time) map[string]any {
	c := &Context{Home: home, Runner: runner, Now: func() time.Time { return now }}
	binary := c.Path(stewardDefaultPath)
	found := stewardWorkerRestartPendingSince(c, binary)
	if !found.pending {
		return nil
	}
	return stewardWorkerRestartWaiting(c, binary, stewardPendingSince(c, found))
}

// stewardPendingSince is since when the running worker has waited for its
// restart: the time a deferring pass recorded for this worker process, or,
// without such a record, the installed binary's modification time, which is
// when the install replaced the file the worker executes. The record matters
// when a second install replaces the binary again while the worker is still
// busy: the binary's time moves, the worker's wait does not.
func stewardPendingSince(c *Context, found stewardPendingRestart) time.Time {
	if record, ok := stewardReadRestartRecord(c); ok && stewardRecordNames(record, found) {
		if since, err := time.Parse(time.RFC3339Nano, record.Since); err == nil {
			return since
		}
	}
	return found.installed
}

func stewardRecordNames(record stewardRestartRecord, found stewardPendingRestart) bool {
	return record.WorkerPID == found.pid && record.WorkerExecutable == found.running.UnixNano()
}

func stewardReadRestartRecord(c *Context) (stewardRestartRecord, bool) {
	var record stewardRestartRecord
	content, err := os.ReadFile(c.Path(stewardWorkerRestartRecord))
	if err != nil || json.Unmarshal(content, &record) != nil || record.WorkerPID <= 0 {
		return stewardRestartRecord{}, false
	}
	return record, true
}

// stewardRecordDeferredRestart records, after a pass deferred the worker
// restart, since when the running worker process has waited, unless the record
// already names that process. It is evidence for the report, not state the
// restart depends on: a failure to write it changes nothing but the time the
// report shows, and a dry run writes nothing.
func stewardRecordDeferredRestart(c *Context, binary string) {
	if c.DryRun {
		return
	}
	found := stewardWorkerRestartPendingSince(c, binary)
	if !found.pending {
		return
	}
	if record, ok := stewardReadRestartRecord(c); ok && stewardRecordNames(record, found) {
		return
	}
	encoded, err := json.Marshal(stewardRestartRecord{
		WorkerPID:        found.pid,
		WorkerExecutable: found.running.UnixNano(),
		Since:            pytime.ISO8601Z(found.installed.UTC().Truncate(time.Second)),
	})
	if err != nil {
		return
	}
	path := c.Path(stewardWorkerRestartRecord)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = fsatomic.WriteFile(path, append(encoded, '\n'), 0o600)
}

// stewardWorkerRestartWaiting explains a pending restart: since when it has
// waited, by this host's clock, and what the worker journal says it waits
// for. The counts are read the way the restart guard reads them, in its order;
// a count the guard would refuse is reported as missing rather than as zero.
//
// The note is the sentence status and doctor print, and it says what the
// guard will do with these counts. Only a worker that owns dispatched or
// settle-pending attempts is waited for; past stewardWorkerRestartStaleAfter
// its note adds the next action, which is always the operator's: UpKeeper
// never restarts a worker that owns execution.
func stewardWorkerRestartWaiting(c *Context, binary string, since time.Time) map[string]any {
	since = since.UTC().Truncate(time.Second)
	age := c.now().Sub(since)
	waiting := map[string]any{
		"since":          pytime.ISO8601Z(since),
		"age_seconds":    int64(age / time.Second),
		"dispatched":     nil,
		"settle_pending": nil,
	}
	const busy = "the restart waits for an idle worker"
	const failing = "each pull fails the restart until t3-steward worker inspect-journal answers"
	counts, outcome := "", ""
	summary := c.Run([]string{binary, "worker", "inspect-journal"}, 30*time.Second, "")
	document, err := canonjson.DecodeObject([]byte(summary.Output))
	switch {
	case !summary.OK:
		counts, outcome = "the worker journal could not be inspected", failing
	case err != nil:
		counts, outcome = "the worker journal summary is malformed", failing
	default:
		dispatched, counted := stewardJournalCount(document, "dispatched")
		_, present := document["settlePending"]
		settlePending, settleCounted := stewardJournalCount(document, "settlePending")
		// The cases follow the guard's order: a positive dispatched count
		// defers the restart before the guard reads the settle-pending count.
		switch {
		case !counted:
			counts, outcome = "the worker journal summary is malformed", failing
		case dispatched != 0 && present && !settleCounted:
			waiting["dispatched"] = dispatched
			counts = fmt.Sprintf("%d dispatched, settle-pending count malformed", dispatched)
			outcome = busy
		case present && !settleCounted:
			counts, outcome = "the worker journal summary is malformed", failing
		case !present:
			waiting["dispatched"] = dispatched
			counts = fmt.Sprintf("%d dispatched, settle-pending not counted", dispatched)
			outcome = "the restart waits for a t3-steward whose journal summary counts " +
				"settle-pending attempts"
			if dispatched != 0 {
				outcome = busy
			}
		default:
			waiting["dispatched"], waiting["settle_pending"] = dispatched, settlePending
			counts = fmt.Sprintf("%d dispatched, %d settle-pending", dispatched, settlePending)
			outcome = "the worker is idle, so the next pull restarts it"
			if dispatched != 0 || settlePending != 0 {
				outcome = busy
			}
		}
	}
	waiting["reason"] = counts
	note := fmt.Sprintf("worker restart pending since %s (%s): %s; %s",
		waiting["since"], stewardAge(age), counts, outcome)
	if outcome == busy && age > stewardWorkerRestartStaleAfter {
		note += fmt.Sprintf("; this host has not been idle for %s; drain the worker once Steward "+
			"supports it, or restart it by hand at a quiet moment after checking "+
			"t3-steward worker inspect-journal", stewardAge(age))
	}
	waiting["note"] = note
	return waiting
}

// stewardAge renders how long a restart has waited, to the minute below a
// day and to the hour above it.
func stewardAge(age time.Duration) string {
	switch {
	case age < 0:
		return "ahead of this host's clock"
	case age < time.Minute:
		return "under a minute"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int64(age/time.Minute))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int64(age/time.Hour), int64(age%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dd%dh", int64(age/(24*time.Hour)), int64(age%(24*time.Hour)/time.Hour))
}
