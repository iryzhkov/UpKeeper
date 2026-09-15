package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

func TestClientReceiptRecoversEveryActivationBoundary(t *testing.T) {
	for _, exists := range []bool{false, true} {
		for _, phase := range []string{"prepared", "activated", "restored"} {
			t.Run(phase+map[bool]string{true: "-existing", false: "-absent"}[exists], func(t *testing.T) {
				c := fleetTestPrepare(t)
				path := c.Path(fleet.ClientDestination)
				var prior []byte
				if exists {
					prior = []byte("{\"original\": true}\n")
					adapterTestWrite(t, path, string(prior))
					if err := os.Chmod(path, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				next := fleetTestFixture(t, "projection-client-homelab.json")
				planned := &fleetDocument{destination: fleet.ClientDestination, target: path, payload: next, before: prior, changed: true}
				if _, err := fleetPrepareClientReceipt(c, []*fleetDocument{planned}); err != nil {
					t.Fatal(err)
				}
				if phase != "prepared" {
					if err := fleetActivate(planned); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "restored" {
					restore := &fleetDocument{destination: fleet.ClientDestination, target: path, payload: prior, before: next, remove: !exists, changed: true}
					if err := fleetRetainPrior(c, "crash-restoration", []*fleetDocument{restore}); err != nil {
						t.Fatal(err)
					}
					if err := fleetRestoreDocument(c.Home, map[string]any{"destination": fleet.ClientDestination, "existed": exists, "content": string(prior)}); err != nil {
						t.Fatal(err)
					}
				}
				for repeat := 0; repeat < 2; repeat++ {
					if _, err := fleetRetireClient(c, "retire", false); err != nil {
						t.Fatal(err)
					}
					got, err := os.ReadFile(path)
					if exists && (err != nil || !bytes.Equal(got, prior)) {
						t.Fatal("original bytes not restored", err)
					}
					if !exists && !os.IsNotExist(err) {
						t.Fatal("original absence not restored", err)
					}
				}
				if err := fleetWriteProjection(c, "reapply", []*fleetDocument{planned}); err != nil {
					t.Fatal(err)
				}
				if _, err := fleetRetireClient(c, "retire-again", false); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(path)
				if exists && (err != nil || !bytes.Equal(got, prior)) {
					t.Fatal("reapply lost original", err)
				}
				if !exists && !os.IsNotExist(err) {
					t.Fatal("reapply lost absence", err)
				}
			})
		}
	}
}

func TestClientReceiptRefusesUnprovedOrUnsafeRestoration(t *testing.T) {
	for _, variant := range []string{"missing", "drift", "receipt-public", "client-public", "receipt-link", "client-link", "receipt-shape", "receipt-oversize", "client-oversize", "null-prior_exists", "null-restored", "null-active"} {
		t.Run(variant, func(t *testing.T) {
			c := fleetTestPrepare(t)
			path := c.Path(fleet.ClientDestination)
			body := fleetTestFixture(t, "projection-client-homelab.json")
			planned := &fleetDocument{destination: fleet.ClientDestination, target: path, payload: body, changed: true}
			if err := fleetWriteProjection(c, "create", []*fleetDocument{planned}); err != nil {
				t.Fatal(err)
			}
			record := c.Path(fleetClientReceiptPath)
			switch variant {
			case "missing":
				if err := os.Remove(record); err != nil {
					t.Fatal(err)
				}
			case "drift":
				body = []byte("unexplained operator change\n")
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			case "receipt-public":
				if err := os.Chmod(record, 0o644); err != nil {
					t.Fatal(err)
				}
			case "client-public":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "receipt-link", "client-link":
				link := record
				if variant == "client-link" {
					link = path
				}
				if err := os.Rename(link, link+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(link+".original", link); err != nil {
					t.Fatal(err)
				}
			case "null-prior_exists", "null-restored", "null-active":
				body, err := os.ReadFile(record)
				if err != nil {
					t.Fatal(err)
				}
				var value map[string]any
				if err := json.Unmarshal(body, &value); err != nil {
					t.Fatal(err)
				}
				value[variant[len("null-"):]] = nil
				body, err = json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(record, body, 0o600); err != nil {
					t.Fatal(err)
				}
			case "receipt-shape":
				if err := os.WriteFile(record, []byte("{\"version\":1}"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "receipt-oversize", "client-oversize":
				target := record
				limit := fleetClientReceiptLimit
				if variant == "client-oversize" {
					target = path
					limit = fleetCoordinatorMaxBytes
				}
				if err := os.Truncate(target, int64(limit+1)); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fleetRetireClient(c, "refused", false); err == nil {
				t.Fatal("unsafe restoration accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed client", err)
			}
			if _, err := os.Stat(c.Path(fleet.StateDirectory, "refused")); !os.IsNotExist(err) {
				t.Fatal("refusal wrote recovery state", err)
			}
		})
	}
}

func TestConvergedClientIsAdoptedWithoutChangingProjection(t *testing.T) {
	c := fleetTestPrepare(t)
	for _, item := range []struct{ path, name string }{
		{fleet.WorkerDestination, "projection-worker-homelab.json"},
		{fleet.ClientDestination, "projection-client-homelab.json"},
	} {
		adapterTestWrite(t, c.Path(item.path), string(fleetTestFixture(t, item.name)))
		if err := os.Chmod(c.Path(item.path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := fleetObserve(c, fleetTestComponent(t))
	if err != nil {
		t.Fatal(err)
	}
	if before["state"] != "converged" {
		t.Fatal(before)
	}
	if _, err := os.Stat(c.Path(fleetClientReceiptPath)); !os.IsNotExist(err) {
		t.Fatal("observation wrote receipt", err)
	}
	actions := []string{}
	state, err := fleetApply(c, fleetTestComponent(t), before, &actions)
	if err != nil || state != "converged" {
		t.Fatal(state, err)
	}
	if _, err := os.Stat(c.Path(fleetClientReceiptPath)); err != nil {
		t.Fatal("converged client was not adopted", err)
	}
	if changed, err := fleetRetireClient(c, "retire-converged", false); err != nil || changed {
		t.Fatal(changed, err)
	}
	got, err := os.ReadFile(c.Path(fleet.ClientDestination))
	if err != nil || !bytes.Equal(got, fleetTestFixture(t, "projection-client-homelab.json")) {
		t.Fatal("preexisting client changed", err)
	}
}

func TestClientRetirementPreflightsCoordinatorBeforeEffects(t *testing.T) {
	c := fleetTestPrepare(t)
	path := c.Path(fleet.ClientDestination)
	body := fleetTestFixture(t, "projection-client-homelab.json")
	planned := &fleetDocument{destination: fleet.ClientDestination, target: path, payload: body, changed: true}
	if err := fleetWriteProjection(c, "create", []*fleetDocument{planned}); err != nil {
		t.Fatal(err)
	}
	recordBefore, err := os.ReadFile(c.Path(fleetClientReceiptPath))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := c.Path(fleet.CoordinatorDestination)
	adapterTestWrite(t, coordinator, "unknown coordinator content\n")
	if err := os.Chmod(coordinator, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetRetireOwnedProjections(c, "refuse", false); err == nil {
		t.Fatal("invalid coordinator accepted")
	}
	clientAfter, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(clientAfter, body) {
		t.Fatal("coordinator refusal mutated client", err)
	}
	recordAfter, err := os.ReadFile(c.Path(fleetClientReceiptPath))
	if err != nil || !bytes.Equal(recordAfter, recordBefore) {
		t.Fatal("coordinator refusal mutated receipt", err)
	}
	if got, err := os.ReadFile(coordinator); err != nil || string(got) != "unknown coordinator content\n" {
		t.Fatal("coordinator changed", err)
	}
}

func TestClientReceiptRejectsSymlinkAncestorWithMissingSuffix(t *testing.T) {
	c := fleetTestPrepare(t)
	outside := t.TempDir()
	parent := c.Path(".local/state")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "dev-fleet")); err != nil {
		t.Fatal(err)
	}
	path := c.Path(fleet.ClientDestination)
	planned := &fleetDocument{destination: fleet.ClientDestination, target: path, payload: fleetTestFixture(t, "projection-client-homelab.json"), changed: true}
	if err := fleetWriteProjection(c, "escape", []*fleetDocument{planned}); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("wrote outside home", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("changed client on refusal", err)
	}
}

func TestClientPlanningRejectsUnsafeFilesBeforeReading(t *testing.T) {
	for _, kind := range []string{"public", "oversize", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			c := fleetTestPrepare(t)
			path := c.Path(fleet.ClientDestination)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if kind == "fifo" {
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				adapterTestWrite(t, path, "{}")
				if kind == "oversize" {
					if err := os.Chmod(path, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Truncate(path, fleetCoordinatorMaxBytes+1); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := fleetObserve(c, fleetTestComponent(t)); err == nil {
				t.Fatal("unsafe client reached planning")
			}
		})
	}
}
