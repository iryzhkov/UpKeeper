//go:build linux

package author

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

func producerSnapshotRebind(t *testing.T, o *CatalogSnapshotOptions, manifest bool) {
	t.Helper()
	if manifest {
		r, e := canonjson.DecodeObject(snapshotTestRead(t, filepath.Join(o.InputRoot, "inputs/tool-producer-receipt.json")))
		if e != nil {
			t.Fatal(e)
		}
		producerTestManifest(r)
		guidanceSnapshotSet(t, o, "inputs/tool-producer-receipt.json", guidanceTestBytes(t, r))
		guidanceSnapshotEdit(t, o, "inputs/tool-producer-input.json", func(v map[string]any) { v["artifact_manifest_sha256"] = r["artifact_manifest_sha256"] })
	}
	guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) {
		v["catalog_sha256"] = o.InputSHA256["inputs/tool-catalog.json"]
		v["input_sha256"] = o.InputSHA256["inputs/tool-producer-input.json"]
	})
	guidanceSnapshotEdit(t, o, "inputs/tool-guidance-manifest.json", func(v map[string]any) {
		v["catalog_sha256"] = o.InputSHA256["inputs/tool-catalog.json"]
		v["producer_receipt_sha256"] = o.InputSHA256["inputs/tool-producer-receipt.json"]
	})
	guidanceSnapshotEdit(t, o, "inputs/guidance-layout.json", func(v map[string]any) {
		catalogObject(v["bindings"])["guidance_manifest_sha256"] = o.InputSHA256["inputs/tool-guidance-manifest.json"]
	})
	snapshotTestCommit(t, o)
}
func TestCatalogProducerSnapshotActualCustody(t *testing.T) {
	o := snapshotFixture(t)
	s := snapshotTestAcquire(t, o)
	if e := s.Verify(); e != nil {
		t.Fatal(e)
	}
	for p, raw := range producerTestInputs(t) {
		if !bytes.Equal(s.InputBytes(p), []byte(raw)) {
			t.Fatal("original input mismatch", p)
		}
	}
	a, e := InspectCatalogProducerSnapshot(s)
	if e != nil {
		t.Fatal(e)
	}
	c, tr := a.Identity()
	pc, pt := a.HistoricalProducerIdentity()
	if c != o.Commit || tr != o.Tree || pc != "0ff4f4c6882294a59992baf2fed6607ff2467c50" || pt != "bd26c0446428e4268e0895c8918f5aeacde8639e" || c == pc {
		t.Fatal("identity conflation")
	}
	if len(a.VerifiedCategories()) != 7 || len(a.Blockers()) != 2 || !strings.Contains(a.Blockers()[0], "blocked-owner-handoff") || !strings.Contains(a.Blockers()[1], "fixture-cli") {
		t.Fatal("categories/blockers", a)
	}
	future := strings.Join(a.FutureRequired(), " ")
	for _, gate := range []string{"missing Huyang producer descriptor", "producer_source ten", "artifact physical provenance", "route-policy", "actual Stage A", "authentic b32", "runtime/interpreter/tokenizer/helper", "parser", "preservation", "preparation/store/installer", "Stage B NOT_MET"} {
		if !strings.Contains(future, gate) {
			t.Fatal("missing future gate", gate)
		}
	}
	for _, pair := range []struct {
		get  func() []byte
		path string
	}{{a.FullCatalogEncoding, "inputs/tool-catalog.json"}, {a.InputEncoding, "inputs/tool-producer-input.json"}, {a.ReceiptEncoding, "inputs/tool-producer-receipt.json"}} {
		raw := pair.get()
		if !bytes.Equal(raw, s.InputBytes(pair.path)) {
			t.Fatal("metadata changed")
		}
		raw[0] = '!'
		if pair.get()[0] == '!' {
			t.Fatal("mutable encoding")
		}
	}
	a.VerifiedCategories()[0] = "approved"
	a.Blockers()[0] = "ready"
	a.FutureRequired()[0] = "done"
	if a.VerifiedCategories()[0] == "approved" || a.Blockers()[0] == "ready" || a.FutureRequired()[0] == "done" {
		t.Fatal("mutable labels")
	}
	if e = s.Verify(); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	for _, s := range []*CatalogSnapshot{nil, {}, s} {
		a, e := InspectCatalogProducerSnapshot(s)
		if e == nil || len(a.VerifiedCategories()) != 0 || len(a.FullCatalogEncoding()) != 0 {
			t.Fatal("usable invalid owner")
		}
	}
}
func TestCatalogProducerSnapshotReboundRelationships(t *testing.T) {
	cases := []struct {
		name, want string
		manifest   bool
		edit       func(*testing.T, *CatalogSnapshotOptions)
	}{
		{"input-order", "descriptor order", false, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-input.json", func(v map[string]any) { a := catalogArray(v["descriptors"]); a[0], a[1] = a[1], a[0] })
		}},
		{"receipt-descriptor", "input/receipt", false, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) {
				catalogObject(catalogArray(v["descriptors"])[0])["path"] = "descriptors/renamed.json"
			})
		}},
		{"receipt-platform", "input/receipt", false, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) { v["platform"] = "linux-arm64" })
		}},
		{"manifest-crosslink", "manifest", false, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-input.json", func(v map[string]any) { v["artifact_manifest_sha256"] = strings.Repeat("0", 64) })
		}},
		{"source-descriptor", "receipt descriptor source", true, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) {
				catalogObject(catalogArray(catalogObject(catalogArray(v["sources"])[0])["files"])[0])["sha256"] = strings.Repeat("0", 64)
			})
		}},
		{"source-tuple", "catalog source projection", true, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) {
				catalogObject(catalogArray(v["sources"])[0])["commit"] = strings.Repeat("1", 40)
			})
		}},
		{"source-unexplained", "unexplained source file", true, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) {
				s := catalogObject(catalogArray(v["sources"])[0])
				s["files"] = append(catalogArray(s["files"]), map[string]any{"path": "z-extra", "mode": jsonNumber420(), "sha256": strings.Repeat("0", 64)})
			})
		}},
		{"artifact-mode", "identity artifact mapping", true, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) { catalogObject(catalogArray(v["artifacts"])[0])["mode"] = jsonNumber493() })
		}},
		{"artifact-path", "catalog artifact projection", true, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) {
				catalogObject(catalogArray(v["artifacts"])[0])["path"] = "payloads/renamed.txt"
			})
		}},
		{"artifact-inventory", "catalog artifact projection", true, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) { v["artifacts"] = catalogArray(v["artifacts"])[:3] })
		}},
		{"tool-alias", "full four-row tool identities", false, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-catalog.json", func(v map[string]any) { catalogObject(catalogArray(v["tools"])[0])["aliases"] = []any{"another-alias"} })
		}},
		{"tool-platform", "full four-row tool identities", false, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-catalog.json", func(v map[string]any) { catalogObject(catalogArray(v["tools"])[0])["platforms"] = []any{"linux-arm64"} })
		}},
		{"decision-required", "historical decision/platform/result", false, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) { catalogObject(catalogArray(v["decisions"])[0])["result"] = "optional-missing" })
		}},
		{"decision-incompatible", "historical decision/platform/result", false, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) {
				catalogObject(catalogArray(v["decisions"])[1])["result"] = "optional-incompatible"
			})
		}},
		{"decision-inventory", "decision inventory/order", false, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) { v["decisions"] = catalogArray(v["decisions"])[:3] })
		}},
		{"producer-closure", "ten-file closure", false, func(t *testing.T, o *CatalogSnapshotOptions) {
			guidanceSnapshotEdit(t, o, "inputs/tool-producer-receipt.json", func(v map[string]any) {
				p := catalogObject(v["producer_source"])
				p["files"] = catalogArray(p["files"])[:9]
			})
		}},
		{"physical-descriptor-full-row", "retained descriptor full tool record", true, func(t *testing.T, o *CatalogSnapshotOptions) {
			p := "internal/toolproducer/testdata/descriptors/fixture-cli.json"
			raw := snapshotTestRead(t, filepath.Join(o.Repository, p))
			v, e := canonjson.DecodeObject(raw)
			if e != nil {
				t.Fatal(e)
			}
			catalogObject(v["tool"])["aliases"] = []any{"another-alias"}
			raw = guidanceTestBytes(t, v)
			snapshotTestWrite(t, filepath.Join(o.Repository, p), raw, 0644)
			h := digestOf(raw)
			o.SourceSHA256[p] = h
			for _, p := range []string{"inputs/tool-producer-input.json", "inputs/tool-producer-receipt.json"} {
				guidanceSnapshotEdit(t, o, p, func(v map[string]any) {
					catalogObject(catalogArray(v["descriptors"])[0])["sha256"] = h
					if s, ok := v["sources"]; ok {
						catalogObject(catalogArray(catalogObject(catalogArray(s)[0])["files"])[0])["sha256"] = h
					}
				})
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := snapshotFixture(t)
			c.edit(t, &o)
			producerSnapshotRebind(t, &o, c.manifest)
			s := snapshotTestAcquire(t, o)
			if e := s.Verify(); e != nil {
				t.Fatal("rebound did not reach actual verified acquisition", e)
			}
			a, e := InspectCatalogProducerSnapshot(s)
			if e == nil || !strings.Contains(e.Error(), c.want) {
				t.Fatalf("want %s, got %v", c.want, e)
			}
			if len(a.VerifiedCategories()) != 0 || len(a.FullCatalogEncoding()) != 0 || len(a.ReceiptEncoding()) != 0 {
				t.Fatal("partial audit")
			}
		})
	}
}

