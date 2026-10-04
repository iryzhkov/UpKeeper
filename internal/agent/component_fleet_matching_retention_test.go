package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/fleet"
)

// Start with actual rendered live bytes, then vary the two ownership boundaries
// independently. No desired field is changed to force the activation branch.
func retentionMatchingContext(t *testing.T, clientState, policyState string) (*Context, map[string]any) {
	t.Helper()
	c, component := policyContext(t)
	if _, err := fleetProjection(c, component, fleetTestHost, "matching-initial", false); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		path, destination, state string
	}{
		{fleetClientReceiptPath, fleet.ClientDestination, clientState},
		{fleetPolicyReceiptPath, fleet.RoutePolicyDestination, policyState},
	} {
		switch item.state {
		case "absent":
			if err := os.Remove(c.Path(item.path)); err != nil {
				t.Fatal(err)
			}
		case "pending":
			body, err := os.ReadFile(c.Path(item.path))
			if err != nil {
				t.Fatal(err)
			}
			var receipt fleetClientReceipt
			if err := json.Unmarshal(body, &receipt); err != nil {
				t.Fatal(err)
			}
			live, err := os.ReadFile(c.Path(item.destination))
			if err != nil {
				t.Fatal(err)
			}
			next := fleetContentHash(live)
			receipt.Active, receipt.Pending = "", &next
			body, err = json.Marshal(&receipt)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(c.Path(item.path), body, 0600); err != nil {
				t.Fatal(err)
			}
		case "existing":
		default:
			t.Fatal("unknown receipt state", item.state)
		}
	}
	observed, err := Handlers()[fleet.Component].Observe(c, component)
	if err != nil || observed["state"] != "converged" {
		t.Fatalf("fixture must have matching desired/live bytes: %v %v", observed, err)
	}
	c.RunID = "matching-retry"
	return c, component
}

func retentionMatchingSnapshot(t *testing.T, c *Context) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	for _, path := range []string{
		fleet.WorkerDestination, fleet.ClientDestination, fleet.CoordinatorDestination,
		fleet.RoutePolicyDestination, fleetClientReceiptPath, fleetPolicyReceiptPath,
		fleet.StateDirectory + "/" + c.RunID + "/prior.json",
	} {
		body, err := os.ReadFile(c.Path(path))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err == nil && body == nil {
			body = []byte{}
		}
		result[path] = body
	}
	return result
}

func retentionMatchingUnchanged(t *testing.T, c *Context, before map[string][]byte) {
	t.Helper()
	after := retentionMatchingSnapshot(t, c)
	for path, body := range before {
		// nil means absent, which must remain distinct from an empty file.
		if !fleetSameBytes(after[path], body) {
			t.Errorf("%s changed: %q -> %q", path, body, after[path])
		}
	}
}

// The independent Astra/Sol reproductions both exercised this registered
// dispatcher with matching bytes. Validate the whole record even when its first
// entry is valid, before either ownership receipt can be prepared or recovered.
func TestFleetMatchingMalformedRetentionDispatch(t *testing.T) {
	for name, record := range retentionBadRecords() {
		for _, clientState := range []string{"existing", "absent", "pending"} {
			for _, policyState := range []string{"existing", "absent", "pending"} {
				t.Run(name+"/"+clientState+"/"+policyState, func(t *testing.T) {
					c, component := retentionMatchingContext(t, clientState, policyState)
					retentionWriteRecord(t, c, c.RunID, record)
					before := retentionMatchingSnapshot(t, c)
					actions := []string{}
					state, err := Handlers()[fleet.Component].Apply(c, component, nil, &actions)
					var rollback *RolledBack
					if err == nil || errors.As(err, &rollback) || state != "" || len(actions) != 0 {
						t.Errorf("expected plain refusal before effects: state=%q error=%v actions=%v", state, err, actions)
					}
					retentionMatchingUnchanged(t, c, before)
				})
			}
		}
	}
}

func TestFleetMatchingValidRetentionAdoption(t *testing.T) {
	for _, format := range []string{"none", "historical", "base64/v1"} {
		for _, clientState := range []string{"existing", "absent", "pending"} {
			for _, policyState := range []string{"existing", "absent", "pending"} {
				t.Run(format+"/"+clientState+"/"+policyState, func(t *testing.T) {
					c, component := retentionMatchingContext(t, clientState, policyState)
					if format != "none" {
						content := "historic unmanaged boundary"
						entry := map[string]any{"destination": fleet.RoutePolicyDestination, "existed": true, "content": content}
						if format == "base64/v1" {
							entry["encoding"] = format
							entry["content"] = base64.StdEncoding.EncodeToString([]byte{0xff, 0, 0xfe, '\n'})
						}
						retentionWriteRecord(t, c, c.RunID, map[string]any{"documents": []any{
							entry, map[string]any{"destination": fleet.ClientDestination, "existed": false, "content": nil},
						}})
					}
					before := retentionMatchingSnapshot(t, c)
					actions := []string{}
					state, err := Handlers()[fleet.Component].Apply(c, component, nil, &actions)
					if err != nil || state != "converged" || len(actions) != 1 {
						t.Fatalf("matching-byte adoption: %q %v %v", state, err, actions)
					}
					for _, item := range []struct {
						path, destination string
						read              func(*Context) (*fleetClientReceipt, error)
					}{
						{fleetClientReceiptPath, fleet.ClientDestination, fleetReadClientReceipt},
						{fleetPolicyReceiptPath, fleet.RoutePolicyDestination, fleetReadPolicyReceipt},
					} {
						receipt, err := item.read(c)
						if err != nil || receipt == nil || receipt.Pending != nil || receipt.Restored || receipt.Active != fleetContentHash(before[item.destination]) {
							t.Fatalf("adoption did not commit active receipt for %s: %+v %v", item.path, receipt, err)
						}
						if (clientState == "absent" && item.path == fleetClientReceiptPath) || (policyState == "absent" && item.path == fleetPolicyReceiptPath) {
							if !receipt.PriorExists || !bytes.Equal(receipt.Prior, before[item.destination]) {
								t.Fatal("adoption lost original matching bytes", item.path)
							}
						}
						delete(before, item.path) // Receipt changes are the intended adoption.
					}
					retentionMatchingUnchanged(t, c, before)
				})
			}
		}
	}
}

func TestFleetMatchingMalformedRetentionDryRun(t *testing.T) {
	for _, receiptState := range []string{"existing", "absent", "pending"} {
		t.Run(receiptState, func(t *testing.T) {
			c, component := retentionMatchingContext(t, receiptState, receiptState)
			retentionWriteRecord(t, c, c.RunID, retentionBadRecords()["malformed-later"])
			before := retentionMatchingSnapshot(t, c)
			c.DryRun = true
			actions := []string{}
			state, err := Handlers()[fleet.Component].Apply(c, component, nil, &actions)
			if err != nil || state != "converged" {
				t.Fatalf("dry-run planning: %q %v", state, err)
			}
			retentionMatchingUnchanged(t, c, before)
		})
	}
}
