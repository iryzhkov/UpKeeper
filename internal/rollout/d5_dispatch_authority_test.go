package rollout

import (
	"context"
	"strings"
	"testing"
)

func TestRepair2D5DirectDropinDispatchRequiresPhaseAuthority(t *testing.T) {
	for _, fault := range []string{"own-completion", "own-ack", "prior-receipt", "prior-phase", "prior-ack", "prior-restored", "prior-wronghost"} {
		t.Run(fault, func(t *testing.T) {
			b, f, c, r := productionHostFixture(t)
			s, _ := loadState(c)
			host := "agent-b"
			if strings.HasPrefix(fault, "prior-") {
				host = "agent-a"
				seedD5(t, c, s, "agent-b", r.Now())
			}
			s.Completed["enroll-worker:"+host] = true
			reviewAcknowledged(t, c, "phase:enroll-worker:"+host, "", r.Now())
			switch fault {
			case "own-completion":
				delete(s.Completed, "enroll-worker:"+host)
			case "own-ack":
				loseD5Ack(t, c, "phase:enroll-worker:"+host)
			case "prior-receipt":
				delete(s.Extra, "dropin:agent-b")
			case "prior-phase":
				delete(s.Completed, "dropin:agent-b")
			case "prior-ack":
				loseD5Ack(t, c, "phase:dropin:agent-b")
			case "prior-restored", "prior-wronghost":
				var receipt dropinReceipt
				_ = evidence(s, "dropin:agent-b", &receipt)
				if fault == "prior-restored" {
					receipt.Restored = true
				} else {
					receipt.Host = "agent-a"
				}
				s.Extra["dropin:agent-b"] = jsonBytes(receipt)
			}
			if err := b.Execute(context.Background(), c, "dropin:"+host, s); err == nil {
				t.Fatal("drop-in dispatched without exact phase authority")
			}
			if len(f.calls) != 0 {
				t.Fatalf("effects before D5 authority: %v", f.calls)
			}
		})
	}
}
