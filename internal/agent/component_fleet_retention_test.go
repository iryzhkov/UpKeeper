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

func retentionWriteRecord(t *testing.T, c *Context, run string, record any) []byte {
	t.Helper()
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	adapterTestWrite(t, c.Path(fleet.StateDirectory, run, "prior.json"), string(b))
	return b
}

func retentionBadRecords() map[string]any {
	entry := func(fields map[string]any) map[string]any {
		m := map[string]any{"destination": fleet.RoutePolicyDestination, "content": nil, "existed": false}
		for k, v := range fields {
			m[k] = v
		}
		return m
	}
	record := func(items ...any) any { return map[string]any{"documents": items} }
	missing := entry(nil)
	delete(missing, "existed")
	valid := entry(nil)
	return map[string]any{
		"missing-existed":         record(missing),
		"null-existed":            record(entry(map[string]any{"existed": nil})),
		"string-existed":          record(entry(map[string]any{"existed": "false"})),
		"absent-unknown-encoding": record(entry(map[string]any{"encoding": "unknown", "content": ""})),
		"absent-invalid-base64":   record(entry(map[string]any{"encoding": "base64/v1", "content": "%%%"})),
		"absent-content":          record(entry(map[string]any{"content": "not absent"})),
		"absent-encoded-content":  record(entry(map[string]any{"encoding": "base64/v1", "content": "YWJj"})),
		"absent-null-encoded":     record(entry(map[string]any{"encoding": "base64/v1"})),
		"present-null":            record(entry(map[string]any{"existed": true})),
		"missing-content":         record(map[string]any{"destination": fleet.RoutePolicyDestination, "existed": false}),
		"malformed-later":         record(valid, map[string]any{"destination": fleet.WorkerDestination, "existed": true, "content": "%%%", "encoding": "base64/v1"}),
		"nonobject-later":         record(valid, nil),
		"unexpected-destination":  record(valid, map[string]any{"destination": "outside", "existed": false, "content": nil}),
		"duplicate-destination":   record(valid, valid),
		"record-guard":            record(entry(map[string]any{"expected": "forged"})),
		"object-shape":            []any{},
		"list-shape":              map[string]any{"documents": map[string]any{}},
	}
}

// Each public entry point must refuse the entire record before either bytes or
// ownership can change, even when the requested/first entry is valid.
func TestFleetMalformedRetentionBeforeEffects(t *testing.T) {
	for name, record := range retentionBadRecords() {
		for _, mode := range []string{"full", "single", "guarded", "retired-policy", "retired-client", "reused-forward", "reused-retirement", "reused-client-retirement"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				c, component := policyContext(t)
				if _, err := fleetProjection(c, component, fleetTestHost, "initial", false); err != nil {
					t.Fatal(err)
				}
				// Keep absence and an unrelated live path in the same snapshot.
				unrelated := "outside"
				adapterTestWrite(t, c.Path(unrelated), "unrelated bytes")
				paths := []string{fleet.RoutePolicyDestination, fleet.ClientDestination, fleet.WorkerDestination, fleet.CoordinatorDestination, fleetPolicyReceiptPath, fleetClientReceiptPath, unrelated}
				before := make(map[string][]byte)
				for _, path := range paths {
					b, err := os.ReadFile(c.Path(path))
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
					before[path] = b
				}
				run := "malformed"
				if mode == "reused-retirement" {
					run += "-client"
				}
				originalRecord := retentionWriteRecord(t, c, run, record)
				var err error
				switch mode {
				case "full":
					err = FleetRestorePrior(c.Home, run)
				case "single":
					_, err = fleetRestoreRetainedDocument(c.Home, run, fleet.RoutePolicyDestination)
				case "guarded":
					_, err = fleetRestoreRetainedExpected(c.Home, run, fleet.RoutePolicyDestination, before[fleet.RoutePolicyDestination], true)
				case "retired-policy":
					err = fleetRestoreRetiredProjection(c, run, true)
				case "retired-client":
					err = fleetRestoreRetiredProjection(c, run, false)
				case "reused-forward":
					role := component["route_policy"].(map[string]any)["roles"].([]any)[0].(map[string]any)
					role["candidates"].([]any)[0].(map[string]any)["effort"] = "high"
					_, err = fleetProjection(c, component, fleetTestHost, run, false)
				case "reused-retirement":
					_, err = fleetProjection(c, nil, fleetTestHost, "malformed", false)
				case "reused-client-retirement":
					_, err = fleetRetireClient(c, run, false)
				}
				var rollback *RolledBack
				if err == nil || errors.As(err, &rollback) {
					t.Fatalf("malformed record accepted or claimed rollback: %v", err)
				}
				for _, path := range paths {
					got, readErr := os.ReadFile(c.Path(path))
					if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
						t.Fatal(readErr)
					}
					if !fleetSameBytes(got, before[path]) {
						t.Fatalf("%s changed: %q -> %q", path, before[path], got)
					}
				}
				got, err := os.ReadFile(c.Path(fleet.StateDirectory, run, "prior.json"))
				if err != nil || !bytes.Equal(got, originalRecord) {
					t.Fatal("retention changed", err)
				}
			})
		}
	}
}

