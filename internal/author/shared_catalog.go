package author

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// CatalogDocumentKind selects one frozen inert wire branch, never a runtime grant.
type CatalogDocumentKind string

const (
	CatalogOwner5   CatalogDocumentKind = "owner5"
	CatalogRequest4 CatalogDocumentKind = "request4"
	CatalogSummary4 CatalogDocumentKind = "summary4"
	CatalogReceipt5 CatalogDocumentKind = "receipt5"
)

// CatalogDocument holds validated canonical bytes. Acquisition, external crosspins,
// producer lineage, preparation and retained-store durability are separate gates.
type CatalogDocument struct {
	kind     CatalogDocumentKind
	encoding []byte
}

// Kind returns the frozen branch.
func (d *CatalogDocument) Kind() CatalogDocumentKind { return d.kind }

// Encoding returns a copy, so a caller cannot mutate the validated document.
func (d *CatalogDocument) Encoding() []byte { return bytes.Clone(d.encoding) }

// ValidateCatalogDocumentEncoding validates structure and self-contained bindings.
// Acceptance includes valid blocking audit documents and grants no permission.
func ValidateCatalogDocumentEncoding(kind CatalogDocumentKind, raw []byte) error {
	_, err := ParseCatalogDocumentEncoding(kind, raw)
	return err
}

// ParseCatalogDocumentEncoding returns an immutable validated wire document.
// It performs no filesystem, process, network or environment operations.
func ParseCatalogDocumentEncoding(kind CatalogDocumentKind, raw []byte) (*CatalogDocument, error) {
	name, limit := "", 0
	switch kind {
	case CatalogOwner5:
		name, limit = "overlay_owner_v5", 256<<10
	case CatalogRequest4:
		name, limit = "overlay_request_v4", 262144
	case CatalogSummary4:
		name, limit = "overlay_summary_v4", 65536
	case CatalogReceipt5:
		name, limit = "shared_receipt_v5", 256<<10
	default:
		return nil, errors.New("unknown catalog document kind")
	}
	if len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) {
		return nil, errors.New("catalog wire size or UTF8")
	}
	if err := boundedSharedJSON(raw, 12, 1024); err != nil {
		return nil, err
	}
	v, err := canonjson.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	// Go replaces invalid surrogate escapes when decoding. Exact ASCII canonical
	// readback refuses those lossy decodings, BOMs, alternate escapes and spacing.
	canonical, err := canonjson.MarshalFile(v)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, errors.New("noncanonical catalog wire")
	}
	if err = catalogScalars(v); err != nil {
		return nil, err
	}
	if err = catalogShape(v, catalogFrozenSchemas[name].(map[string]any), "$"); err != nil {
		return nil, err
	}
	if err = catalogRelationships(kind, v); err != nil {
		return nil, err
	}
	return &CatalogDocument{kind, bytes.Clone(raw)}, nil
}

// These four static schema definitions are retained from the approved owning
// helper at base 1ccf068 (helper SHA256 ac7eb52367ada759690c2c7909af6ca53554cae068421152b91a1b2126932c64).
// The interpreter is private and cannot accept caller-supplied schemas.
var catalogFrozenSchemas = func() map[string]any {
	v, err := canonjson.DecodeObject([]byte(catalogSchemaJSON))
	if err != nil {
		panic(err)
	}
	return v
}()

