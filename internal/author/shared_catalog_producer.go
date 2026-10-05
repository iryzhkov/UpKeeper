package author

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// CatalogProducerDocumentKind selects a frozen declaration grammar, never a grant.
type CatalogProducerDocumentKind string

const (
	CatalogProducerCatalog CatalogProducerDocumentKind = "catalog"
	CatalogProducerInput   CatalogProducerDocumentKind = "input"
	CatalogProducerReceipt CatalogProducerDocumentKind = "receipt"
)

// CatalogProducerDocument retains immutable, canonical metadata only.
type CatalogProducerDocument struct {
	kind     CatalogProducerDocumentKind
	encoding []byte
}

func (d CatalogProducerDocument) Kind() CatalogProducerDocumentKind { return d.kind }
func (d CatalogProducerDocument) Encoding() []byte                  { return bytes.Clone(d.encoding) }
func ValidateCatalogProducerDocumentEncoding(k CatalogProducerDocumentKind, raw []byte) error {
	_, e := ParseCatalogProducerDocumentEncoding(k, raw)
	return e
}

// ParseCatalogProducerDocumentEncoding performs no acquisition or execution.
func ParseCatalogProducerDocumentEncoding(k CatalogProducerDocumentKind, raw []byte) (CatalogProducerDocument, error) {
	name := ""
	switch k {
	case CatalogProducerCatalog:
		name = "catalog_v1"
	case CatalogProducerInput:
		name = "producer_input_v1"
	case CatalogProducerReceipt:
		name = "producer_receipt_v1"
	default:
		return CatalogProducerDocument{}, errors.New("unknown catalog producer document kind")
	}
	v, e := producerDecode(raw, name)
	if e == nil {
		e = producerSemantics(k, v)
	}
	if e != nil {
		return CatalogProducerDocument{}, e
	}
	return CatalogProducerDocument{k, bytes.Clone(raw)}, nil
}
func producerRequire(ok bool, at string) error {
	if !ok {
		return fmt.Errorf("catalog producer: %s", at)
	}
	return nil
}
func producerDecode(raw []byte, name string) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > 262144 {
		return nil, errors.New("catalog producer byte bound")
	}
	if e := snapshotCanonical(raw); e != nil {
		return nil, e
	}
	v, e := canonjson.DecodeObject(raw)
	if e != nil {
		return nil, e
	}
	if e = producerShape(v, catalogObject(producerSchemas[name]), name); e != nil {
		return nil, e
	}
	if e = producerSafeScalars(v); e != nil {
		return nil, e
	}
	return v, nil
}

