package agent

import (
	"time"

	"github.com/iryzhkov/UpKeeper/internal/operatorhold"
)

// holdClock is the time an operator hold is judged at; a variable so that a
// test can stand on either side of a hold's end.
var holdClock = time.Now

// checkOperatorHold fills result and reports true when this host is under an
// operator hold the payload may not pass. A payload that carries the explicit
// override passes, and the pass is logged beside the hold on this host.
func checkOperatorHold(context *Context, result map[string]any) bool {
	directory := operatorhold.Directory(context.Home)
	now := holdClock()
	hold, held := operatorhold.Check(directory, context.Host.Name, now)
	if !held {
		return false
	}
	if context.Payload.OverrideHold {
		if err := operatorhold.RecordOverride(
			directory, hold, operatorhold.UserName(), "agent", context.RunID, now); err != nil {
			result["result"] = "failed"
			result["error"] = "cannot log the operator hold override: " + err.Error()
			return true
		}
		result["hold_overridden"] = hold.Document(now)
		return false
	}
	result["result"] = "held"
	result["hold"] = hold.Document(now)
	result["error"] = hold.Describe() + "; this host converged nothing. An explicit " +
		"`upkeeper pull --override-hold` passes through it and is logged; release it with: " +
		operatorhold.ClearCommand(context.Host.Name)
	return true
}
