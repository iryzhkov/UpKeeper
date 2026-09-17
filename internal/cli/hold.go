package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/pytime"
	"github.com/iryzhkov/UpKeeper/internal/runstore"
	"github.com/iryzhkov/UpKeeper/internal/safety"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
)

// runHold shows or clears the scheduled hold markers on this host.
//
// Both actions work on this host's own state directory only: a hold is written
// by the process that dispatched, so the laptop's marker lives on the laptop
// and normandy's markers for the fleet live on normandy.
func runHold(parsed *arguments, streams Streams) (int, error) {
	directory, err := selfconv.HoldDirectory()
	if err != nil {
		return exitcode.Preflight, err
	}
	store, err := runstore.Default()
	if err != nil {
		return exitcode.Preflight, err
	}
	now := time.Now()
	if parsed.holdAction == "show" {
		markers, err := selfconv.ListHolds(directory, store, now)
		if err != nil {
			return exitcode.Preflight, err
		}
		if parsed.asJSON {
			entries := make([]any, 0, len(markers))
			for _, marker := range markers {
				entries = append(entries, holdDocument(marker))
			}
			return exitcode.Success, printJSON(streams.Out, map[string]any{
				"schema_version": exitcode.SchemaVersion,
				"command":        "hold",
				"action":         "show",
				"directory":      directory,
				"holds":          entries,
				"exit_code":      exitcode.Success,
			})
		}
		printHolds(streams.Out, directory, markers)
		return exitcode.Success, nil
	}

	if _, err := safety.ValidateHostName(parsed.holdHost, nil); err != nil {
		return holdRefusal(parsed, streams, err.Error())
	}
	user := selfconv.UserName()
	marker, logPath, err := selfconv.ClearHold(
		directory, parsed.holdHost, parsed.holdReason, user, now)
	if errors.Is(err, selfconv.ErrNoHold) {
		return holdRefusal(parsed, streams, fmt.Sprintf(
			"no hold marker for %s under %s; nothing to clear", parsed.holdHost, directory))
	}
	if err != nil {
		return exitcode.Preflight, err
	}
	document := map[string]any{
		"schema_version": exitcode.SchemaVersion,
		"command":        "hold",
		"action":         "clear",
		"host":           marker.Host,
		"path":           marker.Path,
		"run_id":         nullable(marker.RunID),
		"reason":         parsed.holdReason,
		"user":           user,
		"cleared_at":     pytime.ISO8601Z(now),
		"cleared_log":    logPath,
		"exit_code":      exitcode.Success,
	}
	if parsed.asJSON {
		return exitcode.Success, printJSON(streams.Out, document)
	}
	fmt.Fprintf(streams.Out, "cleared %s%s; reason recorded in %s\n",
		marker.Path, holdRunSuffix(marker), logPath)
	return exitcode.Success, nil
}

// holdRefusal reports a hold request there is nothing to do for. It exits 2,
// the code for a malformed request, because a missing marker is a mistake in
// what was asked rather than a failure of the host.
func holdRefusal(parsed *arguments, streams Streams, message string) (int, error) {
	if parsed.asJSON {
		_ = printJSON(streams.Out, map[string]any{
			"schema_version": exitcode.SchemaVersion,
			"command":        "hold",
			"result":         "invalid-input",
			"exit_code":      exitcode.Usage,
			"error":          message,
		})
	} else {
		fmt.Fprintf(streams.Err, "upkeeper: error: %s\n", message)
	}
	return exitcode.Usage, nil
}

func holdDocument(marker selfconv.HoldMarker) map[string]any {
	return map[string]any{
		"host":        marker.Host,
		"path":        marker.Path,
		"content":     marker.Content,
		"written_at":  pytime.ISO8601Z(marker.ModifiedAt),
		"age_seconds": int(marker.Age.Seconds()),
		"run_id":      nullable(marker.RunID),
		"run_record":  nullable(marker.RunRecord),
		"run_result":  nullable(marker.RunResult),
	}
}

// printHolds renders the markers for a person: one header per host, the
// marker's own words indented under it, and the command that clears it.
func printHolds(out io.Writer, directory string, markers []selfconv.HoldMarker) {
	if len(markers) == 0 {
		fmt.Fprintf(out, "no scheduled hold markers under %s\n", directory)
		return
	}
	for _, marker := range markers {
		fmt.Fprintf(out, "%s: held for %s%s\n", marker.Host, holdAge(marker.Age), holdRunSuffix(marker))
		for _, line := range strings.Split(strings.TrimSpace(marker.Content), "\n") {
			fmt.Fprintf(out, "  %s\n", line)
		}
		fmt.Fprintf(out, "  clear with: upkeeper hold clear %s --reason TEXT\n", marker.Host)
	}
}

// holdRunSuffix names the run a marker came from and, when this host's run
// store has it, what that run reported.
func holdRunSuffix(marker selfconv.HoldMarker) string {
	if marker.RunID == "" {
		return ""
	}
	suffix := " (run " + marker.RunID
	if marker.RunRecord != "" {
		suffix += " reported " + marker.RunResult + ", record at " + marker.RunRecord
	}
	return suffix + ")"
}

func holdAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	return age.Truncate(time.Second).String()
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
