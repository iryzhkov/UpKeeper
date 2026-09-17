package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
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
const selfTimerUnit = "upkeeper-self.timer"

// driftRecordLimit bounds how far back the run store is read for evidence.
const driftRecordLimit = 200

// componentOutcome is what a recorded run said about one component.
type componentOutcome struct {
	State   string
	Error   string
	Actions []string
}

// lastRun is the newest recorded run on this host that dispatched to a host.
type lastRun struct {
	RunID      string
	CreatedAt  string
	HostResult string
	HostError  string
	Components map[string]componentOutcome
}

// driftEvidence is everything this host can say about why a scheduled pull
// has not converged a drifting component. All of it is local: the hold
// markers this host wrote, the run records this host kept, this host's own
// timer. Nothing here is fetched from another host, so what is not here is
// reported as unavailable rather than guessed.
type driftEvidence struct {
	controller string
	// selfTimerEnabled is nil when the timer state was not observed.
	selfTimerEnabled *bool
	holds            map[string]selfconv.HoldMarker
	lastRuns         map[string]lastRun
}

// gatherDriftEvidence reads the local state directory and this host's timer.
// Evidence is best effort: a store that cannot be read yields no evidence, and
// status still reports the drift it observed.
func gatherDriftEvidence(controller string, process procx.Runner) driftEvidence {
	evidence := driftEvidence{
		controller: controller,
		holds:      map[string]selfconv.HoldMarker{},
		lastRuns:   map[string]lastRun{},
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
		if err != nil {
			continue
		}
		details, _ := record["details"].(map[string]any)
		for _, item := range list(details["hosts"]) {
			host, _ := item.(map[string]any)
			name := text(host["host"])
			if name == "" {
				continue
			}
			if _, seen := evidence.lastRuns[name]; seen {
				continue
			}
			evidence.lastRuns[name] = lastRunOf(ids[index], record, host)
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
		CreatedAt:  text(record["created_at"]),
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

// annotateDriftReasons adds a reason beside every drifting component.
func annotateDriftReasons(document map[string]any, evidence driftEvidence) {
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

// driftReason explains one drifting component from the evidence, in a fixed
// order: a hold marker, this host's own timer, the last recorded run. When
// none of it applies to a remote host, the reason says so in as many words.
func driftReason(host, component string, evidence driftEvidence) string {
	local := host == evidence.controller
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
	}
	run, recorded := evidence.lastRuns[host]
	switch {
	case recorded:
		parts = append(parts, lastRunReason(run, component))
	case local:
		parts = append(parts, "no scheduled run recorded on this host")
	case len(parts) > 0:
		parts = append(parts, "no run record for "+host+" on this host")
	default:
		return fmt.Sprintf("reason unavailable from this host: no hold marker and no run record "+
			"for %s here; inspect %s's own run store and timer", host, host)
	}
	return strings.Join(parts, "; ")
}

func lastRunReason(run lastRun, component string) string {
	prefix := fmt.Sprintf("last recorded run %s (%s)", run.RunID, run.CreatedAt)
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
		return fmt.Sprintf("%s reported %s %s, so the drift appeared after it",
			prefix, component, outcome.State)
	}
}
