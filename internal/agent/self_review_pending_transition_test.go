package agent

import (
	"fmt"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"strings"
	"testing"
)

func TestRound1SelfReviewPendingAppearsAfterPreflight(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprint(dry), func(t *testing.T) {
			c, r := huyangRunningWorld(t, true, "git", "go")
			c.DryRun = dry
			c.Payload.DryRun = dry
			inner := r.handle
			active := 0
			r.handle = func(cmd procx.Command) procx.Result {
				if adapterTestMatches(cmd.Argv, "is-active") {
					active++
					if active == 2 {
						adapterTestWrite(t, c.Path(huyangBootstrapPending), ".local/share/huyang\n")
					}
				}
				return inner(cmd)
			}
			result := Execute(c)
			entry := resultEntry(t, result, "huyang")
			next := resultEntry(t, result, "independent-next")
			msg, _ := entry["error"].(string)
			if entry["state"] != "blocked" || !strings.Contains(msg, "make") || next["state"] != "updated" || r.ran("make", "build") {
				t.Fatalf("pending appeared after preflight: entry=%v next=%v build=%v", entry, next, r.ran("make", "build"))
			}
		})
	}
}