func TestFleetValidRetentionFormats(t *testing.T) {
	for _, format := range []string{"historical", "base64/v1"} {
		for _, present := range []bool{false, true} {
			for _, mode := range []string{"full", "single", "guarded"} {
				t.Run(format+"/"+map[bool]string{true: "present", false: "absent"}[present]+"/"+mode, func(t *testing.T) {
					c := fleetTestPrepare(t)
					current := []byte("current")
					adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), string(current))
					payload := []byte("historical bytes")
					entry := map[string]any{"destination": fleet.RoutePolicyDestination, "existed": present, "content": ""}
					if format == "base64/v1" {
						payload = []byte{0xff, 0, 0xfe, '\n'}
						entry["encoding"] = format
						if present {
							entry["content"] = base64.StdEncoding.EncodeToString(payload)
						}
					} else if present {
						entry["content"] = string(payload)
					}
					retentionWriteRecord(t, c, "valid", map[string]any{"documents": []any{entry}})
					var err error
					switch mode {
					case "full":
						err = FleetRestorePrior(c.Home, "valid")
					case "single":
						_, err = fleetRestoreRetainedDocument(c.Home, "valid", fleet.RoutePolicyDestination)
					case "guarded":
						_, err = fleetRestoreRetainedExpected(c.Home, "valid", fleet.RoutePolicyDestination, current, true)
					}
					if err != nil {
						t.Fatal(err)
					}
					got, err := os.ReadFile(c.Path(fleet.RoutePolicyDestination))
					if present {
						if err != nil || !bytes.Equal(got, payload) {
							t.Fatalf("lost retained bytes: %q %v", got, err)
						}
					} else if !errors.Is(err, os.ErrNotExist) {
						t.Fatal("absent document restored", err)
					}
				})
			}
		}
	}
	// New writer's null-content absent representation also works.
	c := fleetTestPrepare(t)
	retentionWriteRecord(t, c, "null-absent", map[string]any{"documents": []any{map[string]any{"destination": fleet.RoutePolicyDestination, "existed": false, "content": nil}}})
	if err := FleetRestorePrior(c.Home, "null-absent"); err != nil {
		t.Fatal(err)
	}
}

func TestFleetRuntimeRetentionGuardCannotBeForged(t *testing.T) {
	c := fleetTestPrepare(t)
	adapterTestWrite(t, c.Path(fleet.RoutePolicyDestination), "operator drift")
	record := map[string]any{"documents": []any{map[string]any{"destination": fleet.RoutePolicyDestination, "existed": false, "content": nil}}}
	retentionWriteRecord(t, c, "guard", record)
	if _, err := fleetRestoreRetainedExpected(c.Home, "guard", fleet.RoutePolicyDestination, []byte("expected live bytes"), true); err == nil {
		t.Fatal("guard bypassed")
	}
	if got := policyRead(t, c); string(got) != "operator drift" {
		t.Fatal("drift overwritten")
	}
	if err := fleetRestoreDocument(c.Home, map[string]any{"destination": fleet.RoutePolicyDestination, "existed": false, "content": nil, "expected": []byte("operator drift")}); err == nil {
		t.Fatal("record supplied guard authority")
	}
	if got := policyRead(t, c); string(got) != "operator drift" {
		t.Fatal("forged guard changed bytes")
	}
}
