package agent

import (
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"os"
	"strings"
	"testing"
)

func TestRound1SelfReviewBlockedTransitionKeepsUnit(t *testing.T) {
	c, r := huyangRunningWorld(t, true, "git", "go")
	unit := c.Path(".config/systemd/user/huyang.service")
	old := "[Unit]\nDescription=Huyang\nAfter=default.target\n[Service]\nExecStart=/huyang serve\n[Install]\nWantedBy=default.target\n"
	adapterTestWrite(t, unit, old)
	inner := r.handle
	n := 0
	r.handle = func(cmd procx.Command) procx.Result {
		if adapterTestMatches(cmd.Argv, "is-active") {
			n++
			if n == 1 {
				return procx.Result{Stdout: "active"}
			}
			return procx.Result{Code: 3, Stdout: "inactive"}
		}
		return inner(cmd)
	}
	result := Execute(c)
	entry := resultEntry(t, result, "huyang")
	after, _ := os.ReadFile(unit)
	t.Logf("entry=%v; unit=%q; calls=%v", entry, string(after), r.calls)
	if entry["state"] != "blocked" {
		t.Fatalf("expected blocked: %v", result)
	}
	if string(after) != old || r.ran("daemon-reload") {
		t.Fatalf("blocked component changed unit: after=%q reload=%v", after, r.ran("daemon-reload"))
	}
	if !strings.Contains(entry["error"].(string), "make") {
		t.Fatal("missing make remedy")
	}
}