func catalogScalars(v any) error {
	switch x := v.(type) {
	case string:
		if strings.ContainsRune(x, 0) {
			return errors.New("catalog NUL")
		}
	case json.Number:
		if _, err := strconv.ParseInt(string(x), 10, 64); err != nil || strings.ContainsAny(string(x), ".eE+") || string(x) == "-0" {
			return errors.New("catalog integer literal")
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := catalogScalars(k); err != nil {
				return err
			}
			if err := catalogScalars(x[k]); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := catalogScalars(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func catalogEqual(a, b any) bool { return reflect.DeepEqual(a, b) }
func catalogBound(s map[string]any, key string, n int64, lower bool) bool {
	v, ok := s[key]
	if !ok {
		return true
	}
	bound, _ := strconv.ParseInt(string(v.(json.Number)), 10, 64)
	if lower {
		return n >= bound
	}
	return n <= bound
}

func catalogShape(v any, s map[string]any, at string) error {
	fail := func() error { return fmt.Errorf("catalog shape at %s", at) }
	if c, ok := s["const"]; ok && !catalogEqual(c, v) {
		return fail()
	}
	if values, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range values {
			found = found || catalogEqual(e, v)
		}
		if !found {
			return fail()
		}
	}
	if choices, ok := s["oneOf"].([]any); ok {
		matches := 0
		for _, choice := range choices {
			if catalogShape(v, choice.(map[string]any), at) == nil {
				matches++
			}
		}
		if matches != 1 {
			return fail()
		}
	}
	switch s["type"] {
	case "object":
		x, ok := v.(map[string]any)
		if !ok {
			return fail()
		}
		properties := s["properties"].(map[string]any)
		// Every object in these frozen branches is closed and all listed keys required.
		if len(x) != len(properties) {
			return fail()
		}
		keys := make([]string, 0, len(properties))
		for k := range properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			e, present := x[k]
			if !present {
				return fail()
			}
			if err := catalogShape(e, properties[k].(map[string]any), at+"."+k); err != nil {
				return err
			}
		}
	case "array":
		x, ok := v.([]any)
		if !ok || !catalogBound(s, "minItems", int64(len(x)), true) || !catalogBound(s, "maxItems", int64(len(x)), false) {
			return fail()
		}
		prefix, _ := s["prefixItems"].([]any)
		seen := map[string]bool{}
		for i, e := range x {
			var child map[string]any
			if i < len(prefix) {
				child = prefix[i].(map[string]any)
			} else {
				if s["items"] == false {
					return fail()
				}
				child, _ = s["items"].(map[string]any)
			}
			if child != nil {
				if err := catalogShape(e, child, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
			if s["uniqueItems"] == true {
				b, _ := canonjson.Compact(e)
				if seen[string(b)] {
					return fail()
				}
				seen[string(b)] = true
			}
		}
	case "string":
		x, ok := v.(string)
		if !ok || !catalogBound(s, "minLength", int64(utf8.RuneCountInString(x)), true) || !catalogBound(s, "maxLength", int64(utf8.RuneCountInString(x)), false) {
			return fail()
		}
		if pattern, ok := s["pattern"].(string); ok {
			valid := false
			switch pattern {
			case "^[0-9a-f]{64}$":
				valid = catalogPin(x, 64)
			case "^[0-9a-f]{40}$":
				valid = catalogPin(x, 40)
			case "^/":
				valid = catalogPath(x, true)
			default:
				// The only remaining frozen patterns are lexical absolute or relative
				// paths. RE2 has no lookarounds; enforce their obligations directly.
				valid = catalogPath(x, strings.HasPrefix(pattern, "^/"))
			}
			if !valid {
				return fail()
			}
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return fail()
		}
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil || !catalogBound(s, "minimum", i, true) || !catalogBound(s, "maximum", i, false) {
			return fail()
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fail()
		}
	}
	return nil
}

func catalogPin(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func catalogPath(s string, absolute bool) bool {
	if s == "" || len(s) > 512 || strings.Contains(s, "\\") || strings.HasPrefix(s, "/") != absolute {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	if absolute {
		s = strings.TrimPrefix(s, "/")
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." || p == ".." || len(p) > 255 {
			return false
		}
	}
	return true
}
func catalogHash(v any) string {
	b, _ := canonjson.MarshalFile(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func catalogObject(v any) map[string]any { return v.(map[string]any) }
func catalogArray(v any) []any           { return v.([]any) }
func catalogNumber(v any) int64          { n, _ := strconv.ParseInt(string(v.(json.Number)), 10, 64); return n }

var catalogToolIDs = []string{"fixture-cli", "fixture-mcp", "fixture-third-party", "huyang"}
var catalogInstructionPaths = []string{".claude/agents/critical-reviewer.md", ".claude/agents/executor.md", ".claude/agents/planner.md", ".claude/agents/reader.md", ".claude/agents/reviewer.md", ".claude/omarchy-setup/CLAUDE.md", ".codex/AGENTS.md", ".codex/critical-reviewer.config.toml", ".codex/executor.config.toml", ".codex/planner.config.toml", ".codex/reader.config.toml", ".codex/reviewer.config.toml", ".config/agents/AGENTS.md", ".config/agents/huyang.md", ".config/agents/jocasta.md", ".config/agents/operational-guidance.md", ".config/agents/ov-memory.md", ".config/agents/t3-campaign-executor-briefs.md", ".config/agents/t3-steward.md"}
var catalogSkillPaths = []string{"huyang/SKILL.md", "implementation-session/SKILL.md", "implementation-session/references/prompt-template.md", "ov-memory-curation/SKILL.md", "ov-memory/SKILL.md", "publish-agent-tooling/SKILL.md", "t3-campaign/SKILL.md", "t3-campaign/references/executor-briefs.md", "t3-task/SKILL.md", "t3-wait/SKILL.md"}
var catalogNewInputPaths = []string{"inputs/guidance-layout.json", "inputs/guidance-observation.json", "inputs/guidance-profile.json", "inputs/required-global-policy.json", "inputs/tool-guidance-manifest.json", "inputs/tool-guidance/fixture-cli.json", "inputs/tool-guidance/fixture-mcp.json", "inputs/tool-guidance/fixture-third-party.json", "inputs/tool-guidance/huyang.json"}
var catalogMetadataPaths = []string{"metadata/guidance/fragments/fixture-cli.md", "metadata/guidance/fragments/fixture-mcp.md", "metadata/guidance/fragments/fixture-third-party.md", "metadata/guidance/fragments/huyang.md", "metadata/guidance/menu.json", "metadata/guidance/menu.txt"}

func catalogFileInventory() map[string]string {
	paths := map[string]string{}
	for _, p := range catalogInstructionPaths {
		paths[p] = "instruction"
	}
	for _, p := range catalogSkillPaths {
		paths["skills/"+p] = "skill"
	}
	for _, p := range catalogNewInputPaths {
		paths[p] = "guidance-input"
	}
	for _, p := range catalogMetadataPaths {
		paths[p] = "guidance-metadata"
	}
	paths["inputs/route-policy.json"] = "policy-input"
	paths["inputs/tool-producer-receipt.json"] = "producer-receipt"
	paths["inputs/tool-catalog.json"] = "catalog-input"
	paths["inputs/tool-producer-input.json"] = "catalog-input"
	return paths
}

func catalogRelationships(kind CatalogDocumentKind, v map[string]any) error {
	fail := func(s string) error { return fmt.Errorf("catalog binding: %s", s) }
	switch kind {
	case CatalogOwner5:
		if !catalogEqual(v["import_source_files"], catalogStrings(SharedSourcePaths())) ||
			!catalogEqual(v["skill_roots"], catalogStrings([]string{".agents/skills", ".claude/skills"})) ||
			!catalogEqual(v["future_merges"], catalogStrings([]string{".claude/CLAUDE.md:omarchy-setup import fence", ".config/opencode/opencode.json:instructions"})) {
			return fail("owner inventory")
		}
	case CatalogRequest4:
		for k, x := range v {
			if strings.HasSuffix(k, "_path") || strings.HasSuffix(k, "_root") {
				if !catalogPath(x.(string), true) {
					return fail("absolute lexical path")
				}
			}
		}
	case CatalogSummary4:
		g := catalogObject(v["guidance"])
		if err := catalogProjection(g); err != nil {
			return err
		}
		for _, k := range []string{"catalog_sha256", "policy_sha256", "producer_receipt_sha256"} {
			if v[k] != g[k] {
				return fail(k)
			}
		}
		if v["template_sha256"] != g["layout_sha256"] {
			return fail("summary layout")
		}
		return catalogOutputBindings(catalogArray(v["files"]), g)
	case CatalogReceipt5:
		p := catalogObject(v["policy"])
		c := catalogObject(v["catalog"])
		g := catalogObject(v["guidance"])
		projection := catalogObject(g["projection"])
		expectedPolicy := map[string]string{
			"input_contract": "canonical-release-manifest-v1", "policy_format": "route-policy/v1-canonical-json",
			"derivation": "author-derive-policy-v1", "upstream_repository": "github.com/iryzhkov/omarchy-setup",
			"upstream_commit": "b32c0b6f48e12b7d74a60f8a010f84bab73d299f", "upstream_tree": "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52",
			"interpretation": "steward-parser-and-pinned-generator", "configured_catalog_validation": "completed-at-preparation",
			"current_authorization": "unknown", "readiness": "unassessed",
		}
		for k, want := range expectedPolicy {
			if p[k] != want {
				return fail("policy " + k)
			}
		}
		if err := catalogProjection(projection); err != nil {
			return err
		}
		if catalogHash(projection) != g["projection_sha256"] {
			return fail("projection hash")
		}
		for a, b := range map[string]string{"manifest_sha256": "guidance_manifest_sha256", "profile_sha256": "profile_sha256", "global_policy_sha256": "required_global_policy_sha256", "layout_sha256": "layout_sha256"} {
			if g[a] != projection[b] {
				return fail("guidance " + a)
			}
		}
		for _, k := range []string{"catalog_sha256", "producer_receipt_sha256"} {
			if c[k] != projection[k] {
				return fail("catalog " + k)
			}
		}
		if p["canonical_policy_sha256"] != projection["policy_sha256"] {
			return fail("policy pin")
		}
		files := catalogArray(v["files"])
		inventory := catalogFileInventory()
		names := make([]string, 0, len(inventory))
		for n := range inventory {
			names = append(names, n)
		}
		sort.Strings(names)
		byPath := map[string]map[string]any{}
		for i, f := range files {
			r := catalogObject(f)
			name := names[i]
			mode := int64(420)
			if strings.HasPrefix(name, "inputs/") {
				mode = 384
			}
			if r["path"] != name || r["logical_destination"] != name || r["kind"] != inventory[name] || catalogNumber(r["mode"]) != mode {
				return fail("payload inventory")
			}
			byPath[name] = r
		}
		for field, path := range map[string]string{
			"manifest_sha256": "inputs/tool-guidance-manifest.json", "profile_sha256": "inputs/guidance-profile.json",
			"observation_sha256": "inputs/guidance-observation.json", "global_policy_sha256": "inputs/required-global-policy.json", "layout_sha256": "inputs/guidance-layout.json",
		} {
			if g[field] != byPath[path]["sha256"] {
				return fail("input " + field)
			}
		}
		for field, path := range map[string]string{"input_sha256": "inputs/tool-producer-input.json", "catalog_sha256": "inputs/tool-catalog.json", "producer_receipt_sha256": "inputs/tool-producer-receipt.json"} {
			if c[field] != byPath[path]["sha256"] {
				return fail("catalog input " + field)
			}
		}
		if p["canonical_policy_sha256"] != byPath["inputs/route-policy.json"]["sha256"] {
			return fail("policy payload")
		}
		packages := catalogArray(g["packages"])
		if len(packages) != 4 {
			return fail("packages count")
		}
		decisions := catalogArray(projection["decisions"])
		sources := catalogObject(v["source_sha256"])
		for field, source := range map[string]string{
			"manifest_sha256":      "shared/catalog-guidance/guidance/manifest.json",
			"global_policy_sha256": "shared/catalog-guidance/guidance/required-global-policy.json",
			"layout_sha256":        "shared/catalog-guidance/templates/discovery.txt",
		} {
			if g[field] != sources[source] {
				return fail("source/input " + field)
			}
		}
		for _, skill := range catalogSkillPaths {
			if byPath["skills/"+skill]["sha256"] != sources["shared/agent-instructions/config/claude/skills/"+skill] {
				return fail("unchanged skill source")
			}
		}
		if v["owner_descriptor_sha256"] != sources["shared/catalog-guidance/owner.json"] {
			return fail("owner descriptor")
		}
		for i, pack := range packages {
			r := catalogObject(pack)
			id := catalogToolIDs[i]
			path := "guidance/" + id + ".json"
			d := catalogObject(decisions[i])
			owner := catalogObject(d["source_owner"])
			if r["tool_id"] != id || r["path"] != path || r["sha256"] != d["guidance_sha256"] || r["sha256"] != byPath["inputs/tool-guidance/"+id+".json"]["sha256"] || r["sha256"] != sources["shared/catalog-guidance/"+path] {
				return fail("package crosspin")
			}
			if owner["source_sha256"] != sources[owner["source_path"].(string)] {
				return fail("decision source")
			}
		}
		outputs := make([]any, 0, 25)
		for _, f := range files {
			r := catalogObject(f)
			if r["kind"] == "instruction" || r["kind"] == "guidance-metadata" {
				outputs = append(outputs, r)
			}
		}
		return catalogOutputBindings(outputs, projection)
	}
	return nil
}
func catalogStrings(x []string) []any {
	v := make([]any, len(x))
	for i, s := range x {
		v[i] = s
	}
	return v
}

func catalogProjection(g map[string]any) error {
	fail := func(s string) error { return fmt.Errorf("catalog projection: %s", s) }
	for k, v := range g {
		if strings.HasSuffix(k, "_sha256") && !catalogPin(v.(string), 64) {
			return fail(k)
		}
	}
	if g["owner_handoff"] != "blocked-owner-handoff" {
		return fail("owner handoff")
	}
	decisions := catalogArray(g["decisions"])
	if len(decisions) != 4 {
		return fail("decision count")
	}
	blocked := false
	for i, e := range decisions {
		d := catalogObject(e)
		id := catalogToolIDs[i]
		if d["tool_id"] != id {
			return fail("decision IDs/order")
		}
		decision := d["decision"].(string)
		switch decision {
		case "required-interface-blocked":
			if id != "fixture-cli" && id != "huyang" {
				return fail("optional blocker")
			}
			blocked = true
		case "visible-observed":
		case "hidden", "declaration-only", "unavailable":
			if id == "fixture-cli" || id == "huyang" {
				return fail("required interface")
			}
		default:
			return fail("decision enum")
		}
		for _, k := range []string{"fragment_sha256", "guidance_sha256", "help_sha256"} {
			if !catalogPin(d[k].(string), 64) {
				return fail(k)
			}
		}
		rules := []string{}
		if id == "huyang" {
			rules = []string{"huyang.repository-boundary.v1"}
		}
		if !catalogEqual(d["rule_ids"], catalogStrings(rules)) {
			return fail("rule IDs")
		}
		o := catalogObject(d["source_owner"])
		source := "internal/toolproducer/testdata/descriptors/" + id + ".json"
		kind := "catalog-fixture"
		evidence := "inputs/tool-catalog.json"
		if id == "huyang" {
			source = "shared/agent-instructions/config/claude/skills/huyang/SKILL.md"
			kind = "imported-guidance-proposal"
			evidence = "baseline/.config/agents/AGENTS.md"
		}
		if o["kind"] != kind || o["owning_path"] != "shared/catalog-guidance/guidance/"+id+".json" || o["source_path"] != source || o["evidence_path"] != evidence || o["status"] != "hypothetical-new-owner-input-unapproved" {
			return fail("source owner")
		}
		if !catalogPin(o["source_sha256"].(string), 64) || !catalogPin(o["evidence_sha256"].(string), 64) {
			return fail("source pins")
		}
		if id != "huyang" && o["evidence_sha256"] != g["catalog_sha256"] {
			return fail("catalog evidence")
		}
	}
	status := "fixture-projection-only"
	if blocked {
		status = "required-interface-blocked"
	}
	if g["status"] != status {
		return fail("blocking status")
	}
	files := catalogArray(g["files"])
	expected := []string{"discovery.txt", "final-instructions/.claude/omarchy-setup/CLAUDE.md", "final-instructions/.codex/AGENTS.md", "final-instructions/.config/agents/AGENTS.md", "final-instructions/.config/agents/operational-guidance.md", "fragments/fixture-cli.md", "fragments/fixture-mcp.md", "fragments/fixture-third-party.md", "fragments/huyang.md", "menu.json", "menu.txt"}
	if len(files) != len(expected) {
		return fail("projection inventory count")
	}
	empty := sha256.Sum256(nil)
	emptyHash := hex.EncodeToString(empty[:])
	for i, e := range files {
		f := catalogObject(e)
		n := catalogNumber(f["bytes"])
		if f["path"] != expected[i] || catalogNumber(f["mode"]) != 420 || n < 0 || n > 65536 || !catalogPin(f["sha256"].(string), 64) {
			return fail("projection file")
		}
		if (n == 0) != (f["sha256"] == emptyHash) {
			return fail("empty digest")
		}
		if strings.HasPrefix(expected[i], "fragments/") {
			d := catalogObject(decisions[i-5])
			decision := d["decision"]
			suppressed := decision == "hidden" || decision == "declaration-only" || decision == "unavailable"
			if (n == 0) != suppressed || f["sha256"] != d["fragment_sha256"] {
				return fail("fragment binding")
			}
		} else if n == 0 {
			return fail("empty required output")
		}
	}
	return nil
}

func catalogOutputBindings(files []any, g map[string]any) error {
	byPath := map[string]any{}
	for _, e := range files {
		f := catalogObject(e)
		byPath[f["path"].(string)] = f["sha256"]
	}
	huyang := catalogObject(catalogArray(g["decisions"])[3])
	if huyang["help_sha256"] != byPath[".config/agents/huyang.md"] {
		return errors.New("catalog Huyang reference help binding")
	}
	for _, e := range catalogArray(g["files"]) {
		f := catalogObject(e)
		p := f["path"].(string)
		if p == "discovery.txt" {
			continue
		}
		if strings.HasPrefix(p, "final-instructions/") {
			p = strings.TrimPrefix(p, "final-instructions/")
		} else {
			p = "metadata/guidance/" + p
		}
		if byPath[p] != f["sha256"] {
			return fmt.Errorf("catalog output binding: %s", p)
		}
	}
	return nil
}

const catalogSchemaJSON = `{
  "overlay_owner_v5": {
    "additionalProperties": false,
    "properties": {
      "catalog_input_contract": {
        "const": "offline-tool-producer-receipt-v1"
      },
      "evidence_source_files": {
        "const": [
          "internal/toolproducer/testdata/descriptors/fixture-cli.json",
          "internal/toolproducer/testdata/descriptors/fixture-mcp.json",
          "internal/toolproducer/testdata/descriptors/fixture-third-party.json"
        ]
      },
      "future_merges": {
        "items": {
          "type": "string"
        },
        "maxItems": 2,
        "minItems": 2,
        "type": "array",
        "uniqueItems": true
      },
      "import_commit": {
        "const": "b32c0b6f48e12b7d74a60f8a010f84bab73d299f"
      },
      "import_repository": {
        "const": "github.com/iryzhkov/omarchy-setup"
      },
      "import_source_files": {
        "items": {
          "maxLength": 512,
          "minLength": 1,
          "type": "string"
        },
        "maxItems": 13,
        "minItems": 13,
        "type": "array",
        "uniqueItems": true
      },
      "import_tree": {
        "const": "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52"
      },
      "instruction_files": {
        "const": [
          ".claude/agents/critical-reviewer.md",
          ".claude/agents/executor.md",
          ".claude/agents/planner.md",
          ".claude/agents/reader.md",
          ".claude/agents/reviewer.md",
          ".claude/omarchy-setup/CLAUDE.md",
          ".codex/AGENTS.md",
          ".codex/critical-reviewer.config.toml",
          ".codex/executor.config.toml",
          ".codex/planner.config.toml",
          ".codex/reader.config.toml",
          ".codex/reviewer.config.toml",
          ".config/agents/AGENTS.md",
          ".config/agents/huyang.md",
          ".config/agents/jocasta.md",
          ".config/agents/operational-guidance.md",
          ".config/agents/ov-memory.md",
          ".config/agents/t3-campaign-executor-briefs.md",
          ".config/agents/t3-steward.md"
        ]
      },
      "overlay_source_files": {
        "const": [
          "config/bin/agents-tool-project",
          "templates/discovery.txt",
          "guidance/manifest.json",
          "guidance/required-global-policy.json",
          "guidance/fixture-cli.json",
          "guidance/fixture-mcp.json",
          "guidance/fixture-third-party.json",
          "guidance/huyang.json"
        ]
      },
      "owner": {
        "const": "upkeeper"
      },
      "policy_input_contract": {
        "const": "canonical-release-manifest-v1"
      },
      "retired_reference": {
        "const": ".config/agents/agent99.md"
      },
      "schema_version": {
        "const": 5,
        "type": "integer"
      },
      "skill_roots": {
        "items": {
          "maxLength": 512,
          "minLength": 1,
          "type": "string"
        },
        "maxItems": 2,
        "minItems": 2,
        "type": "array",
        "uniqueItems": true
      },
      "source_repository": {
        "const": "github.com/iryzhkov/UpKeeper"
      },
      "state": {
        "const": "candidate-inert",
        "type": "string"
      }
    },
    "required": [
      "schema_version",
      "owner",
      "state",
      "source_repository",
      "import_repository",
      "import_commit",
      "import_tree",
      "import_source_files",
      "overlay_source_files",
      "instruction_files",
      "skill_roots",
      "future_merges",
      "retired_reference",
      "policy_input_contract",
      "catalog_input_contract",
      "evidence_source_files"
    ],
    "type": "object"
  },
  "overlay_request_v4": {
    "additionalProperties": false,
    "properties": {
      "catalog_path": {
        "maxLength": 512,
        "minLength": 2,
        "pattern": "^/(?!.*(?:^|/)\\.\\.?(?:/|$))[^\\\\\\x00-\\x1f\\x7f]+(?<!/)$",
        "type": "string"
      },
      "catalog_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "guidance_manifest_path": {
        "maxLength": 4096,
        "minLength": 1,
        "pattern": "^/",
        "type": "string"
      },
      "guidance_manifest_sha256": {
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "imported_files": {
        "items": false,
        "maxItems": 18,
        "minItems": 18,
        "prefixItems": [
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/agents/critical-reviewer.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/agents/executor.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/agents/planner.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/agents/reader.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/agents/reviewer.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/omarchy-setup/CLAUDE.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/AGENTS.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/critical-reviewer.config.toml",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/executor.config.toml",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/planner.config.toml",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/reader.config.toml",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/reviewer.config.toml",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/AGENTS.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/huyang.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/jocasta.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/ov-memory.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/t3-campaign-executor-briefs.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/t3-steward.md",
                "type": "string"
              },
              "sha256": {
                "maxLength": 64,
                "minLength": 64,
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "path",
              "sha256",
              "mode"
            ],
            "type": "object"
          }
        ],
        "type": "array"
      },
      "imported_output_root": {
        "maxLength": 512,
        "minLength": 2,
        "pattern": "^/(?!.*(?:^|/)\\.\\.?(?:/|$))[^\\\\\\x00-\\x1f\\x7f]+(?<!/)$",
        "type": "string"
      },
      "observation_path": {
        "maxLength": 4096,
        "minLength": 1,
        "pattern": "^/",
        "type": "string"
      },
      "observation_sha256": {
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "platform": {
        "enum": [
          "linux-amd64",
          "linux-arm64"
        ],
        "type": "string"
      },
      "policy_input_path": {
        "maxLength": 512,
        "minLength": 2,
        "pattern": "^/(?!.*(?:^|/)\\.\\.?(?:/|$))[^\\\\\\x00-\\x1f\\x7f]+(?<!/)$",
        "type": "string"
      },
      "policy_input_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "producer_input_path": {
        "maxLength": 512,
        "minLength": 2,
        "pattern": "^/(?!.*(?:^|/)\\.\\.?(?:/|$))[^\\\\\\x00-\\x1f\\x7f]+(?<!/)$",
        "type": "string"
      },
      "producer_input_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "producer_receipt_path": {
        "maxLength": 512,
        "minLength": 2,
        "pattern": "^/(?!.*(?:^|/)\\.\\.?(?:/|$))[^\\\\\\x00-\\x1f\\x7f]+(?<!/)$",
        "type": "string"
      },
      "producer_receipt_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "profile_path": {
        "maxLength": 4096,
        "minLength": 1,
        "pattern": "^/",
        "type": "string"
      },
      "profile_sha256": {
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "schema_version": {
        "const": 4,
        "type": "integer"
      },
      "state": {
        "const": "candidate-inert",
        "type": "string"
      },
      "template_path": {
        "maxLength": 512,
        "minLength": 2,
        "pattern": "^/(?!.*(?:^|/)\\.\\.?(?:/|$))[^\\\\\\x00-\\x1f\\x7f]+(?<!/)$",
        "type": "string"
      },
      "template_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "tokenizer_manifest_path": {
        "maxLength": 512,
        "minLength": 2,
        "pattern": "^/(?!.*(?:^|/)\\.\\.?(?:/|$))[^\\\\\\x00-\\x1f\\x7f]+(?<!/)$",
        "type": "string"
      },
      "tokenizer_manifest_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "tokenizer_root": {
        "maxLength": 512,
        "minLength": 2,
        "pattern": "^/(?!.*(?:^|/)\\.\\.?(?:/|$))[^\\\\\\x00-\\x1f\\x7f]+(?<!/)$",
        "type": "string"
      }
    },
    "required": [
      "schema_version",
      "state",
      "platform",
      "catalog_path",
      "catalog_sha256",
      "producer_receipt_path",
      "producer_receipt_sha256",
      "producer_input_path",
      "producer_input_sha256",
      "policy_input_path",
      "policy_input_sha256",
      "imported_output_root",
      "imported_files",
      "template_path",
      "template_sha256",
      "tokenizer_manifest_path",
      "tokenizer_manifest_sha256",
      "tokenizer_root",
      "guidance_manifest_path",
      "guidance_manifest_sha256",
      "profile_path",
      "profile_sha256",
      "observation_path",
      "observation_sha256"
    ],
    "type": "object"
  },
  "overlay_summary_v4": {
    "additionalProperties": false,
    "properties": {
      "catalog_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "discovery_tokens": {
        "maximum": 1200,
        "minimum": 1,
        "type": "integer"
      },
      "files": {
        "items": false,
        "maxItems": 25,
        "minItems": 25,
        "prefixItems": [
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/agents/critical-reviewer.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/agents/executor.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/agents/planner.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/agents/reader.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/agents/reviewer.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".claude/omarchy-setup/CLAUDE.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/AGENTS.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/critical-reviewer.config.toml"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/executor.config.toml"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/planner.config.toml"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/reader.config.toml"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".codex/reviewer.config.toml"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/AGENTS.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/huyang.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/jocasta.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/operational-guidance.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/ov-memory.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/t3-campaign-executor-briefs.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": ".config/agents/t3-steward.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": "metadata/guidance/fragments/fixture-cli.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": "metadata/guidance/fragments/fixture-mcp.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": "metadata/guidance/fragments/fixture-third-party.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": "metadata/guidance/fragments/huyang.md"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": "metadata/guidance/menu.json"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          },
          {
            "additionalProperties": false,
            "properties": {
              "mode": {
                "const": 420,
                "type": "integer"
              },
              "path": {
                "const": "metadata/guidance/menu.txt"
              },
              "sha256": {
                "pattern": "^[0-9a-f]{64}$",
                "type": "string"
              }
            },
            "required": [
              "mode",
              "path",
              "sha256"
            ],
            "type": "object"
          }
        ],
        "type": "array"
      },
      "guidance": {
        "additionalProperties": false,
        "properties": {
          "catalog_sha256": {
            "maxLength": 65536,
            "minLength": 1,
            "type": "string"
          },
          "decisions": {
            "items": {
              "additionalProperties": false,
              "properties": {
                "decision": {
                  "maxLength": 65536,
                  "minLength": 1,
                  "type": "string"
                },
                "fragment_sha256": {
                  "maxLength": 65536,
                  "minLength": 1,
                  "type": "string"
                },
                "guidance_sha256": {
                  "maxLength": 65536,
                  "minLength": 1,
                  "type": "string"
                },
                "help_sha256": {
                  "maxLength": 65536,
                  "minLength": 1,
                  "type": "string"
                },
                "rule_ids": {
                  "items": {
                    "maxLength": 65536,
                    "minLength": 1,
                    "type": "string"
                  },
                  "maxItems": 1024,
                  "type": "array"
                },
                "source_owner": {
                  "additionalProperties": false,
                  "properties": {
                    "evidence_path": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "evidence_sha256": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "kind": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "owning_path": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "source_path": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "source_sha256": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "status": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    }
                  },
                  "required": [
                    "evidence_path",
                    "evidence_sha256",
                    "kind",
                    "owning_path",
                    "source_path",
                    "source_sha256",
                    "status"
                  ],
                  "type": "object"
                },
                "tool_id": {
                  "maxLength": 65536,
                  "minLength": 1,
                  "type": "string"
                }
              },
              "required": [
                "decision",
                "fragment_sha256",
                "guidance_sha256",
                "help_sha256",
                "rule_ids",
                "source_owner",
                "tool_id"
              ],
              "type": "object"
            },
            "maxItems": 1024,
            "type": "array"
          },
          "files": {
            "items": {
              "additionalProperties": false,
              "properties": {
                "bytes": {
                  "type": "integer"
                },
                "mode": {
                  "type": "integer"
                },
                "path": {
                  "maxLength": 65536,
                  "minLength": 1,
                  "type": "string"
                },
                "sha256": {
                  "maxLength": 65536,
                  "minLength": 1,
                  "type": "string"
                }
              },
              "required": [
                "bytes",
                "mode",
                "path",
                "sha256"
              ],
              "type": "object"
            },
            "maxItems": 1024,
            "type": "array"
          },
          "guidance_manifest_sha256": {
            "maxLength": 65536,
            "minLength": 1,
            "type": "string"
          },
          "layout_sha256": {
            "maxLength": 65536,
            "minLength": 1,
            "type": "string"
          },
          "owner_handoff": {
            "maxLength": 65536,
            "minLength": 1,
            "type": "string"
          },
          "policy_sha256": {
            "maxLength": 65536,
            "minLength": 1,
            "type": "string"
          },
          "producer_receipt_sha256": {
            "maxLength": 65536,
            "minLength": 1,
            "type": "string"
          },
          "profile_sha256": {
            "maxLength": 65536,
            "minLength": 1,
            "type": "string"
          },
          "required_global_policy_sha256": {
            "maxLength": 65536,
            "minLength": 1,
            "type": "string"
          },
          "schema": {
            "const": "dual-guidance-summary/v1"
          },
          "schema_version": {
            "const": 1,
            "type": "integer"
          },
          "status": {
            "maxLength": 65536,
            "minLength": 1,
            "type": "string"
          }
        },
        "required": [
          "catalog_sha256",
          "decisions",
          "files",
          "guidance_manifest_sha256",
          "layout_sha256",
          "owner_handoff",
          "policy_sha256",
          "producer_receipt_sha256",
          "profile_sha256",
          "required_global_policy_sha256",
          "schema",
          "schema_version",
          "status"
        ],
        "type": "object"
      },
      "modified_paths": {
        "const": [
          ".claude/omarchy-setup/CLAUDE.md",
          ".codex/AGENTS.md",
          ".config/agents/AGENTS.md"
        ],
        "maxItems": 3,
        "minItems": 3,
        "type": "array"
      },
      "policy_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "producer_receipt_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "request_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "resident_tokens": {
        "maximum": 3000,
        "minimum": 1,
        "type": "integer"
      },
      "schema_version": {
        "const": 4,
        "type": "integer"
      },
      "state": {
        "const": "candidate-inert",
        "type": "string"
      },
      "template_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "tokenizer": {
        "const": "tiktoken-0.12.0-cl100k_base",
        "type": "string"
      },
      "tokenizer_manifest_sha256": {
        "maxLength": 64,
        "minLength": 64,
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      }
    },
    "required": [
      "schema_version",
      "state",
      "request_sha256",
      "catalog_sha256",
      "producer_receipt_sha256",
      "policy_sha256",
      "template_sha256",
      "tokenizer_manifest_sha256",
      "tokenizer",
      "discovery_tokens",
      "resident_tokens",
      "files",
      "modified_paths",
      "guidance"
    ],
    "type": "object"
  },
  "shared_receipt_v5": {
    "additionalProperties": false,
    "properties": {
      "catalog": {
        "additionalProperties": false,
        "properties": {
          "artifact_manifest_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "authorized": {
            "const": "unknown"
          },
          "catalog_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "configured": {
            "const": "unknown"
          },
          "derivation": {
            "const": "upkeeper-tool-overlay-v4"
          },
          "discovery_tokens": {
            "maximum": 1200,
            "minimum": 1,
            "type": "integer"
          },
          "enforcement": {
            "const": "unqualified"
          },
          "input_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "installed": {
            "const": "unknown"
          },
          "platform": {
            "enum": [
              "linux-amd64",
              "linux-arm64"
            ]
          },
          "producer_receipt_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "readiness": {
            "const": "unassessed"
          },
          "resident_tokens": {
            "maximum": 3000,
            "minimum": 1,
            "type": "integer"
          },
          "tokenizer": {
            "const": "tiktoken-0.12.0-cl100k_base"
          },
          "tokenizer_manifest_sha256": {
            "maxLength": 64,
            "minLength": 64,
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          }
        },
        "required": [
          "input_sha256",
          "catalog_sha256",
          "producer_receipt_sha256",
          "artifact_manifest_sha256",
          "platform",
          "derivation",
          "tokenizer",
          "discovery_tokens",
          "resident_tokens",
          "installed",
          "configured",
          "authorized",
          "enforcement",
          "readiness",
          "tokenizer_manifest_sha256"
        ],
        "type": "object"
      },
      "files": {
        "items": {
          "oneOf": [
            {
              "additionalProperties": false,
              "properties": {
                "kind": {
                  "enum": [
                    "instruction",
                    "skill",
                    "policy-input",
                    "catalog-input",
                    "producer-receipt"
                  ]
                },
                "logical_destination": {
                  "maxLength": 512,
                  "minLength": 1,
                  "type": "string"
                },
                "mode": {
                  "enum": [
                    384,
                    420,
                    448,
                    493
                  ],
                  "type": "integer"
                },
                "path": {
                  "maxLength": 512,
                  "minLength": 1,
                  "type": "string"
                },
                "sha256": {
                  "pattern": "^[0-9a-f]{64}$",
                  "type": "string"
                }
              },
              "required": [
                "path",
                "sha256",
                "mode",
                "kind",
                "logical_destination"
              ],
              "type": "object"
            },
            {
              "additionalProperties": false,
              "properties": {
                "kind": {
                  "const": "guidance-input"
                },
                "logical_destination": {
                  "maxLength": 65536,
                  "minLength": 1,
                  "type": "string"
                },
                "mode": {
                  "const": 384,
                  "type": "integer"
                },
                "path": {
                  "enum": [
                    "inputs/guidance-layout.json",
                    "inputs/guidance-observation.json",
                    "inputs/guidance-profile.json",
                    "inputs/required-global-policy.json",
                    "inputs/tool-guidance-manifest.json",
                    "inputs/tool-guidance/fixture-cli.json",
                    "inputs/tool-guidance/fixture-mcp.json",
                    "inputs/tool-guidance/fixture-third-party.json",
                    "inputs/tool-guidance/huyang.json"
                  ]
                },
                "sha256": {
                  "pattern": "^[0-9a-f]{64}$",
                  "type": "string"
                }
              },
              "required": [
                "kind",
                "logical_destination",
                "mode",
                "path",
                "sha256"
              ],
              "type": "object"
            },
            {
              "additionalProperties": false,
              "properties": {
                "kind": {
                  "const": "guidance-metadata"
                },
                "logical_destination": {
                  "maxLength": 65536,
                  "minLength": 1,
                  "type": "string"
                },
                "mode": {
                  "const": 420,
                  "type": "integer"
                },
                "path": {
                  "enum": [
                    "metadata/guidance/fragments/fixture-cli.md",
                    "metadata/guidance/fragments/fixture-mcp.md",
                    "metadata/guidance/fragments/fixture-third-party.md",
                    "metadata/guidance/fragments/huyang.md",
                    "metadata/guidance/menu.json",
                    "metadata/guidance/menu.txt"
                  ]
                },
                "sha256": {
                  "pattern": "^[0-9a-f]{64}$",
                  "type": "string"
                }
              },
              "required": [
                "kind",
                "logical_destination",
                "mode",
                "path",
                "sha256"
              ],
              "type": "object"
            }
          ]
        },
        "maxItems": 48,
        "minItems": 48,
        "type": "array",
        "uniqueItems": true
      },
      "guidance": {
        "additionalProperties": false,
        "properties": {
          "global_policy_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "layout_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "manifest_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "observation_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "packages": {
            "items": {
              "additionalProperties": false,
              "properties": {
                "path": {
                  "maxLength": 4096,
                  "minLength": 1,
                  "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$",
                  "type": "string"
                },
                "sha256": {
                  "pattern": "^[0-9a-f]{64}$",
                  "type": "string"
                },
                "tool_id": {
                  "maxLength": 65536,
                  "minLength": 1,
                  "type": "string"
                }
              },
              "required": [
                "path",
                "sha256",
                "tool_id"
              ],
              "type": "object"
            },
            "maxItems": 1024,
            "type": "array"
          },
          "profile_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "projection": {
            "additionalProperties": false,
            "properties": {
              "catalog_sha256": {
                "maxLength": 65536,
                "minLength": 1,
                "type": "string"
              },
              "decisions": {
                "items": {
                  "additionalProperties": false,
                  "properties": {
                    "decision": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "fragment_sha256": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "guidance_sha256": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "help_sha256": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "rule_ids": {
                      "items": {
                        "maxLength": 65536,
                        "minLength": 1,
                        "type": "string"
                      },
                      "maxItems": 1024,
                      "type": "array"
                    },
                    "source_owner": {
                      "additionalProperties": false,
                      "properties": {
                        "evidence_path": {
                          "maxLength": 65536,
                          "minLength": 1,
                          "type": "string"
                        },
                        "evidence_sha256": {
                          "maxLength": 65536,
                          "minLength": 1,
                          "type": "string"
                        },
                        "kind": {
                          "maxLength": 65536,
                          "minLength": 1,
                          "type": "string"
                        },
                        "owning_path": {
                          "maxLength": 65536,
                          "minLength": 1,
                          "type": "string"
                        },
                        "source_path": {
                          "maxLength": 65536,
                          "minLength": 1,
                          "type": "string"
                        },
                        "source_sha256": {
                          "maxLength": 65536,
                          "minLength": 1,
                          "type": "string"
                        },
                        "status": {
                          "maxLength": 65536,
                          "minLength": 1,
                          "type": "string"
                        }
                      },
                      "required": [
                        "evidence_path",
                        "evidence_sha256",
                        "kind",
                        "owning_path",
                        "source_path",
                        "source_sha256",
                        "status"
                      ],
                      "type": "object"
                    },
                    "tool_id": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    }
                  },
                  "required": [
                    "decision",
                    "fragment_sha256",
                    "guidance_sha256",
                    "help_sha256",
                    "rule_ids",
                    "source_owner",
                    "tool_id"
                  ],
                  "type": "object"
                },
                "maxItems": 1024,
                "type": "array"
              },
              "files": {
                "items": {
                  "additionalProperties": false,
                  "properties": {
                    "bytes": {
                      "type": "integer"
                    },
                    "mode": {
                      "type": "integer"
                    },
                    "path": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    },
                    "sha256": {
                      "maxLength": 65536,
                      "minLength": 1,
                      "type": "string"
                    }
                  },
                  "required": [
                    "bytes",
                    "mode",
                    "path",
                    "sha256"
                  ],
                  "type": "object"
                },
                "maxItems": 1024,
                "type": "array"
              },
              "guidance_manifest_sha256": {
                "maxLength": 65536,
                "minLength": 1,
                "type": "string"
              },
              "layout_sha256": {
                "maxLength": 65536,
                "minLength": 1,
                "type": "string"
              },
              "owner_handoff": {
                "maxLength": 65536,
                "minLength": 1,
                "type": "string"
              },
              "policy_sha256": {
                "maxLength": 65536,
                "minLength": 1,
                "type": "string"
              },
              "producer_receipt_sha256": {
                "maxLength": 65536,
                "minLength": 1,
                "type": "string"
              },
              "profile_sha256": {
                "maxLength": 65536,
                "minLength": 1,
                "type": "string"
              },
              "required_global_policy_sha256": {
                "maxLength": 65536,
                "minLength": 1,
                "type": "string"
              },
              "schema": {
                "const": "dual-guidance-summary/v1"
              },
              "schema_version": {
                "const": 1,
                "type": "integer"
              },
              "status": {
                "maxLength": 65536,
                "minLength": 1,
                "type": "string"
              }
            },
            "required": [
              "catalog_sha256",
              "decisions",
              "files",
              "guidance_manifest_sha256",
              "layout_sha256",
              "owner_handoff",
              "policy_sha256",
              "producer_receipt_sha256",
              "profile_sha256",
              "required_global_policy_sha256",
              "schema",
              "schema_version",
              "status"
            ],
            "type": "object"
          },
          "projection_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "schema_version": {
            "const": 1,
            "type": "integer"
          }
        },
        "required": [
          "global_policy_sha256",
          "layout_sha256",
          "manifest_sha256",
          "observation_sha256",
          "packages",
          "profile_sha256",
          "projection",
          "projection_sha256",
          "schema_version"
        ],
        "type": "object"
      },
      "owner": {
        "const": "upkeeper"
      },
      "owner_descriptor_sha256": {
        "pattern": "^[0-9a-f]{64}$",
        "type": "string"
      },
      "policy": {
        "additionalProperties": false,
        "properties": {
          "canonical_policy_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "configured_catalog_validation": {
            "type": "string"
          },
          "current_authorization": {
            "type": "string"
          },
          "derivation": {
            "type": "string"
          },
          "input_contract": {
            "type": "string"
          },
          "input_manifest_sha256": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "interpretation": {
            "type": "string"
          },
          "policy_format": {
            "type": "string"
          },
          "readiness": {
            "type": "string"
          },
          "upstream_commit": {
            "type": "string"
          },
          "upstream_repository": {
            "type": "string"
          },
          "upstream_tree": {
            "type": "string"
          }
        },
        "required": [
          "input_manifest_sha256",
          "input_contract",
          "canonical_policy_sha256",
          "policy_format",
          "derivation",
          "upstream_repository",
          "upstream_commit",
          "upstream_tree",
          "interpretation",
          "configured_catalog_validation",
          "current_authorization",
          "readiness"
        ],
        "type": "object"
      },
      "schema_version": {
        "const": 5,
        "type": "integer"
      },
      "source_commit": {
        "pattern": "^[0-9a-f]{40}$",
        "type": "string"
      },
      "source_repository": {
        "const": "github.com/iryzhkov/UpKeeper"
      },
      "source_sha256": {
        "additionalProperties": false,
        "properties": {
          "internal/toolproducer/testdata/descriptors/fixture-cli.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "internal/toolproducer/testdata/descriptors/fixture-mcp.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "internal/toolproducer/testdata/descriptors/fixture-third-party.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/bin/agents-instructions-gen": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/bin/agents-role-project": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/CLAUDE.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/skills/huyang/SKILL.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/skills/implementation-session/SKILL.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/skills/implementation-session/references/prompt-template.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/skills/ov-memory-curation/SKILL.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/skills/ov-memory/SKILL.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/skills/publish-agent-tooling/SKILL.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/skills/t3-campaign/SKILL.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/skills/t3-campaign/references/executor-briefs.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/skills/t3-task/SKILL.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/config/claude/skills/t3-wait/SKILL.md": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/agent-instructions/owner.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/catalog-guidance/config/bin/agents-tool-project": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/catalog-guidance/guidance/fixture-cli.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/catalog-guidance/guidance/fixture-mcp.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/catalog-guidance/guidance/fixture-third-party.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/catalog-guidance/guidance/huyang.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/catalog-guidance/guidance/manifest.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/catalog-guidance/guidance/required-global-policy.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/catalog-guidance/owner.json": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          },
          "shared/catalog-guidance/templates/discovery.txt": {
            "pattern": "^[0-9a-f]{64}$",
            "type": "string"
          }
        },
        "required": [
          "internal/toolproducer/testdata/descriptors/fixture-cli.json",
          "internal/toolproducer/testdata/descriptors/fixture-mcp.json",
          "internal/toolproducer/testdata/descriptors/fixture-third-party.json",
          "shared/agent-instructions/config/bin/agents-instructions-gen",
          "shared/agent-instructions/config/bin/agents-role-project",
          "shared/agent-instructions/config/claude/CLAUDE.md",
          "shared/agent-instructions/config/claude/skills/huyang/SKILL.md",
          "shared/agent-instructions/config/claude/skills/implementation-session/SKILL.md",
          "shared/agent-instructions/config/claude/skills/implementation-session/references/prompt-template.md",
          "shared/agent-instructions/config/claude/skills/ov-memory-curation/SKILL.md",
          "shared/agent-instructions/config/claude/skills/ov-memory/SKILL.md",
          "shared/agent-instructions/config/claude/skills/publish-agent-tooling/SKILL.md",
          "shared/agent-instructions/config/claude/skills/t3-campaign/SKILL.md",
          "shared/agent-instructions/config/claude/skills/t3-campaign/references/executor-briefs.md",
          "shared/agent-instructions/config/claude/skills/t3-task/SKILL.md",
          "shared/agent-instructions/config/claude/skills/t3-wait/SKILL.md",
          "shared/agent-instructions/owner.json",
          "shared/catalog-guidance/config/bin/agents-tool-project",
          "shared/catalog-guidance/guidance/fixture-cli.json",
          "shared/catalog-guidance/guidance/fixture-mcp.json",
          "shared/catalog-guidance/guidance/fixture-third-party.json",
          "shared/catalog-guidance/guidance/huyang.json",
          "shared/catalog-guidance/guidance/manifest.json",
          "shared/catalog-guidance/guidance/required-global-policy.json",
          "shared/catalog-guidance/owner.json",
          "shared/catalog-guidance/templates/discovery.txt"
        ],
        "type": "object"
      },
      "state": {
        "const": "candidate-inert",
        "type": "string"
      }
    },
    "required": [
      "schema_version",
      "state",
      "owner",
      "source_repository",
      "source_commit",
      "owner_descriptor_sha256",
      "source_sha256",
      "files",
      "policy",
      "catalog",
      "guidance"
    ],
    "type": "object"
  }
}`
