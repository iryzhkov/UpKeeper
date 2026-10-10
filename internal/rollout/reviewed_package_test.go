package rollout

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func repair2Observer(b *ProductionBackend, f *backendFake) {
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == f.c.ActivityCommand[0] {
			no := false
			return jsonBytes(ActivityReceipt{Host: args[0], CoordinatorEpoch: args[1], CollectionDeferred: &no, RunningWithoutActivity: &no}), nil
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
}
func TestRepair2FrozenPackageEveryField(t *testing.T) {
	keys := []string{"author-started", "release.diff", "release-before", "release-after", "fleet-plan", "preview-full", "preview-scoped", "preview-rollback", "assets", "rc118-findings"}
	faults := []string{"none", "head", "release", "config", "journal-config", "timer-value", "timer-subset", "package-missing", "package-conflict", "review-missing", "review-conflict", "review-duplicate", "assets-tag", "assets-commit", "assets-checksums", "assets-binary-subset", "assets-platform-hash"}
	for _, key := range keys {
		faults = append(faults, "change:"+key, "missing:"+key)
	}
	for _, fault := range faults {
		t.Run(fault, func(t *testing.T) {
			b, f, c, r := productionHostFixture(t)
			s, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "head":
				s.ExpectHead = strings.Repeat("9", 40)
			case "release":
				s.ReleaseSHA256 = strings.Repeat("9", 64)
			case "config":
				c.Assets = map[string]string{"unreviewed": "identity"}
			case "journal-config":
				s.ConfigDigest = strings.Repeat("9", 64)
			case "timer-value":
				s.TimerPrior[c.TimerHosts[0]] = "changed"
			case "timer-subset":
				delete(s.TimerPrior, c.TimerHosts[0])
			case "package-missing":
				if err := os.Remove(c.StatePath + ".review-package.json"); err != nil {
					t.Fatal(err)
				}
			case "package-conflict":
				if err := os.WriteFile(c.StatePath+".review-package.json", []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "assets-tag", "assets-commit", "assets-checksums", "assets-binary-subset", "assets-platform-hash":
				var assets assetEvidence
				if err := evidence(s, "assets", &assets); err != nil {
					t.Fatal(err)
				}
				switch fault {
				case "assets-tag":
					assets.Tag = "unreviewed-tag"
				case "assets-commit":
					assets.Commit = strings.Repeat("9", 40)
				case "assets-checksums":
					assets.ChecksumsSHA256 = strings.Repeat("9", 64)
				case "assets-binary-subset":
					delete(assets.Binaries, "linux_amd64")
				case "assets-platform-hash":
					assets.Binaries["linux_amd64"] = strings.Repeat("9", 64)
				}
				s.Extra["assets"] = jsonBytes(assets)
			case "review-duplicate":
				raw, err := os.ReadFile(c.ReviewFile)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(c.ReviewFile, append(raw, []byte("PACKAGE conflicting\n")...), 0600); err != nil {
					t.Fatal(err)
				}
			case "review-missing":
				if err := os.Remove(c.ReviewFile); err != nil {
					t.Fatal(err)
				}
			case "review-conflict":
				if err := os.WriteFile(c.ReviewFile, []byte("VERDICT: ACCEPT\nPACKAGE wrong"), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if strings.HasPrefix(fault, "change:") {
					s.Extra[strings.TrimPrefix(fault, "change:")] = jsonBytes("unreviewed")
				}
				if strings.HasPrefix(fault, "missing:") {
					delete(s.Extra, strings.TrimPrefix(fault, "missing:"))
				}
			}
			if fault == "config" {
				// Keep the journal config digest aligned with tampered runtime configuration;
				// the immutable package must independently refuse it.
				s.ConfigDigest = c.Digest()
			}
			if err := saveState(c.StatePath, s); err != nil {
				t.Fatal(err)
			}
			repair2Observer(b, f)
			err = RunHostWithRuntime(context.Background(), c, b, c.Canary, true, io.Discard, r)
			if fault == "none" {
				if err != nil || productionPulls(f) != 1 {
					t.Fatalf("positive: %v pulls=%d", err, productionPulls(f))
				}
				loaded, e := loadState(c)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = acceptedPackage(c, loaded); e != nil {
					t.Fatalf("evolving receipts changed package: %v", e)
				}
			} else {
				if err == nil || productionPulls(f) != 0 {
					t.Fatalf("unsafe %s: %v pulls=%d", fault, err, productionPulls(f))
				}
				_ = RunHostWithRuntime(context.Background(), c, b, c.Canary, true, io.Discard, r)
				if productionPulls(f) != 0 {
					t.Fatal("reentry pulled")
				}
			}
		})
	}
}
func TestRepair2FinalCompleteInventory(t *testing.T) {
	b, f, c, _ := independentFinal(t)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"agent-b", "normandy", "homelab", "agent-a", "omarchy-pc", "laptop"} {
		for _, name := range reviewedComponentNames {
			t.Run("omitted/"+host+"/"+name, func(t *testing.T) {
				b.Runner = commandFunc(func(ctx context.Context, dir, stdin, command string, args ...string) ([]byte, error) {
					p, e := f.Run(ctx, dir, stdin, command, args...)
					if command == "upkeeper" && len(args) > 0 && args[0] == "status" && e == nil {
						var doc map[string]any
						_ = json.Unmarshal(p, &doc)
						for _, row := range doc["hosts"].([]any) {
							h := row.(map[string]any)
							if h["host"] == host {
								keep := []any{}
								for _, v := range h["components"].([]any) {
									if v.(map[string]any)["name"] != name {
										keep = append(keep, v)
									}
								}
								h["components"] = keep
							}
						}
						return jsonBytes(doc), nil
					}
					return p, e
				})
				if err := b.finalConfiguration(context.Background(), c, s); err == nil {
					t.Fatal("accepted component omission")
				}
			})
		}
	}
	for _, fault := range []string{"none", "duplicate", "unknown", "conflicting", "ahead", "failed", "updated", "missing-host", "unknown-host", "duplicate-host", "exit20"} {
		t.Run(fault, func(t *testing.T) {
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, command string, args ...string) ([]byte, error) {
				p, e := f.Run(ctx, dir, stdin, command, args...)
				if command == "upkeeper" && len(args) > 0 && args[0] == "status" && e == nil {
					var doc map[string]any
					_ = json.Unmarshal(p, &doc)
					hosts := doc["hosts"].([]any)
					h := hosts[0].(map[string]any)
					comps := h["components"].([]any)
					switch fault {
					case "duplicate":
						h["components"] = append(comps, comps[0])
					case "unknown":
						comps[0].(map[string]any)["name"] = "unreviewed-component"
					case "conflicting":
						extra := map[string]any{"name": comps[0].(map[string]any)["name"], "state": "failed"}
						h["components"] = append(comps, extra)
					case "ahead", "failed", "updated":
						comps[0].(map[string]any)["state"] = fault
					case "missing-host":
						doc["hosts"] = hosts[1:]
					case "unknown-host":
						h["host"] = "unreviewed-host"
					case "duplicate-host":
						doc["hosts"] = append(hosts, h)
					case "exit20":
						doc["exit_code"] = 20
					}
					return jsonBytes(doc), nil
				}
				return p, e
			})
			err := b.finalConfiguration(context.Background(), c, s)
			if (err == nil) != (fault == "none") {
				t.Fatalf("%s: %v", fault, err)
			}
		})
	}
}
func TestRepair2ReviewedInventoryReferences(t *testing.T) {
	for _, fault := range []string{"none", "missing-preview", "duplicate-host", "missing-host", "conflicting-host", "conflicting-host-alias", "component-subset", "duplicate-component", "unknown-component", "release-subset"} {
		t.Run(fault, func(t *testing.T) {
			b, _, c, _ := productionHostFixture(t)
			s, _ := loadState(c)
			pkg, err := acceptedPackage(c, s)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			_ = json.Unmarshal(pkg.Evidence["preview-full"], &doc)
			hosts := doc["hosts"].([]any)
			h := hosts[0].(map[string]any)
			comps := h["components"].([]any)
			switch fault {
			case "missing-preview":
				delete(pkg.Evidence, "preview-full")
			case "duplicate-host":
				doc["hosts"] = append(hosts, h)
			case "missing-host":
				doc["hosts"] = hosts[1:]
			case "conflicting-host":
				h["host"] = "unreviewed-host"
			case "conflicting-host-alias":
				h["name"] = "unreviewed-host"
			case "component-subset":
				h["components"] = comps[1:]
			case "duplicate-component":
				h["components"] = append(comps, comps[0])
			case "unknown-component":
				comps[0].(map[string]any)["name"] = "unreviewed"
			case "release-subset":
				var rel map[string]any
				_ = json.Unmarshal(pkg.Evidence["release-after"], &rel)
				delete(rel["components"].(map[string]any), "worker-configuration")
				pkg.Evidence["release-after"] = jsonBytes(rel)
			}
			if fault != "missing-preview" {
				pkg.Evidence["preview-full"] = jsonBytes(doc)
			}
			_, err = packageComponents(c, pkg)
			if (err == nil) != (fault == "none") {
				t.Fatalf("%s: %v", fault, err)
			}
			_ = b
		})
	}
}
