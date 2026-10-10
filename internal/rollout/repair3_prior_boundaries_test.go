package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func TestAstraFrozenAssetProductionBoundary(t *testing.T) {
	for _, fault := range []bool{false, true} {
		name := "positive"
		if fault {
			name = "journal-assets-wrong-observed-binary"
		}
		t.Run(name, func(t *testing.T) {
			b, f, c, r := productionHostFixture(t)
			s, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = acceptedPackage(c, s); err != nil {
				t.Fatal("invalid baseline", err)
			}
			before, err := os.ReadFile(c.StatePath + ".review-package.json")
			if err != nil {
				t.Fatal(err)
			}
			packageDigest := s.PackageDigest
			rogue := digest([]byte("independently unreviewed binary"))
			if fault {
				var assets assetEvidence
				if err = evidence(s, "assets", &assets); err != nil {
					t.Fatal(err)
				}
				assets.Binaries["linux_amd64"] = rogue
				s.Extra["assets"] = jsonBytes(assets)
				if err = s.Save(); err != nil {
					t.Fatal(err)
				}
			}
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				if name == c.ActivityCommand[0] {
					no := false
					return jsonBytes(ActivityReceipt{Host: args[0], CoordinatorEpoch: args[1], CollectionDeferred: &no, RunningWithoutActivity: &no}), nil
				}
				p, e := f.Run(ctx, dir, stdin, name, args...)
				if fault && stdin == snapshotScript && f.upgraded[c.Canary] {
					kv := keyValues(p)
					kv["binary_sha256"] = rogue
					return encodeKV(kv), e
				}
				return p, e
			})
			err = RunHostWithRuntime(context.Background(), c, b, c.Canary, true, io.Discard, r)
			t.Logf("fault=%v err=%v pulls=%d", fault, err, productionPulls(f))
			if fault {
				if err == nil || !strings.Contains(err.Error(), "immutable reviewed evidence changed: assets") || productionPulls(f) != 0 {
					t.Fatal("wrong refusal boundary", err, productionPulls(f))
				}
			} else if err != nil || productionPulls(f) != 1 {
				t.Fatal("positive control", err, productionPulls(f))
			}
			after, e := os.ReadFile(c.StatePath + ".review-package.json")
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("frozen package changed", e)
			}
			loaded, e := loadState(c)
			if e != nil || loaded.PackageDigest != packageDigest {
				t.Fatal("package digest changed", e)
			}
		})
	}
}

func TestAstraFullVerifyInventoryAndD5(t *testing.T) {
	for _, fault := range []string{"positive", "receipt", "all-phase", "worker-config", "fleet-config", "duplicate"} {
		t.Run(fault, func(t *testing.T) {
			b, f, c, _ := independentFinal(t)
			s, e := loadState(c)
			if e != nil {
				t.Fatal(e)
			}
			if fault == "receipt" || fault == "all-phase" {
				delete(s.Extra, "dropin:agent-b")
				if fault == "all-phase" {
					delete(s.Completed, "dropin:agent-b")
					if e := os.Remove(attemptPath(c, "phase:dropin:agent-b")); e != nil {
						t.Fatal(e)
					}
				}
				if e := s.Save(); e != nil {
					t.Fatal(e)
				}
			}
			if fault == "worker-config" || fault == "fleet-config" || fault == "duplicate" {
				b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
					p, e := f.Run(ctx, dir, stdin, name, args...)
					if name == "upkeeper" && len(args) > 0 && args[0] == "status" && e == nil {
						var doc map[string]any
						if e := json.Unmarshal(p, &doc); e != nil {
							t.Fatal(e)
						}
						for _, row := range doc["hosts"].([]any) {
							h := row.(map[string]any)
							comps := h["components"].([]any)
							if len(comps) != 11 {
								t.Fatal("positive inventory not eleven", len(comps))
							}
							if h["host"] != "agent-b" {
								continue
							}
							if fault == "duplicate" {
								h["components"] = append(comps, comps[0])
								continue
							}
							omitted := "worker-configuration"
							if fault == "fleet-config" {
								omitted = "steward-fleet-configuration"
							}
							filtered := []any{}
							for _, v := range comps {
								if v.(map[string]any)["name"] != omitted {
									filtered = append(filtered, v)
								}
							}
							h["components"] = filtered
						}
						return jsonBytes(doc), nil
					}
					return p, e
				})
			}
			err := VerifyRollout(context.Background(), c, b, true)
			t.Logf("fault=%s full verify=%v", fault, err)
			if (err == nil) != (fault == "positive") {
				t.Fatal("unexpected final acceptance", err)
			}
			if (fault == "worker-config" || fault == "fleet-config") && !strings.Contains(err.Error(), "incomplete component inventory") {
				t.Fatal("wrong refusal", err)
			}
			if fault == "duplicate" && !strings.Contains(err.Error(), "unresolved configuration/drift") {
				t.Fatal("wrong refusal", err)
			}
		})
	}
}