// Numeric helpers use the same exact canonical Number representation as fixtures.
func jsonNumber420() any { v, _ := canonjson.DecodeObject([]byte("{\"n\":420}")); return v["n"] }
func jsonNumber493() any { v, _ := canonjson.DecodeObject([]byte("{\"n\":493}")); return v["n"] }

func TestCatalogProducerSnapshotRawPins(t *testing.T) {
	for _, key := range []string{"catalog_sha256", "input_sha256"} {
		t.Run(key, func(t *testing.T) {
			o := snapshotFixture(t)
			guidanceSnapshotEdit(t, &o, "inputs/tool-producer-receipt.json", func(v map[string]any) { v[key] = strings.Repeat("0", 64) })
			guidanceSnapshotEdit(t, &o, "inputs/tool-guidance-manifest.json", func(v map[string]any) {
				v["producer_receipt_sha256"] = o.InputSHA256["inputs/tool-producer-receipt.json"]
			})
			guidanceSnapshotEdit(t, &o, "inputs/guidance-layout.json", func(v map[string]any) {
				catalogObject(v["bindings"])["guidance_manifest_sha256"] = o.InputSHA256["inputs/tool-guidance-manifest.json"]
			})
			snapshotTestCommit(t, &o)
			s := snapshotTestAcquire(t, o)
			if e := s.Verify(); e != nil {
				t.Fatal(e)
			}
			a, e := InspectCatalogProducerSnapshot(s)
			if e == nil || !strings.Contains(e.Error(), "input/receipt raw pins") || len(a.VerifiedCategories()) != 0 {
				t.Fatal("raw relationship", e)
			}
		})
	}
}
func TestCatalogProducerSnapshotReboundArtifactDigest(t *testing.T) {
	o := snapshotFixture(t)
	guidanceSnapshotEdit(t, &o, "inputs/tool-producer-receipt.json", func(v map[string]any) {
		for _, x := range catalogArray(v["artifacts"]) {
			catalogObject(x)["sha256"] = strings.Repeat("0", 64)
		}
		for _, x := range catalogArray(catalogObject(catalogArray(v["sources"])[0])["files"]) {
			f := catalogObject(x)
			if f["path"] == "payload.txt" {
				f["sha256"] = strings.Repeat("0", 64)
			}
		}
	})
	producerSnapshotRebind(t, &o, true)
	s := snapshotTestAcquire(t, o)
	if e := s.Verify(); e != nil {
		t.Fatal(e)
	}
	a, e := InspectCatalogProducerSnapshot(s)
	if e == nil || !strings.Contains(e.Error(), "catalog artifact projection") || len(a.VerifiedCategories()) != 0 {
		t.Fatal("rebound artifact declaration", e)
	}
}

