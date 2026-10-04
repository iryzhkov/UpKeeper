package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func TestFleetPolicyValidatorDestinationDriftPreserved(t *testing.T) {
	for _, variant := range []string{"edit", "replace", "remove", "create", "leaf-link", "parent-link", "ancestor-link"} {
		t.Run(variant, func(t *testing.T) {
			c, component := policyContext(t)
			path := c.Path(fleet.RoutePolicyDestination)
			old, external := []byte("prior"), []byte("external operator bytes")
			if variant != "create" {
				adapterTestWrite(t, path, string(old))
			}
			outside := t.TempDir()
			outsidePolicy := filepath.Join(outside, "route-policy.yaml")
			adapterTestWrite(t, outsidePolicy, string(external))
			calls := 0
			c.Runner = func(cmd procx.Command) procx.Result {
				result := procx.Run(cmd)
				if strings.Contains(strings.Join(cmd.Argv, " "), "policy validate") {
					calls++
					if calls == 2 {
						var err error
						switch variant {
						case "edit", "create":
							err = os.WriteFile(path, external, 0644)
						case "replace":
							err = os.WriteFile(path+".operator", external, 0644)
							if err == nil {
								err = os.Rename(path+".operator", path)
							}
						case "remove":
							err = os.Remove(path)
						case "leaf-link":
							err = os.Remove(path)
							if err == nil {
								err = os.Symlink(outsidePolicy, path)
							}
						case "parent-link":
							err = os.Rename(filepath.Dir(path), filepath.Dir(path)+".operator")
							if err == nil {
								err = os.Symlink(outside, filepath.Dir(path))
							}
						case "ancestor-link":
							adapterTestWrite(t, filepath.Join(outside, "t3-steward", "route-policy.yaml"), string(external))
							err = os.Rename(c.Path(".config"), c.Path(".config.operator"))
							if err == nil {
								err = os.Symlink(outside, c.Path(".config"))
							}
						}
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				return result
			}
			_, err := fleetProjection(c, component, fleetTestHost, "validator-drift", false)
			if err == nil || calls != 2 {
				t.Fatalf("drift accepted (%d): %v", calls, err)
			}
			got, readErr := os.ReadFile(path)
			if variant == "remove" {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatal("external removal overwritten", readErr)
				}
			} else if readErr != nil || !bytes.Equal(got, external) {
				t.Fatalf("external bytes lost: %q %v", got, readErr)
			}
			if variant == "parent-link" || variant == "ancestor-link" {
				saved := filepath.Dir(path) + ".operator/route-policy.yaml"
				if variant == "ancestor-link" {
					saved = c.Path(".config.operator/t3-steward/route-policy.yaml")
				}
				got, err := os.ReadFile(saved)
				if err != nil || !bytes.Equal(got, old) {
					t.Fatal("detached prior modified", err)
				}
			}
		})
	}
}

func TestFleetPolicyNonUTF8RetentionAndFaultRollback(t *testing.T) {
	c, component := policyContext(t)
	old := []byte{'#', 0xff, 0xfe, 0x00, '\n'}
	adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), string(old))
	if _, err := fleetProjection(c, component, fleetTestHost, "binary-adopt", false); err != nil {
		t.Fatal(err)
	}
	if err := FleetRestorePrior(c.Home, "binary-adopt"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old, policyRead(t, c)) {
		t.Fatal("lossy prior retention")
	}
	// A policy activates before a subsequent projection fails; rollback must
	// restore binary bytes, not the replacement characters JSON strings produce.
	document, err := fleet.Parse(component)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := fleetPlanPolicy(c, document.RoutePolicyBytes())
	if err != nil {
		t.Fatal(err)
	}
	blocked := c.Path("blocked")
	adapterTestWrite(t, blocked, "not a directory")
	failing := &fleetDocument{destination: fleet.WorkerDestination, target: filepath.Join(blocked, fleet.WorkerDestination), payload: []byte("{}"), changed: true}
	err = fleetWriteProjection(c, "binary-fault", []*fleetDocument{planned, failing})
	var rolledBack *RolledBack
	if !errors.As(err, &rolledBack) || !bytes.Equal(old, policyRead(t, c)) {
		t.Fatalf("binary rollback: %v", err)
	}
	r, err := fleetReadPolicyReceipt(c)
	if err != nil || !fleetReceiptMatches(r, old) {
		t.Fatal("dishonest policy receipt", err)
	}
	if got, err := fleetRetainedBytes(map[string]any{"content": "legacy"}); err != nil || string(got) != "legacy" {
		t.Fatal("legacy decoding", err)
	}
	for _, entry := range []map[string]any{
		{"content": "%%%", "encoding": "base64/v1"},
		{"content": "YWJj", "encoding": "unknown"},
	} {
		if _, err := fleetRetainedBytes(entry); err == nil {
			t.Fatal("invalid encoding accepted")
		}
	}
}

