package agent

import (
	"bytes"
	"os"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

func TestCoordinatorFleetActivatesWithoutAWorkerEntryAndRestores(t *testing.T) {
	for _, priorExists := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[priorExists], func(t *testing.T) {
			context := fleetTestPrepare(t)
			context.Host.Name = "normandy" // coordinator is not a worker in the fixture
			path := context.Path(fleet.CoordinatorDestination)
			prior := []byte("previous private coordinator projection\n")
			if priorExists {
				adapterTestWrite(t, path, string(prior))
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			spec := fleetTestComponent(t)
			preview, err := fleetObserve(context, spec)
			if err != nil {
				t.Fatal(err)
			}
			if preview["state"] != "updated" || preview["enrolled"] != false {
				t.Fatal(preview)
			}
			if got, err := os.ReadFile(path); priorExists && (err != nil || !bytes.Equal(got, prior)) {
				t.Fatalf("preview changed prior: %q %v", got, err)
			} else if !priorExists && !os.IsNotExist(err) {
				t.Fatalf("preview wrote projection: %v", err)
			}
			actions := []string{}
			if _, err := fleetApply(context, spec, preview, &actions); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, fleetTestFixture(t, "projection-coordinator.json")) {
				t.Fatalf("coordinator did not receive authored fixture: %s", got)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("mode: %v %v", info, err)
			}
			for _, unwanted := range []string{fleet.WorkerDestination, fleet.ClientDestination} {
				if _, err := os.Stat(context.Path(unwanted)); !os.IsNotExist(err) {
					t.Fatalf("coordinator-only host acquired %s: %v", unwanted, err)
				}
			}
			if err := FleetRestorePrior(context.Home, context.RunID); err != nil {
				t.Fatal(err)
			}
			got, err = os.ReadFile(path)
			if priorExists {
				if err != nil || !bytes.Equal(got, prior) {
					t.Fatalf("rollback: %q %v", got, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("rollback did not restore absence: %v", err)
			}
		})
	}
}
