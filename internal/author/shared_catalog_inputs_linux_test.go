//go:build linux

package author

import (
	"bytes"
	"context"
	"fmt"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Updates retained fixture input pins, the corresponding source copy and real
// Git identity. Acquisition succeeds, so negatives exercise semantic evidence.
func guidanceSnapshotSet(t *testing.T, o *CatalogSnapshotOptions, p string, raw []byte) {
	t.Helper()
	snapshotTestWrite(t, filepath.Join(o.InputRoot, p), raw, 0600)
	o.InputSHA256[p] = digestOf(raw)
	q := ""
	switch p {
	case "inputs/guidance-layout.json":
		q = "templates/discovery.txt"
	case "inputs/tool-guidance-manifest.json":
		q = "guidance/manifest.json"
	case "inputs/required-global-policy.json":
		q = "guidance/required-global-policy.json"
	default:
		if strings.HasPrefix(p, "inputs/tool-guidance/") {
			q = "guidance/" + strings.TrimPrefix(p, "inputs/tool-guidance/")
		}
	}
	if q != "" {
		q = "shared/catalog-guidance/" + q
		snapshotTestWrite(t, filepath.Join(o.Repository, q), raw, 0644)
		o.SourceSHA256[q] = digestOf(raw)
	}
}
func guidanceSnapshotEdit(t *testing.T, o *CatalogSnapshotOptions, p string, edit func(map[string]any)) {
	t.Helper()
	raw := snapshotTestRead(t, filepath.Join(o.InputRoot, p))
	v, e := guidanceFrozenDecode(raw)
	if e != nil {
		t.Fatal(e)
	}
	edit(v)
	guidanceSnapshotSet(t, o, p, guidanceTestBytes(t, v))
}
func guidanceFrozenDecode(raw []byte) (map[string]any, error) {
	// Use the owning canonical decode via the already validated test encoding.
	return canonjson.DecodeObject(raw)
}
func TestCatalogGuidanceSnapshotActualCustody(t *testing.T) {
	t.Parallel()
	o := snapshotFixture(t)
	s := snapshotTestAcquire(t, o)
	a, e := InspectCatalogGuidanceSnapshot(s)
	if e != nil {
		t.Fatal(e)
	}
	c, tr := a.Identity()
	if c != o.Commit || tr != o.Tree {
		t.Fatal("actual identity")
	}
	if len(a.VerifiedCategories()) != 9 || len(a.FutureRequired()) != 8 || len(a.Blockers()) != 2 {
		t.Fatal("audit categories/blockers", a)
	}
	if !strings.Contains(a.Blockers()[0], "blocked-owner-handoff") || !strings.Contains(a.Blockers()[1], "fixture-cli") {
		t.Fatal("required blockers")
	}
	if !bytes.Equal(a.FullCatalogEncoding(), s.InputBytes("inputs/tool-catalog.json")) || !bytes.Equal(a.ProfileEncoding(), s.InputBytes("inputs/guidance-profile.json")) {
		t.Fatal("metadata loss")
	}
	got := a.FullCatalogEncoding()
	got[0] = '!'
	p := a.ProfileEncoding()
	p[0] = '!'
	a.VerifiedCategories()[0] = "permission"
	a.FutureRequired()[0] = "done"
	a.Blockers()[0] = "ready"
	if a.FullCatalogEncoding()[0] == '!' || a.ProfileEncoding()[0] == '!' || len(a.Blockers()) != 2 || a.VerifiedCategories()[0] == "permission" || a.FutureRequired()[0] == "done" {
		t.Fatal("mutable report")
	}
	if e = s.Verify(); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = InspectCatalogGuidanceSnapshot(s); e == nil {
		t.Fatal("closed")
	}
	if _, e = InspectCatalogGuidanceSnapshot(nil); e == nil {
		t.Fatal("nil")
	}
	if _, e = InspectCatalogGuidanceSnapshot(&CatalogSnapshot{}); e == nil {
		t.Fatal("uninitialized")
	}
	if len((CatalogGuidanceAudit{}).VerifiedCategories()) != 0 {
		t.Fatal("zero audit authority")
	}
}
func TestCatalogGuidanceSnapshotRelationships(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, want string
		edit       func(*testing.T, *CatalogSnapshotOptions)
	}{
		{"manifest-catalog", "digest crosspins", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-guidance-manifest.json", func(v map[string]any) { v["catalog_sha256"] = strings.Repeat("0", 64) })
		}},
		{"manifest-producer", "digest crosspins", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-guidance-manifest.json", func(v map[string]any) { v["producer_receipt_sha256"] = strings.Repeat("0", 64) })
		}},
		{"manifest-policy", "digest crosspins", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-guidance-manifest.json", func(v map[string]any) { v["policy_sha256"] = strings.Repeat("0", 64) })
		}},
		{"producer-input", "digest crosspins", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) { v["input_sha256"] = strings.Repeat("0", 64) })
			h := o.InputSHA256["inputs/tool-producer-receipt.json"]
			guidanceSnapshotEdit(t, o, "inputs/tool-guidance-manifest.json", func(v map[string]any) { v["producer_receipt_sha256"] = h })
		}},
		{"producer-catalog", "digest crosspins", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) { v["catalog_sha256"] = strings.Repeat("0", 64) })
			h := o.InputSHA256["inputs/tool-producer-receipt.json"]
			guidanceSnapshotEdit(t, o, "inputs/tool-guidance-manifest.json", func(v map[string]any) { v["producer_receipt_sha256"] = h })
		}},
		{"catalog-capabilities", "full canonical catalog rows", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-catalog.json", func(v map[string]any) {
				catalogObject(catalogArray(v["tools"])[3])["capabilities"] = []any{"file-read"}
			})
			h := o.InputSHA256["inputs/tool-catalog.json"]
			guidanceSnapshotEdit(t, o, "inputs/tool-guidance-manifest.json", func(v map[string]any) { v["catalog_sha256"] = h })
		}},
		{"catalog-required", "full canonical catalog rows", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-catalog.json", func(v map[string]any) { catalogObject(catalogArray(v["tools"])[0])["required"] = false })
		}},
		{"catalog-platform", "full canonical catalog rows", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-catalog.json", func(v map[string]any) { catalogObject(catalogArray(v["tools"])[0])["platforms"] = []any{"linux-amd64"} })
		}},
		{"catalog-route", "full canonical catalog rows", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-catalog.json", func(v map[string]any) {
				catalogObject(catalogObject(catalogArray(v["tools"])[0])["primary_route"])["name"] = "huyang"
			})
		}},
		{"manifest-package", "package manifest/source bytes", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-guidance-manifest.json", func(v map[string]any) {
				catalogObject(catalogArray(v["records"])[0])["sha256"] = strings.Repeat("0", 64)
			})
			h := o.InputSHA256["inputs/tool-guidance-manifest.json"]
			guidanceSnapshotEdit(t, o, "inputs/guidance-layout.json", func(v map[string]any) { catalogObject(v["bindings"])["guidance_manifest_sha256"] = h })
		}},
		{"profile-rebound-observation", "observation source/descriptor record", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/guidance-observation.json", func(v map[string]any) {
				d := catalogObject(catalogObject(v["source_reads"])["data"])
				r := catalogObject(catalogArray(d["files"])[0])
				r["content"] = r["content"].(string) + "# rebound\n"
			})
			h := o.InputSHA256["inputs/guidance-observation.json"]
			guidanceSnapshotEdit(t, o, "inputs/guidance-profile.json", func(v map[string]any) { catalogObject(v["evidence"])["sha256"] = h })
		}},
		{"profile-observation", "profile observation bytes", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/guidance-profile.json", func(v map[string]any) { catalogObject(v["evidence"])["sha256"] = strings.Repeat("0", 64) })
		}},
		{"layout-manifest", "layout/global/manifest/reference crosslinks", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/guidance-layout.json", func(v map[string]any) {
				catalogObject(v["bindings"])["guidance_manifest_sha256"] = strings.Repeat("0", 64)
			})
		}},
		{"layout-owner", "imported owner bytes", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/guidance-layout.json", func(v map[string]any) {
				catalogObject(v["bindings"])["imported_owner_sha256"] = strings.Repeat("0", 64)
			})
		}},
		{"layout-imported-source", "imported source bytes", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/guidance-layout.json", func(v map[string]any) {
				catalogObject(catalogObject(v["bindings"])["imported_source_sha256"])["config/bin/agents-instructions-gen"] = strings.Repeat("0", 64)
			})
		}},
		{"global-source", "global retained source binding", func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/required-global-policy.json", func(v map[string]any) { catalogObject(v["provenance"])["source_sha256"] = strings.Repeat("0", 64) })
			h := o.InputSHA256["inputs/required-global-policy.json"]
			guidanceSnapshotEdit(t, o, "inputs/tool-guidance-manifest.json", func(v map[string]any) { catalogObject(v["required_global_policy"])["sha256"] = h })
			h2 := o.InputSHA256["inputs/tool-guidance-manifest.json"]
			guidanceSnapshotEdit(t, o, "inputs/guidance-layout.json", func(v map[string]any) {
				b := catalogObject(v["bindings"])
				b["required_global_policy_sha256"] = h
				b["guidance_manifest_sha256"] = h2
			})
		}},
		{"actual-observed-source", "actual observation source parity", func(t *testing.T, o *CatalogSnapshotOptions) {
			path := "shared/agent-instructions/config/claude/skills/implementation-session/SKILL.md"
			raw := snapshotTestRead(t, filepath.Join(o.Repository, path))
			raw = append(raw, []byte("\n# substituted retained source\n")...)
			snapshotTestWrite(t, filepath.Join(o.Repository, path), raw, 0644)
			o.SourceSHA256[path] = digestOf(raw)
			guidanceSnapshotEdit(t, o, "inputs/guidance-layout.json", func(v map[string]any) {
				catalogObject(catalogObject(v["bindings"])["imported_source_sha256"])["config/claude/skills/implementation-session/SKILL.md"] = digestOf(raw)
			})
		}},
		{"source-input-parity", "source/input parity", func(t *testing.T, o *CatalogSnapshotOptions) {
			snapshotTestInputEdit(t, o, "inputs/guidance-profile.json", func(v map[string]any) { v["snapshot_id"] = "allowed fixture change" })
			snapshotTestInputEdit(t, o, "inputs/tool-guidance-manifest.json", func(v map[string]any) { v["catalog_sha256"] = strings.Repeat("0", 64) })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := snapshotFixture(t)
			c.edit(t, &o)
			snapshotTestCommit(t, &o)
			s := snapshotTestAcquire(t, o)
			a, e := InspectCatalogGuidanceSnapshot(s)
			if e == nil || !strings.Contains(e.Error(), c.want) {
				t.Fatalf("expected %s, got %v", c.want, e)
			}
			if len(a.VerifiedCategories()) != 0 || len(a.FullCatalogEncoding()) != 0 {
				t.Fatal("partial audit returned")
			}
		})
	}
}
func TestCatalogGuidanceSnapshotConcurrentClose(t *testing.T) {
	t.Parallel()
	o := snapshotFixture(t)
	s := snapshotTestAcquire(t, o)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 8; n++ {
				a, e := InspectCatalogGuidanceSnapshot(s)
				if e == nil && len(a.VerifiedCategories()) != 9 {
					t.Error("partial coherent audit")
				}
				s.InputBytes("inputs/guidance-profile.json")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	}()
	wg.Wait()
	if _, e := InspectCatalogGuidanceSnapshot(s); e == nil {
		t.Fatal("post-close audit")
	}
}
func TestCatalogGuidanceSnapshotCustodyRefusal(t *testing.T) {
	t.Parallel()
	o := snapshotFixture(t)
	s := snapshotTestAcquire(t, o)
	path := filepath.Join(o.InputRoot, "inputs/guidance-profile.json")
	raw := snapshotTestRead(t, path)
	snapshotTestWrite(t, path, append(raw, ' '), 0600)
	a, e := InspectCatalogGuidanceSnapshot(s)
	if e == nil || len(a.VerifiedCategories()) != 0 {
		t.Fatal("held mutation accepted")
	}
}
func TestCatalogGuidanceSnapshotOptionalMetadata(t *testing.T) {
	t.Parallel()
	for i, labels := range [][3]string{{"unknown", "visible", "denied"}, {"unavailable", "visible", "declaration-only"}, {"observed", "hidden", "unknown"}} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			o := snapshotFixture(t)
			guidanceSnapshotEdit(t, &o, "inputs/guidance-profile.json", func(v map[string]any) {
				r := catalogObject(catalogArray(v["tools"])[1])
				r["availability"] = labels[0]
				r["visibility"] = labels[1]
				r["authorized"] = labels[2]
			})
			s, e := AcquireCatalogSnapshot(context.Background(), o)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			a, e := InspectCatalogGuidanceSnapshot(s)
			if e != nil {
				t.Fatal(e)
			}
			if len(a.Blockers()) != 2 || !bytes.Equal(a.FullCatalogEncoding(), s.InputBytes("inputs/tool-catalog.json")) {
				t.Fatal("metadata/blocker changed")
			}
		})
	}
}
