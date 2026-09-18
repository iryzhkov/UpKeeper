package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/hostevidence"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/runstore"
	"github.com/iryzhkov/UpKeeper/internal/safety"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
)

func runReleaseStatus(parsed *arguments, streams Streams) (int, error) {
	root, err := ConfigRoot()
	if err != nil {
		return exitcode.Preflight, err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	value, err := release.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	hosts, err := safety.ParseHostSelector(parsed.limit, config.KnownHosts())
	if err != nil {
		return exitcode.Preflight, err
	}
	if len(hosts) == 0 {
		hosts = config.HostNames()
	}
	doc, _, err := converge.Fleet(config, value, converge.Options{
		Hosts: hosts, DryRun: true, ObserveOnly: true, ManifestCommit: manifestCommit(root), Timeout: time.Duration(parsed.probeTimeout) * time.Second, Process: procx.Run,
	})
	if err != nil {
		return exitcode.Preflight, err
	}
	code := 0
	result := "converged"
	for _, raw := range doc["hosts"].([]any) {
		host := raw.(map[string]any)
		host["name"] = host["host"]
		if host["observe_only"] != true {
			host["result"] = "unavailable"
			host["error"] = "release observation unavailable; target needs a compatible UpKeeper binary"
			code = 20
			result = "partial"
			continue
		}
		switch host["result"] {
		case "failed":
			code = 20
			result = "partial"
		case "drift":
			if code == 0 {
				code = 10
				result = "drift"
			}
		}
	}
	doc["command"] = "status"
	doc["result"] = result
	doc["exit_code"] = code
	annotateDriftReasons(doc, gatherDriftEvidence(text(doc["controller"]), procx.Run))
	if parsed.asJSON {
		err = printJSON(streams.Out, doc)
	} else {
		printStatus(streams.Out, doc)
	}
	return code, err
}

// selfTimerUnit is the timer every host converges through.
const selfTimerUnit = hostevidence.SelfTimer

// driftRecordLimit bounds how far back the run store is read for evidence.
const driftRecordLimit = 200

// componentOutcome is what a recorded run said about one component.
type componentOutcome struct {
	State   string
	Error   string
	Actions []string
}

// lastRun is one scheduled run this host recorded that dispatched to a host.
type lastRun struct {
	RunID     string
	Command   string
	CreatedAt string
	// Selected is the component list the run was asked to converge; a run that
	// never selected a component says nothing about that component's drift.
	Selected   []string
	HostResult string
	HostError  string
	Components map[string]componentOutcome
}

// scheduledCommands are the run records that count as scheduled convergence.
// A manual pull is recorded too, but it is not the answer to "why has the
// scheduled pull not converged this".
var scheduledCommands = hostevidence.ScheduledCommands

// driftEvidence is everything status can say about why a scheduled pull has
// not converged a drifting component: what this host holds locally (the hold
// markers it wrote, the run records it kept, its own timer) and, for a host
// whose probe reported them, that host's own markers, runs and timer. What
// neither source has is reported as unavailable rather than guessed.
type driftEvidence struct {
	controller string
	// selfTimerEnabled is nil when the timer state was not observed.
	selfTimerEnabled *bool
	holds            map[string]selfconv.HoldMarker
	// scheduledRuns holds, per host, the scheduled runs recorded here, newest
	// first.
	scheduledRuns map[string][]lastRun
	// probed names every host whose own probe evidence was read; timers is
	// what those probes said about the host's self timer, nil when unobserved.
	probed map[string]bool
	timers map[string]*bool
}

// gatherDriftEvidence reads the local state directory and this host's timer.
// Evidence is best effort: a store that cannot be read yields no evidence, and
// status still reports the drift it observed.
func gatherDriftEvidence(controller string, process procx.Runner) driftEvidence {
	evidence := driftEvidence{
		controller:    controller,
		holds:         map[string]selfconv.HoldMarker{},
		scheduledRuns: map[string][]lastRun{},
	}
	store, err := runstore.Default()
	if err != nil {
		return evidence
	}
	if directory, err := selfconv.HoldDirectory(); err == nil {
		if markers, err := selfconv.ListHolds(directory, store, time.Now()); err == nil {
			for _, marker := range markers {
				evidence.holds[marker.Host] = marker
			}
		}
	}
	ids := store.ListIDs()
	if len(ids) > driftRecordLimit {
		ids = ids[len(ids)-driftRecordLimit:]
	}
	for index := len(ids) - 1; index >= 0; index-- {
		record, err := store.Read(ids[index])
		if err != nil || !scheduledCommands[text(record["command"])] {
			continue
		}
		details, _ := record["details"].(map[string]any)
		for _, item := range list(details["hosts"]) {
			host, _ := item.(map[string]any)
			name := text(host["host"])
			if name == "" {
				continue
			}
			evidence.scheduledRuns[name] = append(
				evidence.scheduledRuns[name], lastRunOf(ids[index], record, host))
		}
	}
	if controller != "unknown" && process != nil {
		result := process(procx.Command{
			Argv:    []string{"systemctl", "--user", "is-enabled", selfTimerUnit},
			Timeout: 10 * time.Second,
		})
		if result.Err == nil && strings.TrimSpace(result.Stdout) != "" {
			enabled := strings.TrimSpace(result.Stdout) == "enabled"
			evidence.selfTimerEnabled = &enabled
		}
	}
	return evidence
}

func lastRunOf(runID string, record, host map[string]any) lastRun {
	value := lastRun{
		RunID:      runID,
		Command:    text(record["command"]),
		CreatedAt:  text(record["created_at"]),
		Selected:   stringList(record["components"]),
		HostResult: text(host["result"]),
		HostError:  text(host["error"]),
		Components: map[string]componentOutcome{},
	}
	for _, entry := range list(host["components"]) {
		component, _ := entry.(map[string]any)
		value.Components[text(component["name"])] = componentOutcome{
			State:   text(component["state"]),
			Error:   text(component["error"]),
			Actions: stringList(component["actions"]),
		}
	}
	return value
}

// annotateDriftReasons adds a reason beside every drifting component, from
// the controller's local evidence and from what each host's probe reported.
func annotateDriftReasons(document map[string]any, evidence driftEvidence) {
	evidence = withProbeEvidence(document, evidence)
	for _, item := range list(document["hosts"]) {
		host, _ := item.(map[string]any)
		name := text(host["host"])
		for _, entry := range list(host["components"]) {
			component, _ := entry.(map[string]any)
			if text(component["state"]) != "drift" {
				continue
			}
			component["reason"] = driftReason(name, text(component["name"]), evidence)
		}
	}
}

// withProbeEvidence reads the "upkeeper" evidence each remote host's probe
// reported into the evidence, beside what the controller holds locally. Local
// evidence is kept in front: a marker this controller wrote for a host is
// this controller's own knowledge about that host. The controller's own
// entry is skipped, because its local evidence is the same store.
func withProbeEvidence(document map[string]any, evidence driftEvidence) driftEvidence {
	if evidence.holds == nil {
		evidence.holds = map[string]selfconv.HoldMarker{}
	}
	if evidence.scheduledRuns == nil {
		evidence.scheduledRuns = map[string][]lastRun{}
	}
	evidence.probed = map[string]bool{}
	evidence.timers = map[string]*bool{}
	for _, item := range list(document["hosts"]) {
		host, _ := item.(map[string]any)
		name := text(host["host"])
		probe, reported := host["upkeeper"].(map[string]any)
		if !reported || name == "" || name == evidence.controller {
			continue
		}
		evidence.probed[name] = true
		for _, entry := range list(probe["holds"]) {
			marker, _ := entry.(map[string]any)
			if text(marker["host"]) != name {
				continue
			}
			if _, held := evidence.holds[name]; held {
				break
			}
			evidence.holds[name] = selfconv.HoldMarker{
				Host:  name,
				Path:  text(marker["path"]),
				RunID: text(marker["run_id"]),
				Age:   time.Duration(number(marker["age_seconds"])) * time.Second,
			}
		}
		for _, entry := range list(probe["scheduled_runs"]) {
			run, _ := entry.(map[string]any)
			if !scheduledCommands[text(run["command"])] {
				continue
			}
			for _, raw := range list(run["hosts"]) {
				outcome, _ := raw.(map[string]any)
				if text(outcome["host"]) == name {
					evidence.scheduledRuns[name] = append(
						evidence.scheduledRuns[name], lastRunOf(text(run["run_id"]), run, outcome))
				}
			}
		}
		timers, _ := probe["timers"].(map[string]any)
		self, _ := timers[selfTimerUnit].(map[string]any)
		if enabled, observed := self["enabled"].(bool); observed {
			evidence.timers[name] = &enabled
		}
	}
	return evidence
}

// number reads an integer however the document encoded it: as a Go int when
// built in process, or as a JSON number when read back from a host.
func number(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	}
	return 0
}