// This private interpreter evaluates only the five frozen definitions below.
// Producer byte lengths and nonnegative int64 bounds remain conjunctive with
// shared canonical/depth/container/text bounds.
func producerShape(v any, s map[string]any, at string) error {
	bad := func() error { return fmt.Errorf("catalog producer shape at %s", at) }
	if c, ok := s["const"]; ok && !catalogEqual(v, c) {
		return bad()
	}
	if a, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range a {
			found = found || catalogEqual(v, x)
		}
		if !found {
			return bad()
		}
	}
	switch s["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return bad()
		}
		p := catalogObject(s["properties"])
		if len(m) != len(p) {
			return bad()
		}
		for k, shape := range p {
			x, ok := m[k]
			if !ok {
				return bad()
			}
			if e := producerShape(x, catalogObject(shape), at+"."+k); e != nil {
				return e
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok || !catalogBound(s, "minItems", int64(len(a)), true) || !catalogBound(s, "maxItems", int64(len(a)), false) {
			return bad()
		}
		seen := map[string]bool{}
		for i, x := range a {
			if e := producerShape(x, catalogObject(s["items"]), fmt.Sprintf("%s[%d]", at, i)); e != nil {
				return e
			}
			b, e := canonjson.Compact(x)
			if e != nil {
				return e
			}
			if s["uniqueItems"] == true && seen[string(b)] {
				return bad()
			}
			seen[string(b)] = true
		}
	case "string":
		x, ok := v.(string)
		if !ok || !catalogBound(s, "minLength", int64(len(x)), true) || !catalogBound(s, "maxLength", int64(len(x)), false) {
			return bad()
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			return bad()
		}
	case "integer":
		// canonical decoding plus catalogScalars guarantees exact integer literals.
		if _, ok := v.(json.Number); !ok {
			return bad()
		}
		n := catalogNumber(v)
		if n < 0 || !catalogBound(s, "minimum", n, true) || !catalogBound(s, "maximum", n, false) {
			return bad()
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return bad()
		}
	}
	return nil
}
func producerSafeScalars(v any) error {
	switch x := v.(type) {
	case string:
		return producerRequire(utf8.ValidString(x) && strings.IndexFunc(x, unicode.IsControl) < 0, "control string")
	case map[string]any:
		for k, e := range x {
			if err := producerSafeScalars(k); err != nil {
				return err
			}
			if err := producerSafeScalars(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := producerSafeScalars(e); err != nil {
				return err
			}
		}
	}
	return nil
}
func producerRepository(s string) bool {
	u, e := url.Parse(s)
	return e == nil && len(s) <= 512 && u.Scheme == "https" && u.Host != "" && u.Hostname() != "" && u.Port() == "" && !strings.Contains(u.Host, ":") && u.User == nil && u.Path != "" && u.Path != "/" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.Contains(s, "#") && u.String() == s
}
func producerOrdered(a []any, key string) bool {
	last := ""
	for _, v := range a {
		s := ""
		if key == "" {
			s = v.(string)
		} else {
			s = catalogObject(v)[key].(string)
		}
		if s <= last {
			return false
		}
		last = s
	}
	return true
}
func producerHas(a []any, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}

var producerVocabulary = map[string]struct {
	kind, route, purpose string
	caps                 []string
}{
	"huyang":              {"mcp-only", "mcp", "Repository source operations declaration.", []string{"file-read", "file-search", "repository-operations", "transactional-edit"}},
	"fixture-cli":         {"cli", "cli", "Synthetic CLI inspection declaration.", []string{"fixture-inspect"}},
	"fixture-mcp":         {"mcp-only", "mcp", "Synthetic MCP metadata declaration.", []string{"fixture-metadata"}},
	"fixture-third-party": {"third-party", "external", "Synthetic third-party reference declaration.", []string{"fixture-reference"}},
}

func producerTools(rows []any) error {
	if e := producerRequire(producerOrdered(rows, "id"), "tool order"); e != nil {
		return e
	}
	names := map[string]bool{}
	for _, row := range rows {
		t := catalogObject(row)
		id := t["id"].(string)
		v, ok := producerVocabulary[id]
		r := catalogObject(t["primary_route"])
		if e := producerRequire(ok && t["canonical_tool"] == id && t["kind"] == v.kind && t["purpose"] == v.purpose && r["kind"] == v.route && r["name"] == id, "tool vocabulary"); e != nil {
			return e
		}
		for _, k := range []string{"aliases", "capabilities", "artifact_ids", "platforms"} {
			if e := producerRequire(producerOrdered(catalogArray(t[k]), ""), "tool list order"); e != nil {
				return e
			}
		}
		for _, c := range catalogArray(t["capabilities"]) {
			found := false
			for _, w := range v.caps {
				found = found || c == w
			}
			if e := producerRequire(found, "tool capability"); e != nil {
				return e
			}
		}
		all := append([]any{id}, catalogArray(t["aliases"])...)
		for _, x := range all {
			n := x.(string)
			_, reserved := producerVocabulary[n]
			if e := producerRequire(n != "agent99" && !names[n] && (!reserved || n == id), "alias collision/reserved"); e != nil {
				return e
			}
			names[n] = true
		}
	}
	return nil
}
func producerDescriptors(rows []any) error {
	if e := producerRequire(producerOrdered(rows, "id"), "descriptor order"); e != nil {
		return e
	}
	paths, refs := map[string]bool{}, map[string]bool{}
	for _, x := range rows {
		d := catalogObject(x)
		_, ok := producerVocabulary[d["id"].(string)]
		p := d["path"].(string)
		q := d["source_path"].(string)
		ref := d["source_id"].(string) + "/" + q
		if e := producerRequire(ok && catalogPath(p, false) && catalogPath(q, false) && !paths[p] && !refs[ref], "descriptor identity/path/reference"); e != nil {
			return e
		}
		paths[p], refs[ref] = true, true
	}
	return nil
}

var producerDeclaredClosure = []string{"go.mod", "go.sum", "internal/canonjson/canonjson.go", "internal/toolcatalog/catalog.go", "internal/toolcatalog/inspect_linux.go", "internal/toolcatalog/inspect_other.go", "internal/toolproducer/compile.go", "internal/toolproducer/files_linux.go", "internal/toolproducer/files_other.go", "internal/toolproducer/schema.go"}

func producerFiles(rows []any) error {
	if e := producerRequire(producerOrdered(rows, "path"), "source file order"); e != nil {
		return e
	}
	for _, x := range rows {
		if e := producerRequire(catalogPath(catalogObject(x)["path"].(string), false), "source file path"); e != nil {
			return e
		}
	}
	return nil
}
func producerSemantics(k CatalogProducerDocumentKind, v map[string]any) error {
	switch k {
	case CatalogProducerInput:
		d, ids := catalogArray(v["descriptors"]), catalogArray(v["tool_ids"])
		if e := producerDescriptors(d); e != nil {
			return e
		}
		if e := producerRequire(producerOrdered(ids, "") && len(ids) == len(d), "input tool inventory/order"); e != nil {
			return e
		}
		for i, x := range d {
			if e := producerRequire(catalogObject(x)["id"] == ids[i], "input descriptor ID"); e != nil {
				return e
			}
		}
	case CatalogProducerReceipt:
		d := catalogArray(v["descriptors"])
		if e := producerDescriptors(d); e != nil {
			return e
		}
		p := catalogObject(v["producer_source"])
		files := catalogArray(p["files"])
		if e := producerRequire(len(files) == len(producerDeclaredClosure), "producer_source ten-file closure"); e != nil {
			return e
		}
		for i, x := range files {
			if e := producerRequire(catalogObject(x)["path"] == producerDeclaredClosure[i], "producer_source closure path/order"); e != nil {
				return e
			}
		}
		if e := producerFiles(files); e != nil {
			return e
		}
		decisions := catalogArray(v["decisions"])
		if e := producerRequire(len(decisions) == len(d) && producerOrdered(decisions, "tool_id"), "decision inventory/order"); e != nil {
			return e
		}
		for i, x := range decisions {
			if e := producerRequire(catalogObject(x)["tool_id"] == catalogObject(d[i])["id"], "decision descriptor ID"); e != nil {
				return e
			}
		}
		manifest := map[string]any{"schema_version": v["schema_version"], "state": v["state"], "sources": v["sources"], "artifacts": v["artifacts"]}
		if e := producerRequire(catalogHash(manifest) == v["artifact_manifest_sha256"], "reconstructed retained manifest digest"); e != nil {
			return e
		}
		return producerReceiptReferences(v)
	case CatalogProducerCatalog:
		sources := catalogArray(v["sources"])
		artifacts := catalogArray(v["artifacts"])
		tools := catalogArray(v["tools"])
		if e := producerRequire(producerOrdered(sources, "id") && producerOrdered(artifacts, "id"), "catalog source/artifact order"); e != nil {
			return e
		}
		if e := producerTools(tools); e != nil {
			return e
		}
		sourceIDs := map[string]bool{}
		for _, x := range sources {
			s := catalogObject(x)
			if e := producerRequire(producerRepository(s["repository"].(string)) && s["revision_kind"] == "git-sha1" && catalogPin(s["revision"].(string), 40), "catalog source tuple"); e != nil {
				return e
			}
			sourceIDs[s["id"].(string)] = true
		}
		as, paths := map[string]map[string]any{}, map[string]bool{}
		for _, x := range artifacts {
			a := catalogObject(x)
			p := a["path"].(string)
			n := catalogNumber(a["mode"])
			if e := producerRequire(sourceIDs[a["source_id"].(string)] && catalogPath(p, false) && !paths[p] && (n == 420 || n == 493), "catalog artifact reference/path/mode"); e != nil {
				return e
			}
			paths[p] = true
			as[a["id"].(string)] = a
		}
		used := map[string]bool{}
		for _, x := range tools {
			t := catalogObject(x)
			for _, id := range catalogArray(t["artifact_ids"]) {
				a, ok := as[id.(string)]
				if e := producerRequire(ok && !used[id.(string)] && a["required"] == t["required"], "catalog artifact ownership"); e != nil {
					return e
				}
				used[id.(string)] = true
			}
		}
		if e := producerRequire(len(used) == len(as), "catalog unexplained artifact"); e != nil {
			return e
		}
	}
	return nil
}
func producerReceiptReferences(v map[string]any) error {
	sources := catalogArray(v["sources"])
	arts := catalogArray(v["artifacts"])
	if e := producerRequire(producerOrdered(sources, "id") && producerOrdered(arts, "id"), "receipt source/artifact order"); e != nil {
		return e
	}
	fs := map[string]map[string]any{}
	used := map[string]bool{}
	for _, x := range sources {
		s := catalogObject(x)
		if e := producerRequire(producerRepository(s["repository"].(string)), "receipt source repository"); e != nil {
			return e
		}
		rows := catalogArray(s["files"])
		if e := producerFiles(rows); e != nil {
			return e
		}
		for _, r := range rows {
			f := catalogObject(r)
			fs[s["id"].(string)+"/"+f["path"].(string)] = f
		}
	}
	if e := producerRequire(len(fs) <= 128, "receipt source file count"); e != nil {
		return e
	}
	paths := map[string]bool{}
	for _, x := range arts {
		a := catalogObject(x)
		p := a["path"].(string)
		q := a["source_path"].(string)
		key := a["source_id"].(string) + "/" + q
		f, ok := fs[key]
		if e := producerRequire(ok && catalogPath(p, false) && catalogPath(q, false) && !paths[p] && f["sha256"] == a["sha256"] && f["mode"] == a["mode"], "receipt identity artifact mapping"); e != nil {
			return e
		}
		paths[p], used[key] = true, true
	}
	for _, x := range catalogArray(v["descriptors"]) {
		d := catalogObject(x)
		key := d["source_id"].(string) + "/" + d["source_path"].(string)
		f, ok := fs[key]
		if e := producerRequire(ok && f["sha256"] == d["sha256"], "receipt descriptor source"); e != nil {
			return e
		}
		used[key] = true
	}
	return producerRequire(len(used) == len(fs), "receipt unexplained source file")
}

// CatalogProducerAudit describes retained declaration consistency only.
// Historical verified decisions do not recertify Stage A or qualify runtime.
type CatalogProducerAudit struct {
	commit, tree, producerCommit, producerTree string
	verified, blockers, future                 []string
	catalog, input, receipt                    []byte
}

func (a CatalogProducerAudit) Identity() (string, string) { return a.commit, a.tree }
func (a CatalogProducerAudit) HistoricalProducerIdentity() (string, string) {
	return a.producerCommit, a.producerTree
}
func (a CatalogProducerAudit) VerifiedCategories() []string {
	return append([]string(nil), a.verified...)
}
func (a CatalogProducerAudit) Blockers() []string          { return append([]string(nil), a.blockers...) }
func (a CatalogProducerAudit) FutureRequired() []string    { return append([]string(nil), a.future...) }
func (a CatalogProducerAudit) FullCatalogEncoding() []byte { return bytes.Clone(a.catalog) }
func (a CatalogProducerAudit) InputEncoding() []byte       { return bytes.Clone(a.input) }
func (a CatalogProducerAudit) ReceiptEncoding() []byte     { return bytes.Clone(a.receipt) }

// InspectCatalogProducerSnapshot holds custody across validation and both
// independent physical rechecks, serializing with Close without nested locking.
func InspectCatalogProducerSnapshot(s *CatalogSnapshot) (CatalogProducerAudit, error) {
	if s == nil {
		return CatalogProducerAudit{}, errors.New("catalog producer nil snapshot")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return inspectCatalogProducerLocked(context.Background(), s)
}

// inspectCatalogProducerLocked is the one semantic body behind both producer
// frontdoors. The caller holds s.mu; ctx is checked between bounded stages.
func inspectCatalogProducerLocked(ctx context.Context, s *CatalogSnapshot) (CatalogProducerAudit, error) {
	zero := CatalogProducerAudit{}
	if s.closed || s.verify == nil || s.close == nil || !catalogPin(s.commit, 40) || !catalogPin(s.tree, 40) || len(s.sources) != 26 || len(s.inputs) != 13 {
		return zero, errors.New("catalog producer closed/uninitialized/incomplete snapshot")
	}
	if e := s.verify(ctx); e != nil {
		return zero, e
	}
	if e := ctx.Err(); e != nil {
		return zero, e
	}
	for p := range snapshotSourceSpecs() {
		b := s.sources[p]
		if len(b) == 0 || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
			return zero, fmt.Errorf("catalog producer retained source %s", p)
		}
	}
	for p := range snapshotInputSpecs() {
		if len(s.inputs[p]) == 0 {
			return zero, fmt.Errorf("catalog producer retained input %s", p)
		}
	}
	old := map[string]map[string]any{}
	for p, k := range map[string]CatalogProducerDocumentKind{"tool-catalog": CatalogProducerCatalog, "tool-producer-input": CatalogProducerInput, "tool-producer-receipt": CatalogProducerReceipt} {
		if e := ctx.Err(); e != nil {
			return zero, e
		}
		raw := s.inputs["inputs/"+p+".json"]
		if e := ValidateCatalogProducerDocumentEncoding(k, raw); e != nil {
			return zero, fmt.Errorf("%s: %w", p, e)
		}
		v, e := canonjson.DecodeObject(raw)
		if e != nil {
			return zero, e
		}
		old[p] = v
	}
	if e := ctx.Err(); e != nil {
		return zero, e
	}
	if e := producerSnapshotRelationships(s.sources, s.inputs, old); e != nil {
		return zero, e
	}
	guidance := map[string]map[string]any{}
	for p, k := range guidanceInputKinds() {
		if e := ctx.Err(); e != nil {
			return zero, e
		}
		raw := s.inputs[p]
		if e := ValidateCatalogGuidanceInputEncoding(k, raw); e != nil {
			return zero, e
		}
		v, e := canonjson.DecodeObject(raw)
		if e != nil {
			return zero, e
		}
		guidance[p] = v
	}
	if e := ctx.Err(); e != nil {
		return zero, e
	}
	if e := guidanceSnapshotRelationships(s.sources, s.inputs, guidance); e != nil {
		return zero, e
	}
	if e := ctx.Err(); e != nil {
		return zero, e
	}
	if e := s.verify(ctx); e != nil {
		return zero, e
	}
	if e := ctx.Err(); e != nil {
		return zero, e
	}
	blockers := []string{"t3-steward: blocked-owner-handoff; catalog-absent"}
	for _, x := range catalogArray(guidance["inputs/guidance-profile.json"]["tools"]) {
		r := catalogObject(x)
		id := r["tool_id"].(string)
		if (id == "fixture-cli" || id == "huyang") && (r["availability"] != "observed" || r["visibility"] != "visible") {
			blockers = append(blockers, id+": required-interface-blocked")
		}
	}
	p := catalogObject(old["tool-producer-receipt"]["producer_source"])
	return CatalogProducerAudit{
		commit: s.commit, tree: s.tree, producerCommit: p["commit"].(string), producerTree: p["tree"].(string),
		verified: []string{"retained physical custody and complete source26/input13 inventory", "three frozen closed producer metadata grammars and finite vocabulary", "retained input/receipt descriptor, platform and raw digest relationships", "receipt source/artifact identity references and reconstructed manifest declaration digest", "catalog source/artifact ownership and historical decision/platform consistency", "three physically retained synthetic descriptor bytes/shape/full tool records", "unchanged nine guidance grammars and available retained guidance relationships"},
		blockers: blockers,
		future: []string{
			"missing Huyang producer descriptor original bytes/physical provenance; never synthesized",
			"producer_source ten implementation blobs/local Git/source build approval and lineage; retained historical tuple is not snapshot identity",
			"original artifact manifest acquisition, synthetic payload/source Git and artifact physical provenance",
			"route-policy owning grammar/configured catalog authorization and release-manifest validation",
			"actual Stage A rerun/build/expected producer receipt authenticity; historical verified decisions are declarations",
			"authentic b32 baseline_main, prior_template, model_block and pinned-reference generated-byte derivation",
			"runtime/interpreter/tokenizer/helper acquisition, parser and execution lineage",
			"projection/output/protocol/recovery, preservation and unchanged token budgets",
			"genuine preparation/store/installer/installed reference accessibility/fleet/runtime grants; Stage B NOT_MET",
			"independent exact candidate review/source CI and verified Steward owning guidance/help handoff",
		},
		catalog: bytes.Clone(s.inputs["inputs/tool-catalog.json"]), input: bytes.Clone(s.inputs["inputs/tool-producer-input.json"]), receipt: bytes.Clone(s.inputs["inputs/tool-producer-receipt.json"]),
	}, nil
}
func producerSnapshotRelationships(sources, inputs map[string][]byte, v map[string]map[string]any) error {
	c, in, r := v["tool-catalog"], v["tool-producer-input"], v["tool-producer-receipt"]
	if e := producerRequire(r["catalog_sha256"] == digestOf(inputs["inputs/tool-catalog.json"]) && r["input_sha256"] == digestOf(inputs["inputs/tool-producer-input.json"]) && r["artifact_manifest_sha256"] == in["artifact_manifest_sha256"] && r["platform"] == in["platform"] && catalogEqual(r["descriptors"], in["descriptors"]), "input/receipt raw pins, manifest, platform, descriptors"); e != nil {
		return e
	}
	// Frozen snapshot identities are a separate contract from standalone vocabulary.
	if e := producerRequire(catalogEqual(c["tools"], guidanceCatalogTools), "snapshot full four-row tool identities"); e != nil {
		return e
	}
	tools := map[string]map[string]any{}
	for _, x := range catalogArray(c["tools"]) {
		t := catalogObject(x)
		tools[t["id"].(string)] = t
	}
	ds, dec := catalogArray(in["descriptors"]), catalogArray(r["decisions"])
	if e := producerRequire(len(ds) == len(tools) && len(dec) == len(tools), "snapshot complete descriptor/decision inventory"); e != nil {
		return e
	}
	owners := map[string]map[string]any{}
	for _, t := range tools {
		for _, id := range catalogArray(t["artifact_ids"]) {
			owners[id.(string)] = t
		}
	}
	projectedArtifacts := []any{}
	usedSources := map[string]bool{}
	for _, x := range catalogArray(r["artifacts"]) {
		a := catalogObject(x)
		t, ok := owners[a["id"].(string)]
		if e := producerRequire(ok && t["required"] == a["required"], "snapshot receipt artifact ownership"); e != nil {
			return e
		}
		projectedArtifacts = append(projectedArtifacts, map[string]any{"id": a["id"], "source_id": a["source_id"], "path": a["path"], "sha256": a["sha256"], "mode": a["mode"], "required": a["required"]})
		usedSources[a["source_id"].(string)] = true
	}
	if e := producerRequire(len(projectedArtifacts) == len(owners) && catalogEqual(c["artifacts"], projectedArtifacts), "snapshot catalog artifact projection/inventory"); e != nil {
		return e
	}
	projectedSources := []any{}
	for _, x := range catalogArray(r["sources"]) {
		s := catalogObject(x)
		if usedSources[s["id"].(string)] {
			projectedSources = append(projectedSources, map[string]any{"id": s["id"], "repository": s["repository"], "revision_kind": "git-sha1", "revision": s["commit"]})
		}
	}
	if e := producerRequire(catalogEqual(c["sources"], projectedSources), "snapshot catalog source projection"); e != nil {
		return e
	}
	for i, x := range ds {
		d := catalogObject(x)
		id := d["id"].(string)
		t, ok := tools[id]
		decision := catalogObject(dec[i])
		if e := producerRequire(ok && decision["tool_id"] == id && producerHas(catalogArray(t["platforms"]), in["platform"].(string)) && (decision["result"] == "verified" || decision["result"] == "optional-missing" && t["required"] == false), "snapshot historical decision/platform/result"); e != nil {
			return e
		}
		if id == "huyang" {
			continue
		} // Missing bytes remain a named future gate.
		raw := sources["internal/toolproducer/testdata/descriptors/"+id+".json"]
		if e := producerRequire(d["source_id"] == "synthetic" && d["path"] == "descriptors/"+id+".json" && d["source_path"] == d["path"] && digestOf(raw) == d["sha256"], "retained descriptor bytes/reference"); e != nil {
			return e
		}
		desc, e := producerDecode(raw, "descriptor_v1")
		if e != nil {
			return e
		}
		if e = producerTools([]any{desc["tool"]}); e != nil {
			return e
		}
		if e = producerRequire(catalogEqual(desc["tool"], t), "retained descriptor full tool record"); e != nil {
			return e
		}
		// The unchanged source26 descriptor mode is 0644; original receipt source
		// mode must agree even though source Git provenance is not retained.
		for _, sx := range catalogArray(r["sources"]) {
			sr := catalogObject(sx)
			if sr["id"] == d["source_id"] {
				for _, fx := range catalogArray(sr["files"]) {
					f := catalogObject(fx)
					if f["path"] == d["source_path"] {
						if e := producerRequire(catalogNumber(f["mode"]) == 420, "retained descriptor source mode"); e != nil {
							return e
						}
					}
				}
			}
		}
	}
	return nil
}

// Exact schemas from archive 8ddcd067..., schemas f097805b.... The four
// producer branches equal owning toolproducer wireSchemas; catalog_v1 is the
// authoritative catalog schema, conjunctive with Stage A vocabulary semantics.
var producerSchemas = guidanceFrozenObject(producerSchemaJSON)

const producerSchemaJSON = `{"artifact_manifest_v1":{"additionalProperties":false,"properties":{"artifacts":{"items":{"additionalProperties":false,"properties":{"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"mode":{"enum":[420,493],"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"required":{"type":"boolean"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"source_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"source_path":{"maxLength":512,"minLength":1,"type":"string"},"transform":{"const":"identity-v1"}},"required":["id","source_id","source_path","path","sha256","mode","required","transform"],"type":"object"},"maxItems":32,"minItems":1,"type":"array","uniqueItems":true},"schema_version":{"const":1,"type":"integer"},"sources":{"items":{"additionalProperties":false,"properties":{"commit":{"pattern":"^[0-9a-f]{40}$","type":"string"},"files":{"items":{"additionalProperties":false,"properties":{"mode":{"enum":[420,493],"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"}},"required":["path","sha256","mode"],"type":"object"},"maxItems":32,"minItems":1,"type":"array","uniqueItems":true},"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"repository":{"pattern":"^https://[^/?#@]+/[^?#]+$","type":"string"},"tree":{"pattern":"^[0-9a-f]{40}$","type":"string"}},"required":["id","repository","commit","tree","files"],"type":"object"},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true},"state":{"const":"candidate-inert","type":"string"}},"required":["schema_version","state","sources","artifacts"],"type":"object"},"catalog_v1":{"additionalProperties":false,"properties":{"artifacts":{"items":{"additionalProperties":false,"properties":{"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"mode":{"maximum":511,"minimum":0,"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"required":{"type":"boolean"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"source_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"}},"required":["id","source_id","path","sha256","mode","required"],"type":"object"},"maxItems":1024,"minItems":0,"type":"array","uniqueItems":true},"schema_version":{"const":1,"type":"integer"},"sources":{"items":{"additionalProperties":false,"properties":{"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"repository":{"type":"string"},"revision":{"type":"string"},"revision_kind":{"enum":["git-sha1","content-sha256"]}},"required":["id","repository","revision_kind","revision"],"type":"object"},"maxItems":128,"minItems":0,"type":"array","uniqueItems":true},"state":{"const":"candidate-inert","type":"string"},"tools":{"items":{"additionalProperties":false,"properties":{"aliases":{"items":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"maxItems":4,"minItems":0,"type":"array","uniqueItems":true},"artifact_ids":{"items":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true},"canonical_tool":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"capabilities":{"items":{"enum":["repository-operations","file-read","file-search","transactional-edit","fixture-inspect","fixture-metadata","fixture-reference"]},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true},"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"kind":{"enum":["cli","mcp-only","third-party"]},"platforms":{"items":{"enum":["linux-amd64","linux-arm64"]},"maxItems":2,"minItems":1,"type":"array","uniqueItems":true},"primary_route":{"additionalProperties":false,"properties":{"kind":{"enum":["cli","mcp","external"]},"name":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"}},"required":["kind","name"],"type":"object"},"purpose":{"maxLength":160,"minLength":1,"type":"string"},"required":{"type":"boolean"}},"required":["id","canonical_tool","aliases","kind","purpose","capabilities","primary_route","artifact_ids","required","platforms"],"type":"object"},"maxItems":256,"minItems":0,"type":"array","uniqueItems":true}},"required":["schema_version","state","sources","artifacts","tools"],"type":"object"},"descriptor_v1":{"additionalProperties":false,"properties":{"schema_version":{"const":1,"type":"integer"},"state":{"const":"candidate-inert","type":"string"},"tool":{"additionalProperties":false,"properties":{"aliases":{"items":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"maxItems":4,"minItems":0,"type":"array","uniqueItems":true},"artifact_ids":{"items":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true},"canonical_tool":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"capabilities":{"items":{"enum":["repository-operations","file-read","file-search","transactional-edit","fixture-inspect","fixture-metadata","fixture-reference"]},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true},"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"kind":{"enum":["cli","mcp-only","third-party"]},"platforms":{"items":{"enum":["linux-amd64","linux-arm64"]},"maxItems":2,"minItems":1,"type":"array","uniqueItems":true},"primary_route":{"additionalProperties":false,"properties":{"kind":{"enum":["cli","mcp","external"]},"name":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"}},"required":["kind","name"],"type":"object"},"purpose":{"maxLength":160,"minLength":1,"type":"string"},"required":{"type":"boolean"}},"required":["id","canonical_tool","aliases","kind","purpose","capabilities","primary_route","artifact_ids","required","platforms"],"type":"object"}},"required":["schema_version","state","tool"],"type":"object"},"producer_input_v1":{"additionalProperties":false,"properties":{"artifact_manifest_sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"descriptors":{"items":{"additionalProperties":false,"properties":{"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"path":{"maxLength":512,"minLength":1,"type":"string"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"source_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"source_path":{"maxLength":512,"minLength":1,"type":"string"}},"required":["id","path","sha256","source_id","source_path"],"type":"object"},"maxItems":4,"minItems":1,"type":"array","uniqueItems":true},"platform":{"enum":["linux-amd64","linux-arm64"]},"schema_version":{"const":1,"type":"integer"},"state":{"const":"candidate-inert","type":"string"},"tool_ids":{"items":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"maxItems":4,"minItems":1,"type":"array","uniqueItems":true}},"required":["schema_version","state","platform","artifact_manifest_sha256","descriptors","tool_ids"],"type":"object"},"producer_receipt_v1":{"additionalProperties":false,"properties":{"artifact_manifest_sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"artifacts":{"items":{"additionalProperties":false,"properties":{"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"mode":{"enum":[420,493],"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"required":{"type":"boolean"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"source_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"source_path":{"maxLength":512,"minLength":1,"type":"string"},"transform":{"const":"identity-v1"}},"required":["id","source_id","source_path","path","sha256","mode","required","transform"],"type":"object"},"maxItems":32,"minItems":1,"type":"array","uniqueItems":true},"catalog_sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"decisions":{"items":{"additionalProperties":false,"properties":{"result":{"enum":["verified","optional-missing","optional-incompatible"]},"tool_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"}},"required":["tool_id","result"],"type":"object"},"maxItems":4,"minItems":1,"type":"array","uniqueItems":true},"descriptors":{"items":{"additionalProperties":false,"properties":{"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"path":{"maxLength":512,"minLength":1,"type":"string"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"source_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"source_path":{"maxLength":512,"minLength":1,"type":"string"}},"required":["id","path","sha256","source_id","source_path"],"type":"object"},"maxItems":4,"minItems":1,"type":"array","uniqueItems":true},"input_sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"platform":{"enum":["linux-amd64","linux-arm64"]},"producer_source":{"additionalProperties":false,"properties":{"commit":{"pattern":"^[0-9a-f]{40}$","type":"string"},"files":{"items":{"additionalProperties":false,"properties":{"mode":{"enum":[420,493],"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"}},"required":["path","sha256","mode"],"type":"object"},"maxItems":32,"minItems":1,"type":"array","uniqueItems":true},"repository":{"const":"https://github.com/iryzhkov/UpKeeper"},"tree":{"pattern":"^[0-9a-f]{40}$","type":"string"}},"required":["repository","commit","tree","files"],"type":"object"},"schema_version":{"const":1,"type":"integer"},"sources":{"items":{"additionalProperties":false,"properties":{"commit":{"pattern":"^[0-9a-f]{40}$","type":"string"},"files":{"items":{"additionalProperties":false,"properties":{"mode":{"enum":[420,493],"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"}},"required":["path","sha256","mode"],"type":"object"},"maxItems":32,"minItems":1,"type":"array","uniqueItems":true},"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"repository":{"pattern":"^https://[^/?#@]+/[^?#]+$","type":"string"},"tree":{"pattern":"^[0-9a-f]{40}$","type":"string"}},"required":["id","repository","commit","tree","files"],"type":"object"},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true},"state":{"const":"candidate-inert","type":"string"}},"required":["schema_version","state","producer_source","input_sha256","artifact_manifest_sha256","descriptors","sources","artifacts","catalog_sha256","platform","decisions"],"type":"object"}}`
