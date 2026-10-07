package agent

import (
	"fmt"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
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

// ObserveWorkerRestart reports a pending t3-steward worker restart on this
// host exactly as the t3-steward component's observation does, or nil when
// none is pending. Doctor's probe calls it. It only reads: it never restarts
// anything.
func ObserveWorkerRestart(home string, runner procx.Runner, now time.Time) map[string]any {
	c := &Context{Home: home, Runner: runner, Now: func() time.Time { return now }}
	binary := c.Path(stewardDefaultPath)
	since, pending := stewardWorkerRestartPendingSince(c, binary)
	if !pending {
		return nil
	}
	return stewardWorkerRestartWaiting(c, binary, since)
}

// stewardWorkerRestartWaiting explains a pending restart: since when it has
// waited, by this host's clock, and what the worker journal says it waits
// for. The counts are read the way the restart guard reads them; a count the
// guard would refuse is reported as missing rather than as zero.
//
// The note is the sentence status and doctor print. Past
// stewardWorkerRestartStaleAfter it adds the next action, which is always the
// operator's: UpKeeper never restarts a worker that owns execution.
func stewardWorkerRestartWaiting(c *Context, binary string, since time.Time) map[string]any {
	since = since.UTC().Truncate(time.Second)
	age := c.now().Sub(since)
	waiting := map[string]any{
		"since":          pytime.ISO8601Z(since),
		"age_seconds":    int64(age / time.Second),
		"dispatched":     nil,
		"settle_pending": nil,
	}
	reason := ""
	summary := c.Run([]string{binary, "worker", "inspect-journal"}, 30*time.Second, "")
	document, err := canonjson.DecodeObject([]byte(summary.Output))
	switch {
	case !summary.OK:
		reason = "the worker journal could not be inspected"
	case err != nil:
		reason = "the worker journal summary is malformed"
	default:
		dispatched, counted := stewardJournalCount(document, "dispatched")
		_, present := document["settlePending"]
		settlePending, settleCounted := stewardJournalCount(document, "settlePending")
		switch {
		case !counted || present && !settleCounted:
			reason = "the worker journal summary is malformed"
		case !present:
			waiting["dispatched"] = dispatched
			reason = fmt.Sprintf("%d dispatched, settle-pending not counted", dispatched)
		default:
			waiting["dispatched"], waiting["settle_pending"] = dispatched, settlePending
			reason = fmt.Sprintf("%d dispatched, %d settle-pending", dispatched, settlePending)
		}
	}
	waiting["reason"] = reason
	note := fmt.Sprintf("worker restart pending since %s (%s): %s; the restart waits for an idle worker",
		waiting["since"], stewardAge(age), reason)
	if age > stewardWorkerRestartStaleAfter {
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