// driftReason explains one drifting component from the evidence, in a fixed
// order: a hold marker, the host's timer, the last recorded run. When none
// of it applies to a host whose probe reported nothing, the reason says so
// in as many words.
func driftReason(host, component string, evidence driftEvidence) string {
	local := host == evidence.controller
	probed := evidence.probed[host]
	parts := []string{}
	if marker, held := evidence.holds[host]; held {
		leftBy := ""
		if marker.RunID != "" {
			leftBy = " (left by run " + marker.RunID + ")"
		}
		parts = append(parts, fmt.Sprintf(
			"scheduled hold marker present for %s%s; observe the host, then clear it with: "+
				"upkeeper hold clear %s --reason TEXT", holdAge(marker.Age), leftBy, host))
	}
	if local && evidence.selfTimerEnabled != nil && !*evidence.selfTimerEnabled {
		parts = append(parts, selfTimerUnit+
			" is disabled on this host, so no scheduled run will converge it")
	} else if enabled := evidence.timers[host]; probed && enabled != nil && !*enabled {
		parts = append(parts, selfTimerUnit+
			" is disabled on "+host+", so no scheduled run will converge it")
	}
	run, recorded := lastScheduledRun(evidence.scheduledRuns[host], component)
	switch {
	case recorded:
		parts = append(parts, lastRunReason(run, component))
	case local:
		parts = append(parts, "no scheduled run selecting "+component+" recorded on this host")
	case probed:
		parts = append(parts, "no scheduled run selecting "+component+" recorded on "+host)
	case len(parts) > 0:
		parts = append(parts, "no scheduled run record for "+host+" on this host")
	default:
		return fmt.Sprintf("reason unavailable from this host: no hold marker and no scheduled "+
			"run record for %s here; inspect %s's own run store and timer", host, host)
	}
	return strings.Join(parts, "; ")
}