func TestCatalogProducerSnapshotMissingProvenance(t *testing.T) {
	o := snapshotFixture(t)
	guidanceSnapshotEdit(t, &o, "inputs/tool-producer-receipt.json", func(v map[string]any) {
		p := catalogObject(v["producer_source"])
		p["commit"] = strings.Repeat("1", 40)
		p["tree"] = strings.Repeat("2", 40)
		catalogObject(catalogArray(p["files"])[0])["sha256"] = strings.Repeat("3", 64)
	})
	producerSnapshotRebind(t, &o, false)
	s := snapshotTestAcquire(t, o)
	a, e := InspectCatalogProducerSnapshot(s)
	if e != nil {
		t.Fatal(e)
	}
	pc, pt := a.HistoricalProducerIdentity()
	if pc != strings.Repeat("1", 40) || pt != strings.Repeat("2", 40) || len(a.Blockers()) != 2 {
		t.Fatal("rewritten historical labels")
	}
	if !strings.Contains(strings.Join(a.FutureRequired(), " "), "producer_source ten implementation blobs/local Git/source build") {
		t.Fatal("unretained producer source was qualified")
	}
}
func TestCatalogProducerSnapshotCustody(t *testing.T) {
	for _, kind := range []string{"input-inode", "source-inode", "mode", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			o := snapshotFixture(t)
			s := snapshotTestAcquire(t, o)
			if _, e := InspectCatalogProducerSnapshot(s); e != nil {
				t.Fatal(e)
			}
			p := filepath.Join(o.InputRoot, "inputs/tool-producer-receipt.json")
			mode := os.FileMode(0600)
			if kind == "source-inode" {
				p = filepath.Join(o.Repository, "internal/toolproducer/testdata/descriptors/fixture-cli.json")
				mode = 0644
			}
			b := snapshotTestRead(t, p)
			switch kind {
			case "input-inode", "source-inode":
				snapshotTestWrite(t, p+".replacement", b, mode)
				if e := os.Rename(p+".replacement", p); e != nil {
					t.Fatal(e)
				}
			case "mode":
				if e := os.Chmod(p, 0644); e != nil {
					t.Fatal(e)
				}
			case "bytes":
				snapshotTestWrite(t, p, append(b, ' '), 0600)
			}
			a, e := InspectCatalogProducerSnapshot(s)
			if e == nil || len(a.VerifiedCategories()) != 0 {
				t.Fatal("lost custody accepted")
			}
		})
	}
}
func TestCatalogProducerSnapshotConcurrentClose(t *testing.T) {
	for round := 0; round < 3; round++ {
		o := snapshotFixture(t)
		s := snapshotTestAcquire(t, o)
		if _, e := InspectCatalogProducerSnapshot(s); e != nil {
			t.Fatal(e)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for n := 0; n < 5; n++ {
					a, e := InspectCatalogProducerSnapshot(s)
					if e == nil {
						c, tr := a.Identity()
						if c != o.Commit || tr != o.Tree || len(a.VerifiedCategories()) != 7 || len(a.Blockers()) != 2 {
							t.Error("incoherent audit")
						}
						b := a.FullCatalogEncoding()
						b[0] = '!'
						if a.FullCatalogEncoding()[0] == '!' {
							t.Error("mutable report")
						}
					} else if len(a.VerifiedCategories()) != 0 {
						t.Error("partial raced refusal")
					}
				}
			}()
		}
		close(start)
		if _, e := InspectCatalogProducerSnapshot(s); e != nil {
			t.Fatal(e)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := s.Close(); e != nil {
				t.Error(e)
			}
		}()
		wg.Wait()
		a, e := InspectCatalogProducerSnapshot(s)
		if e == nil || len(a.VerifiedCategories()) != 0 {
			t.Fatalf("round%d closed owner", round)
		}
	}
}
