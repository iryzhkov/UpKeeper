package author

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

const producerOriginalInputsGzip = "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"

// producerTestInputsCache holds the decoded originals, inflated once per test
// process; it is never changed after it is filled.
var producerTestInputsCache struct {
	sync.Mutex
	data map[string]string
}

// producerTestInputs returns the caller's own map of the original encodings.
// Strings are immutable, so a copy of the map is a complete copy.
func producerTestInputs(t *testing.T) map[string]string {
	t.Helper()
	c := &producerTestInputsCache
	c.Lock()
	defer c.Unlock()
	if c.data == nil {
		v := map[string]string{}
		if e := json.Unmarshal(guidanceTestInflate(t, producerOriginalInputsGzip), &v); e != nil {
			t.Fatal(e)
		}
		c.data = v
	}
	return maps.Clone(c.data)
}
func producerTestValue(t *testing.T, p string) map[string]any {
	t.Helper()
	v, e := canonjson.DecodeObject([]byte(producerTestInputs(t)[p]))
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func producerTestKinds() map[string]CatalogProducerDocumentKind {
	return map[string]CatalogProducerDocumentKind{"inputs/tool-catalog.json": CatalogProducerCatalog, "inputs/tool-producer-input.json": CatalogProducerInput, "inputs/tool-producer-receipt.json": CatalogProducerReceipt}
}
func TestCatalogProducerDocumentOriginals(t *testing.T) {
	for p, k := range producerTestKinds() {
		t.Run(p, func(t *testing.T) {
			raw := []byte(producerTestInputs(t)[p])
			d, e := ParseCatalogProducerDocumentEncoding(k, raw)
			if e != nil {
				t.Fatal(e)
			}
			expected := bytes.Clone(raw)
			raw[0] = '!'
			copyRaw := d.Encoding()
			copyRaw[0] = '!'
			if d.Kind() != k || !bytes.Equal(d.Encoding(), expected) {
				t.Fatal("mutable/changed document")
			}
		})
	}
	for _, k := range []CatalogProducerDocumentKind{"", "unknown"} {
		if e := ValidateCatalogProducerDocumentEncoding(k, []byte("{}\n")); e == nil {
			t.Fatal("unknown kind")
		}
	}
	if (CatalogProducerDocument{}).Encoding() != nil {
		t.Fatal("zero state")
	}
}
func producerTestDeclaration(t *testing.T, path, name string) ast.Expr {
	t.Helper()
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	f, e := parser.ParseFile(token.NewFileSet(), path, raw, 0)
	if e != nil {
		t.Fatal(e)
	}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range g.Specs {
			v, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range v.Names {
				if n.Name == name {
					return v.Values[i]
				}
			}
		}
	}
	t.Fatal("missing owning declaration", name)
	return nil
}
func TestCatalogProducerDocumentAuthoritativeSchemas(t *testing.T) {
	raw := guidanceTestInflate(t, guidanceOriginalSchemasGzip)
	if digestOf(raw) != "f097805b008917b152c7976f31749397df96ea2c4af408d0d5280648827cebf6" {
		t.Fatal("schema pin")
	}
	v, e := canonjson.DecodeObject(raw)
	if e != nil {
		t.Fatal(e)
	}
	frozen := catalogObject(v["schemas"])
	expr := producerTestDeclaration(t, "../toolproducer/schema.go", "wireSchemas").(*ast.BasicLit)
	s, e := strconv.Unquote(expr.Value)
	if e != nil {
		t.Fatal(e)
	}
	owning, e := canonjson.DecodeObject([]byte(s))
	if e != nil {
		t.Fatal(e)
	}
	for name, actual := range producerSchemas {
		if !catalogEqual(actual, frozen[name]) {
			t.Fatal("authoritative schema mismatch", name)
		}
		if name != "catalog_v1" && !catalogEqual(actual, owning[name]) {
			t.Fatal("owning compiler schema mismatch", name)
		}
		var check func(map[string]any)
		check = func(s map[string]any) {
			if s["type"] == "object" {
				p := catalogObject(s["properties"])
				if s["additionalProperties"] != false || len(catalogArray(s["required"])) != len(p) {
					t.Fatal("not closed", name)
				}
				for _, k := range catalogArray(s["required"]) {
					if p[k.(string)] == nil {
						t.Fatal("required/property mismatch")
					}
				}
				for _, x := range p {
					check(catalogObject(x))
				}
			}
			if s["type"] == "array" {
				check(catalogObject(s["items"]))
			}
		}
		check(catalogObject(actual))
	}
	exprs := producerTestDeclaration(t, "../toolproducer/compile.go", "producerClosure").(*ast.CompositeLit).Elts
	if len(exprs) != 10 || len(producerDeclaredClosure) != 10 {
		t.Fatal("closure size")
	}
	for i, x := range exprs {
		s, e := strconv.Unquote(x.(*ast.BasicLit).Value)
		if e != nil || s != producerDeclaredClosure[i] {
			t.Fatal("actual owning closure mismatch", i)
		}
	}
}
func TestCatalogProducerDocumentEveryClosedField(t *testing.T) {
	t.Parallel()
	count := 0
	for p, k := range producerTestKinds() {
		base := producerTestValue(t, p)
		var paths [][]catalogTestStep
		catalogObjectPaths(base, nil, &paths)
		for i, path := range paths {
			for key, original := range catalogTestAt(base, path) {
				mutations := []string{"missing", "null", "wrong-type", "duplicate"}
				if _, ok := original.(bool); !ok {
					mutations = append(mutations, "bool")
				}
				for _, mutation := range mutations {
					t.Run(fmt.Sprintf("%s/object%d/%s/%s", p, i, key, mutation), func(t *testing.T) {
						t.Parallel()
						v := producerTestValue(t, p)
						o := catalogTestAt(v, path)
						var raw []byte
						switch mutation {
						case "missing":
							delete(o, key)
						case "null":
							o[key] = nil
						case "bool":
							o[key] = true
						case "wrong-type":
							if _, ok := o[key].(string); ok {
								o[key] = json.Number("7")
							} else {
								o[key] = "wrong"
							}
						case "duplicate":
							raw = guidanceTestBytes(t, v)
							needle := []byte(fmt.Sprintf("%q:", key))
							at := bytes.Index(raw, needle)
							if at < 0 {
								t.Fatal("missing key")
							}
							raw = append(append(bytes.Clone(raw[:at]), []byte(fmt.Sprintf("%q: null, ", key))...), raw[at:]...)
						}
						if raw == nil {
							raw = guidanceTestBytes(t, v)
						}
						if e := ValidateCatalogProducerDocumentEncoding(k, raw); e == nil {
							t.Fatal("closed field mutation accepted")
						}
					})
					count++
				}
			}
			t.Run(fmt.Sprintf("%s/object%d/unknown", p, i), func(t *testing.T) {
				t.Parallel()
				v := producerTestValue(t, p)
				catalogTestAt(v, path)["unexpected"] = json.Number("1")
				if e := ValidateCatalogProducerDocumentEncoding(k, guidanceTestBytes(t, v)); e == nil {
					t.Fatal("unknown field")
				}
			})
		}
	}
	t.Logf("closed-field mutations %d", count)
}
func TestCatalogProducerDocumentBoundsAndEncoding(t *testing.T) {
	raw := []byte(producerTestInputs(t)["inputs/tool-producer-input.json"])
	for n, b := range map[string][]byte{"empty": nil, "oversize": bytes.Repeat([]byte(" "), 262145), "BOM": append([]byte{239, 187, 191}, raw...), "UTF8": append([]byte{255}, raw...), "trailing": append(bytes.Clone(raw), []byte("{}")...), "LF": bytes.TrimSuffix(raw, []byte("\n")), "compact": []byte(strings.ReplaceAll(string(raw), "  ", "")), "surrogate": bytes.Replace(raw, []byte("candidate-inert"), []byte("\\ud800"), 1), "NUL": bytes.Replace(raw, []byte("candidate-inert"), []byte("\\u0000"), 1), "control": bytes.Replace(raw, []byte("candidate-inert"), []byte("candidate\\ninert"), 1)} {
		t.Run(n, func(t *testing.T) {
			if e := ValidateCatalogProducerDocumentEncoding(CatalogProducerInput, b); e == nil {
				t.Fatal("encoding accepted")
			}
		})
	}
	for _, n := range []int{65536, 65537} {
		v := producerTestValue(t, "inputs/tool-catalog.json")
		catalogObject(catalogArray(v["tools"])[0])["purpose"] = strings.Repeat("x", n)
		if e := ValidateCatalogProducerDocumentEncoding(CatalogProducerCatalog, guidanceTestBytes(t, v)); e == nil {
			t.Fatal("text/owning purpose bound")
		}
	}
}
func TestCatalogProducerDocumentSemanticCases(t *testing.T) {
	const cp = "inputs/tool-catalog.json"
	const ip = "inputs/tool-producer-input.json"
	const rp = "inputs/tool-producer-receipt.json"
	cases := []struct {
		p    string
		edit func(map[string]any)
	}{
		{ip, func(v map[string]any) { d := catalogArray(v["descriptors"]); d[0], d[1] = d[1], d[0] }},
		{ip, func(v map[string]any) { v["tool_ids"] = []any{"fixture-cli"} }},
		{ip, func(v map[string]any) {
			catalogObject(catalogArray(v["descriptors"])[1])["path"] = catalogObject(catalogArray(v["descriptors"])[0])["path"]
		}},
		{ip, func(v map[string]any) { catalogObject(catalogArray(v["descriptors"])[0])["source_path"] = "../escape" }},
		{ip, func(v map[string]any) { catalogObject(catalogArray(v["descriptors"])[0])["path"] = "descriptors//bad" }},
		{cp, func(v map[string]any) { catalogObject(catalogArray(v["tools"])[0])["aliases"] = []any{"agent99"} }},
		{cp, func(v map[string]any) { catalogObject(catalogArray(v["tools"])[0])["aliases"] = []any{"huyang"} }},
		{cp, func(v map[string]any) {
			catalogObject(catalogArray(v["tools"])[0])["capabilities"] = []any{"file-read"}
		}},
		{cp, func(v map[string]any) {
			catalogObject(catalogObject(catalogArray(v["tools"])[0])["primary_route"])["name"] = "huyang"
		}},
		{cp, func(v map[string]any) { catalogObject(catalogArray(v["artifacts"])[0])["mode"] = json.Number("511") }},
		{cp, func(v map[string]any) { catalogObject(catalogArray(v["artifacts"])[0])["source_id"] = "absent" }},
		{cp, func(v map[string]any) {
			catalogObject(catalogArray(v["artifacts"])[1])["path"] = catalogObject(catalogArray(v["artifacts"])[0])["path"]
		}},
		{cp, func(v map[string]any) {
			catalogObject(catalogArray(v["sources"])[0])["repository"] = "https://user@example.invalid/repo"
		}},
		{cp, func(v map[string]any) {
			catalogObject(catalogArray(v["sources"])[0])["revision"] = strings.Repeat("g", 40)
		}},
		{rp, func(v map[string]any) { catalogObject(catalogArray(v["artifacts"])[0])["transform"] = "other" }},
		{rp, func(v map[string]any) {
			catalogObject(v["producer_source"])["files"] = catalogArray(catalogObject(v["producer_source"])["files"])[:9]
		}},
		{rp, func(v map[string]any) {
			catalogObject(catalogArray(catalogObject(v["producer_source"])["files"])[0])["path"] = "go.other"
		}},
		{rp, func(v map[string]any) { v["decisions"] = catalogArray(v["decisions"])[:3] }},
		{rp, func(v map[string]any) { catalogObject(catalogArray(v["decisions"])[0])["tool_id"] = "huyang" }},
		{rp, func(v map[string]any) {
			catalogObject(catalogArray(v["descriptors"])[0])["sha256"] = strings.Repeat("0", 64)
		}},
		{rp, func(v map[string]any) {
			catalogObject(catalogArray(v["sources"])[0])["files"] = append(catalogArray(catalogObject(catalogArray(v["sources"])[0])["files"]), map[string]any{"path": "z-unused", "mode": json.Number("420"), "sha256": strings.Repeat("0", 64)})
			producerTestManifest(v)
		}},
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			v := producerTestValue(t, c.p)
			c.edit(v)
			if e := ValidateCatalogProducerDocumentEncoding(producerTestKinds()[c.p], guidanceTestBytes(t, v)); e == nil {
				t.Fatal("malformed semantics accepted")
			}
		})
	}
	// Standalone grammar permits owning vocabulary subsets and valid new aliases,
	// capabilities, pins and platform variants; it is not a frozen-fixture matcher.
	c := producerTestValue(t, cp)
	c["tools"] = catalogArray(c["tools"])[:1]
	c["artifacts"] = catalogArray(c["artifacts"])[:1]
	catalogObject(catalogArray(c["tools"])[0])["aliases"] = []any{"other-alias"}
	if e := ValidateCatalogProducerDocumentEncoding(CatalogProducerCatalog, guidanceTestBytes(t, c)); e != nil {
		t.Fatal("valid catalog variant", e)
	}
	in := producerTestValue(t, ip)
	in["descriptors"] = catalogArray(in["descriptors"])[:1]
	in["tool_ids"] = catalogArray(in["tool_ids"])[:1]
	in["platform"] = "linux-arm64"
	if e := ValidateCatalogProducerDocumentEncoding(CatalogProducerInput, guidanceTestBytes(t, in)); e != nil {
		t.Fatal("valid input variant", e)
	}
	r := producerTestValue(t, rp)
	catalogObject(catalogArray(r["decisions"])[1])["result"] = "optional-missing"
	if e := ValidateCatalogProducerDocumentEncoding(CatalogProducerReceipt, guidanceTestBytes(t, r)); e != nil {
		t.Fatal("historical declaration variant", e)
	}
}