// lastScheduledRun picks the newest scheduled run that selected the component.
// A run that did not select it says nothing about it.
func lastScheduledRun(runs []lastRun, component string) (lastRun, bool) {
	for _, run := range runs {
		for _, selected := range run.Selected {
			if selected == component {
				return run, true
			}
		}
	}
	return lastRun{}, false
}

// lastRunReason reports what the run recorded, and only that.
func lastRunReason(run lastRun, component string) string {
	prefix := fmt.Sprintf("last %s run %s (%s)", run.Command, run.RunID, run.CreatedAt)
	outcome, reached := run.Components[component]
	if !reached {
		hostOutcome := "host " + run.HostResult
		if run.HostError != "" {
			hostOutcome += ": " + run.HostError
		}
		return fmt.Sprintf("%s did not reach %s: %s", prefix, component, hostOutcome)
	}
	switch outcome.State {
	case "failed", "rolled-back", "rollback-failed":
		return fmt.Sprintf("%s %s %s: %s", prefix, outcome.State, component, firstLineOf(outcome.Error))
	case "deferred":
		detail := outcome.Error
		if len(outcome.Actions) > 0 {
			detail = outcome.Actions[0]
		}
		return fmt.Sprintf("%s deferred %s: %s", prefix, component, detail)
	default:
		return fmt.Sprintf("%s reported %s %s", prefix, component, outcome.State)
	}
}
