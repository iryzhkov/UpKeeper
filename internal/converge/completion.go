package converge

// Completion says how much of a run got done, next to the result, which says
// how bad the worst outcome was. A host whose agent-tooling failed but whose
// every other component converged is "failed" and "partial".
//
// It is computed here, on the controller, from each host's component list, so
// that a host running an older agent, which stops at its first failure and
// reports every later component as a bare skip, gets a value too.
const (
	// CompletionComplete is a host whose every selected component converged,
	// was updated or is ahead, including a host with no components to converge.
	CompletionComplete = "complete"
	// CompletionPartial is a host where at least one component succeeded and
	// at least one did not.
	CompletionPartial = "partial"
	// CompletionNone is a host where no component succeeded, including one
	// that failed, was locked or was unreachable before its components ran.
	CompletionNone = "none"
)

// succeededStates are the component outcomes that count as done.
var succeededStates = map[string]bool{"converged": true, "updated": true, "ahead": true}

// HostCompletion is the completion of one host entry of a report.
func HostCompletion(host map[string]any) string {
	entries, _ := host["components"].([]any)
	if len(entries) == 0 {
		// Nothing was selected, or nothing ran: the host result decides.
		result, _ := host["result"].(string)
		if boolField(host, "reachable") && succeededStates[result] {
			return CompletionComplete
		}
		return CompletionNone
	}
	succeeded := 0
	for _, item := range entries {
		entry, _ := item.(map[string]any)
		if state, _ := entry["state"].(string); succeededStates[state] {
			succeeded++
		}
	}
	switch succeeded {
	case len(entries):
		return CompletionComplete
	case 0:
		return CompletionNone
	default:
		return CompletionPartial
	}
}

// ReportCompletion is the completion of a whole run from its host entries,
// each of which already carries its own completion: complete when every host
// is complete, none when every host is none, and partial otherwise.
//
// Every host counts. A host the run deliberately did not dispatch to, because
// it is self-managed or was skipped as unreachable on request, is none: it
// does not change the exit code, but nothing on it converged in this run. A
// run with no host is complete.
func ReportCompletion(hosts []any) string {
	seen := map[string]bool{}
	for _, item := range hosts {
		host, _ := item.(map[string]any)
		completion, _ := host["completion"].(string)
		seen[completion] = true
	}
	switch {
	case len(seen) == 0:
		return CompletionComplete
	case len(seen) == 1 && seen[CompletionComplete]:
		return CompletionComplete
	case len(seen) == 1 && seen[CompletionNone]:
		return CompletionNone
	default:
		return CompletionPartial
	}
}