func TestCatalogProducerDocumentStructuralBounds(t *testing.T) {
	for _, n := range []int{12, 13} {
		v := producerTestValue(t, "inputs/tool-producer-input.json")
		var x any = "leaf"
		for i := 0; i < n; i++ {
			x = []any{x}
		}
		v["extra"] = x
		e := ValidateCatalogProducerDocumentEncoding(CatalogProducerInput, guidanceTestBytes(t, v))
		if e == nil || !strings.Contains(e.Error(), "depth") {
			t.Fatal("depth bound not reached", n, e)
		}
	}
	v := producerTestValue(t, "inputs/tool-producer-input.json")
	a := make([]any, 1025)
	for i := range a {
		a[i] = "x"
	}
	v["tool_ids"] = a
	if e := ValidateCatalogProducerDocumentEncoding(CatalogProducerInput, guidanceTestBytes(t, v)); e == nil || !strings.Contains(e.Error(), "container bound") {
		t.Fatal("container bound", e)
	}
	v = producerTestValue(t, "inputs/tool-producer-input.json")
	v["extra"] = strings.Repeat("x", 65537)
	if e := ValidateCatalogProducerDocumentEncoding(CatalogProducerInput, guidanceTestBytes(t, v)); e == nil || !strings.Contains(e.Error(), "text byte bound") {
		t.Fatal("text bound", e)
	}
}

func producerTestManifest(v map[string]any) {
	v["artifact_manifest_sha256"] = catalogHash(map[string]any{"schema_version": v["schema_version"], "state": v["state"], "sources": v["sources"], "artifacts": v["artifacts"]})
}
