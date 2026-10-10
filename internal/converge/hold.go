package converge

import (
	"time"

	"github.com/iryzhkov/UpKeeper/internal/operatorhold"
)

// HeldResult is the report entry of a host an operator hold kept a run away
// from. Nothing was dispatched, so the host is reported reachable and held
// rather than offline: it is waiting for a person, not for the network.
func HeldResult(host string, hold operatorhold.Hold, now time.Time) map[string]any {
	return map[string]any{
		"host":      host,
		"reachable": true,
		"result":    "held",
		"hold":      hold.Document(now),
		"error": hold.Describe() + "; nothing was dispatched. An explicit " +
			"`upkeeper pull --override-hold` passes through it and is logged; " +
			"release it with: " + operatorhold.ClearCommand(host),
	}
}
