package selfconv

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/fsatomic"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
)

// understoodResults are the outcomes that prove the run finished and was
// reported. Anything else leaves the hold in place for a human.
var understoodResults = map[string]bool{
	"converged": true, "updated": true, "ahead": true,
	"deferred": true, "lock-contention": true,
}

// HoldRunner wraps a dispatcher with a durable marker written before dispatch.
//
// An unattended run that dies between "started" and "reported" leaves no other
// evidence. The marker survives process death and lost output, so the next
// scheduled run refuses to dispatch again until somebody has looked at the
// host. That is deliberately more conservative than retrying: a half-applied
// component is not safe to re-apply blind.
type HoldRunner struct {
	Directory string
	Inner     converge.HostRunner
}

// Run dispatches under a hold marker.
func (h *HoldRunner) Run(
	spec inventory.HostSpec, payload agent.Payload, timeout time.Duration, localOnly bool,
) map[string]any {
	if err := fsatomic.EnsureDirectory(h.Directory, 0o700); err != nil {
		return map[string]any{
			"host": spec.Name, "reachable": true, "result": "failed",
			"error": "cannot create the scheduled hold directory: " + err.Error(),
		}
	}
	marker := HoldPath(h.Directory, spec.Name)
	err := fsatomic.CreateExclusive(marker, []byte(holdContent(spec.Name, payload.RunID)), 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return map[string]any{
				"host": spec.Name, "reachable": true, "result": "failed",
				"error": "scheduled hold requires operator observation: " + marker,
			}
		}
		return map[string]any{
			"host": spec.Name, "reachable": true, "result": "failed",
			"error": "cannot write the scheduled hold marker: " + err.Error(),
		}
	}
	_ = fsatomic.SyncDirectory(h.Directory)
	result := h.Inner(spec, payload, timeout, localOnly)
	reachable, _ := result["reachable"].(bool)
	state, _ := result["result"].(string)
	// Only a complete, understood result allows the next daily observation.
	if reachable && understoodResults[state] {
		_ = os.Remove(marker)
	}
	return result
}

// HoldPath is the marker one host's scheduled dispatch writes.
func HoldPath(directory, host string) string {
	return filepath.Join(directory, host+".hold")
}

// holdContent is what an operator finds in the marker: which run left it and
// which command clears it. The run identifier is the thread back to the run
// record; the command is there because the marker is the only breadcrumb a
// lost run leaves.
func holdContent(host, runID string) string {
	if runID == "" {
		runID = "unknown"
	}
	return "Scheduled dispatch may have run (run " + runID + "). Observe the host before clearing.\n" +
		"Clear with: upkeeper hold clear " + host + " --reason TEXT\n"
}
