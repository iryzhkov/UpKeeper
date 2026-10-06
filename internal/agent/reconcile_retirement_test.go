package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// F8 of the second custody review: the production reconciliation pull sets
// RetireUpdater like every other pull, but reconciliation must not disable,
// stop or delete legacy updater units.
func TestReconcileT3TransactionSkipsUpdaterRetirement(t *testing.T) {
	for name, reconcile := range map[string]string{"ordinary-pull": "", "reconcile": ".upkeeper-t3-1"} {
		t.Run(name, func(t *testing.T) {
			queried := false
			home := t.TempDir()
			c := &Context{
				Payload:       Payload{RetireUpdater: true, ReconcileT3Transaction: reconcile},
				Home:          home,
				Host:          HostRef{Name: "host"},
				lockDirectory: filepath.Join(home, "lock"),
				Runner: func(cmd procx.Command) procx.Result {
					queried = queried || strings.Contains(strings.Join(cmd.Argv, " "), "t3-update")
					return procx.Result{Code: 1}
				},
			}
			result := Execute(c)
			_, retired := result["legacy_updater"]
			if reconcile == "" && (!retired || !queried) {
				t.Fatalf("ordinary pull did not attempt retirement: %v", result)
			}
			if reconcile != "" && (retired || queried) {
				t.Fatalf("reconciliation attempted legacy updater retirement: %v", result)
			}
		})
	}
}
