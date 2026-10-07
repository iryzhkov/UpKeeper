package probe

import (
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// The probe reports the t3-steward worker restart the component observes, and
// a host with no worker unit, such as the laptop, reports none.
func TestCollectReportsNoWorkerRestartWithoutAWorker(t *testing.T) {
	home := collectFixture(t)
	runner := func(command procx.Command) procx.Result {
		for _, argument := range command.Argv {
			if argument == "restart" {
				t.Errorf("the probe ran a restart: %v", command.Argv)
			}
		}
		return procx.Result{Code: 1}
	}
	components := collect(home, runner)["components"].(map[string]any)
	steward := components["t3-steward"].(map[string]any)
	restart, present := steward["worker_restart"]
	if !present {
		t.Fatalf("t3-steward has no worker_restart entry: %v", steward)
	}
	if restart != nil && restart.(map[string]any) != nil {
		t.Fatalf("worker_restart = %v on a host without a worker", restart)
	}
}