func TestFleetCombinedRetirementPostMutationFailure(t *testing.T) {
	for _, variant := range []string{"after-mutation", "receipt-failure", "external-drift"} {
		t.Run(variant, func(t *testing.T) {
			c, component := policyContext(t)
			if _, err := fleetProjection(c, component, fleetTestHost, "combined-start", false); err != nil {
				t.Fatal(err)
			}
			coordinator := fleetTestFixture(t, "projection-coordinator.json")
			adapterTestWrite(t, c.Path(fleet.CoordinatorDestination), string(coordinator))
			if err := os.Chmod(c.Path(fleet.CoordinatorDestination), 0600); err != nil {
				t.Fatal(err)
			}
			policyBefore := policyRead(t, c)
			clientBefore, err := os.ReadFile(c.Path(fleet.ClientDestination))
			if err != nil {
				t.Fatal(err)
			}
			receiptBefore, err := os.ReadFile(c.Path(fleetClientReceiptPath))
			if err != nil {
				t.Fatal(err)
			}
			mutated := false
			retire := func(c *Context, run string, dry bool) (bool, error) {
				return fleetRetireClientWithRestore(c, run, dry, func(home string, entry map[string]any, expected []byte) error {
					if err := fleetRestoreDocumentExpected(home, entry, expected, true); err != nil {
						return err
					}
					mutated = true
					if _, err := os.Stat(c.Path(fleet.ClientDestination)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("fault was not after removal", err)
					}
					if variant == "receipt-failure" {
						if err := os.Chmod(c.Path(fleetClientReceiptPath), 0644); err != nil {
							t.Fatal(err)
						}
						return nil // Complete readback, then fail receipt commit.
					}
					if variant == "external-drift" {
						adapterTestWrite(t, c.Path(fleet.ClientDestination), "external client edit")
					}
					return errors.New("injected after client mutation")
				})
			}
			_, err = fleetRetireOwnedProjectionsWithClient(c, "combined-fault", false, retire)
			var rollback *RolledBack
			if err == nil || !mutated {
				t.Fatal("fault did not run", err)
			}
			if variant == "after-mutation" {
				if !errors.As(err, &rollback) {
					t.Fatal("successful recovery status", err)
				}
				got, err := os.ReadFile(c.Path(fleet.ClientDestination))
				if err != nil || !bytes.Equal(got, clientBefore) {
					t.Fatal("client not restored", err)
				}
				got, err = os.ReadFile(c.Path(fleetClientReceiptPath))
				if err != nil || !bytes.Equal(got, receiptBefore) {
					t.Fatal("ownership not restored exactly", err)
				}
			} else {
				if errors.As(err, &rollback) || !strings.Contains(err.Error(), "rollback failed:") {
					t.Fatal("mixed state reported as rollback", err)
				}
				if variant == "external-drift" {
					got, err := os.ReadFile(c.Path(fleet.ClientDestination))
					if err != nil || string(got) != "external client edit" {
						t.Fatal("client drift overwritten", err)
					}
				}
			}
			if !bytes.Equal(policyBefore, policyRead(t, c)) {
				t.Fatal("policy not restored")
			}
			got, err := os.ReadFile(c.Path(fleet.CoordinatorDestination))
			if err != nil || !bytes.Equal(got, coordinator) {
				t.Fatal("coordinator changed after client failure", err)
			}
		})
	}
}
