package agent

import (
	"bytes"
	"os"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

func TestCoordinatorRetirementPreservesSiblingsAndRetainsPrior(t *testing.T) {
	c := fleetTestPrepare(t)
	c.Host.Name = "normandy"
	spec := fleetTestComponent(t)
	actions := []string{}
	if _, err := fleetApply(c, spec, nil, &actions); err != nil {
		t.Fatal(err)
	}
	path := c.Path(fleet.CoordinatorDestination)
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, sibling := range []string{fleet.WorkerDestination, fleet.ClientDestination} {
		adapterTestWrite(t, c.Path(sibling), "preserve sibling\n")
	}
	c.RunID = "retire-coordinator"
	before, err := fleetObserve(c, nil)
	if err != nil || before["state"] != "updated" {
		t.Fatalf("preview: %#v %v", before, err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, prior) {
		t.Fatal("preview removed active projection")
	}
	if _, err := fleetApply(c, nil, before, &actions); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("projection remains: %v", err)
	}
	if err := FleetRestorePrior(c.Home, c.RunID); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, prior) {
		t.Fatal("retirement did not durably retain exact prior bytes")
	}
	for _, sibling := range []string{fleet.WorkerDestination, fleet.ClientDestination} {
		if got, _ := os.ReadFile(c.Path(sibling)); string(got) != "preserve sibling\n" {
			t.Fatalf("sibling changed: %s", sibling)
		}
	}
}

func TestCoordinatorRetirementRefusesUnknownOrUnsafeDocuments(t *testing.T) {
	for _, kind := range []string{"unknown", "future", "public", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			c := fleetTestPrepare(t)
			path := c.Path(fleet.CoordinatorDestination)
			prior := fleetTestFixture(t, "projection-coordinator.json")
			if kind == "unknown" {
				prior = []byte("operator-owned text\n")
			}
			if kind == "future" {
				prior = bytes.ReplaceAll(prior, []byte(`"schema_version": 1`), []byte(`"schema_version": 999`))
			}
			adapterTestWrite(t, path, string(prior))
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "public" {
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				target := c.Path("operator-file")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fleetRetireCoordinator(c, "refusal", false); err == nil {
				t.Fatal("unsafe retirement accepted")
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, prior) {
				t.Fatal("refusal changed prior bytes")
			}
		})
	}
}
