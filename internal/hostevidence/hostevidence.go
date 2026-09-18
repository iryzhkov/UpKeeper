// Package hostevidence reads what a host keeps about its own scheduled
// convergence: the runs its run store recorded, the hold markers it holds
// and the state of its UpKeeper timers.
//
// The document it renders is carried by the target-side probe and by the
// observe-only agent, so that a controller can explain a remote host's drift
// from the same facts status reads locally for the controller itself. It is
// evidence only: nothing here decides whether the host has drifted, and
// nothing here writes.
package hostevidence

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/pytime"
	"github.com/iryzhkov/UpKeeper/internal/runstore"
)

const (
	// SelfTimer is the timer every fleet host converges itself through.
	SelfTimer = "upkeeper-self.timer"
	// FleetTimer is the controller's daily fleet convergence timer.
	FleetTimer = "upkeeper-pull.timer"
	// RunStoreDirectory and HoldDirectory are the state paths, relative to the
	// home directory, that runstore.Default and selfconv.HoldDirectory name.
	RunStoreDirectory = ".local/state/dev-fleet/runs"
	HoldDirectory     = ".local/state/dev-fleet/scheduled-holds"
)

// RunLimit bounds how many scheduled runs the document carries, newest
// first. A controller wants the last one that selected a component; a few
// more cover a run that selected something else in between.
const RunLimit = 20

// scanLimit bounds how far back the run store is read to find them.
const scanLimit = 200

// ScheduledCommands are the run records that count as scheduled convergence.
// A manual pull is recorded too, but it is not the answer to "why has the
// scheduled pull not converged this".
var ScheduledCommands = map[string]bool{"self-pull": true, "scheduled-pull": true}

// failedStates are the component outcomes a run's failed_components names.
var failedStates = map[string]bool{"failed": true, "rolled-back": true, "rollback-failed": true}

// holdRunPattern finds the run identifier a marker names.
var holdRunPattern = regexp.MustCompile(`run ([0-9]{8}T[0-9]{6}Z-[0-9a-f]{8})`)

// RunIDInMarker returns the run identifier a hold marker names, or "" for a
// marker written before markers named their run.
func RunIDInMarker(content string) string {
	if match := holdRunPattern.FindStringSubmatch(content); match != nil {
		return match[1]
	}
	return ""
}

const timerTimeout = 10 * time.Second

// Collect renders the host's evidence. A missing store or directory is
// empty evidence, and a nil runner leaves the timers unobserved; neither is
// an error, because the absence is exactly what a controller reports.
func Collect(home string, runner procx.Runner, now time.Time) map[string]any {
	runs := scheduledRuns(&runstore.Store{Root: filepath.Join(home, RunStoreDirectory)})
	var last any
	if len(runs) > 0 {
		last = runs[0]
	}
	return map[string]any{
		"run_store":          filepath.Join(home, RunStoreDirectory),
		"hold_directory":     filepath.Join(home, HoldDirectory),
		"scheduled_runs":     runs,
		"last_scheduled_run": last,
		"holds":              holds(filepath.Join(home, HoldDirectory), now),
		"timers": map[string]any{
			SelfTimer:  timer(runner, SelfTimer),
			FleetTimer: timer(runner, FleetTimer),
		},
	}
}

// scheduledRuns reads the newest scheduled runs, newest first, keeping of
// each record only what explains drift: identity, time, command, outcome,
// the components it selected and what it reported per host.
func scheduledRuns(store *runstore.Store) []any {
	runs := []any{}
	ids := store.ListIDs()
	if len(ids) > scanLimit {
		ids = ids[len(ids)-scanLimit:]
	}
	for index := len(ids) - 1; index >= 0 && len(runs) < RunLimit; index-- {
		record, err := store.Read(ids[index])
		if err != nil || !ScheduledCommands[text(record["command"])] {
			continue
		}
		details, _ := record["details"].(map[string]any)
		hosts := []any{}
		failed := []any{}
		for _, item := range list(details["hosts"]) {
			host, _ := item.(map[string]any)
			name := text(host["host"])
			components := []any{}
			for _, entry := range list(host["components"]) {
				component, _ := entry.(map[string]any)
				state := text(component["state"])
				components = append(components, map[string]any{
					"name":    text(component["name"]),
					"state":   state,
					"error":   text(component["error"]),
					"actions": stringsOf(component["actions"]),
				})
				if failedStates[state] {
					failed = append(failed, name+" "+text(component["name"])+" "+state)
				}
			}
			hosts = append(hosts, map[string]any{
				"host":       name,
				"result":     text(host["result"]),
				"error":      text(host["error"]),
				"components": components,
			})
		}
		runs = append(runs, map[string]any{
			"run_id":            ids[index],
			"command":           text(record["command"]),
			"created_at":        text(record["created_at"]),
			"result":            text(record["result"]),
			"components":        stringsOf(record["components"]),
			"failed_components": failed,
			"hosts":             hosts,
		})
	}
	return runs
}

// holds lists every marker under the hold directory, by host. The run that
// left each one is read from the marker's own words; whether that run is in
// the store is the controller's question, answered from scheduled_runs.
func holds(directory string, now time.Time) []any {
	markers := []any{}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return markers
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".hold") && !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(directory, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		content, _ := os.ReadFile(path)
		age := now.Sub(info.ModTime())
		if age < 0 {
			age = 0
		}
		markers = append(markers, map[string]any{
			"host":        strings.TrimSuffix(name, ".hold"),
			"path":        path,
			"run_id":      nullable(RunIDInMarker(string(content))),
			"written_at":  pytime.ISO8601Z(info.ModTime()),
			"age_seconds": int(age.Seconds()),
		})
	}
	return markers
}

// timer reports one timer unit as systemctl shows it. enabled and active are
// null when the unit was not observed at all, which is a different answer
// from a unit that is not there: that one is reported disabled with its
// load_state saying not-found.
func timer(runner procx.Runner, unit string) map[string]any {
	entry := map[string]any{
		"unit": unit, "enabled": nil, "active": nil, "load_state": nil,
		"unit_file_state": nil, "next_elapse": nil,
	}
	if runner == nil {
		return entry
	}
	result := runner(procx.Command{
		Argv: []string{
			"systemctl", "--user", "show", unit,
			"-p", "LoadState", "-p", "ActiveState", "-p", "UnitFileState", "-p", "NextElapseUSecRealtime",
		},
		Timeout: timerTimeout,
	})
	if result.Err != nil || result.Code != 0 {
		return entry
	}
	properties := map[string]string{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if key, value, found := strings.Cut(strings.TrimSpace(line), "="); found {
			properties[key] = value
		}
	}
	if _, shown := properties["LoadState"]; !shown {
		return entry
	}
	entry["load_state"] = properties["LoadState"]
	entry["unit_file_state"] = nullable(properties["UnitFileState"])
	entry["enabled"] = properties["UnitFileState"] == "enabled"
	entry["active"] = properties["ActiveState"] == "active"
	if next := properties["NextElapseUSecRealtime"]; next != "" && next != "n/a" {
		entry["next_elapse"] = next
	}
	return entry
}

func text(value any) string {
	text, _ := value.(string)
	return text
}

func list(value any) []any {
	items, _ := value.([]any)
	return items
}

func stringsOf(value any) []any {
	values := []any{}
	for _, item := range list(value) {
		if text, ok := item.(string); ok {
			values = append(values, text)
		}
	}
	return values
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
