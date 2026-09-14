package agent

import (
	"github.com/iryzhkov/UpKeeper/internal/agentenv"
)

// The agent-environment component projects the shared harness files the fleet
// owns: global instructions, skills, harness settings and MCP registrations.
//
// Everything the projection touches is also written by a harness or by the
// person at the keyboard, so the adapter never overwrites a whole file. It
// merges, verifies, and restores the complete previous environment when any
// step fails.
func init() {
	Register("agent-environment", Handler{
		Observe: environmentObserve,
		Apply:   environmentApply,
	})
}

// environmentObserve renders what every managed path would become and reports
// the drift, without changing anything on the target.
func environmentObserve(c *Context, spec map[string]any) (map[string]any, error) {
	observation, err := agentenv.ObserveEnvironment(spec, c.Home, environmentPendingIdentity)
	if err != nil {
		return nil, environmentFailure(err)
	}
	return map[string]any{"drift": agentenv.DriftToMaps(observation.Drift)}, nil
}

// environmentApply projects the owned files and reports the outcome.
//
// A successful compensation is reported as the "rolled-back" state rather than
// as an error, which is what the controller's report and its exit code have
// always distinguished: the run stopped, but this machine is exactly as the run
// found it.
func environmentApply(
	c *Context, spec map[string]any, before map[string]any, actions *[]string,
) (string, error) {
	result, err := agentenv.ApplyEnvironment(spec, c.Home, c.RunID, c.DryRun, nil)
	if err != nil {
		return "", environmentFailure(err)
	}
	*actions = append(*actions, environmentProjectionActions(result.Changed, c.DryRun)...)
	if result.State == "rolled-back" {
		*actions = append(*actions, "restored and verified the complete previous environment")
	}
	return result.State, nil
}

// environmentPendingIdentity names the staging directory an observation would
// use. An observation never creates it, so the name only has to be stable.
const environmentPendingIdentity = "pending"

// environmentProjectionActions names every path the projection changed, in the
// tense the run is actually in.
func environmentProjectionActions(changed []string, dryRun bool) []string {
	prefix := "projected "
	if dryRun {
		prefix = "would project "
	}
	actions := make([]string, 0, len(changed))
	for _, path := range changed {
		actions = append(actions, prefix+path)
	}
	return actions
}

// environmentFailure classifies an error from the projection. A message that
// begins with "rollback failed" is reported as such by the agent, because a
// machine whose previous environment could not be restored needs a person.
func environmentFailure(err error) error {
	return Failf("%s", err.Error())
}
