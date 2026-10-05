package sharedgeneration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// This branch reads retained inert documents only. It does not acquire source,
// approve declared pins, prepare output, install references or grant access.
const catalogTrustLabel = "retained-inert-catalog-integrity; derivation-only; current-authorization-unknown; readiness-unassessed"

func decodeCatalogReceipt(b []byte, r *Receipt) error {
	doc, err := author.ParseCatalogDocumentEncoding(author.CatalogReceipt5, b)
	if err != nil {
		return err
	}
	// Typed use starts only after the owning closed canonical validator succeeds.
	return json.Unmarshal(doc.Encoding(), r)
}
func validateCatalogReceipt(r Receipt) error {
	b, err := canonjson.MarshalFile(r)
	if err != nil {
		return err
	}
	return author.ValidateCatalogDocumentEncoding(author.CatalogReceipt5, b)
}
func catalogPayloadLimit(f File) int64 {
	if f.Path == "inputs/guidance-layout.json" {
		return 131072
	}
	if strings.HasPrefix(f.Path, "inputs/") {
		return metadataLimit
	}
	return 65536
}
func retainedObject(v any) map[string]any { x, _ := v.(map[string]any); return x }
func retainedArray(v any) []any           { x, _ := v.([]any); return x }
func retainedString(v any) string         { x, _ := v.(string); return x }
func retainedEqual(a, b any) bool {
	x, e := canonjson.Compact(a)
	y, e2 := canonjson.Compact(b)
	return e == nil && e2 == nil && bytes.Equal(x, y)
}
func retainedHash(v any) string { b, _ := canonjson.MarshalFile(v); return digest(b) }
func retainedScalars(v any) error {
	switch x := v.(type) {
	case string:
		if len(x) > 65536 || !utf8.ValidString(x) || strings.ContainsRune(x, 0) {
			return errors.New("catalog retained scalar encoding")
		}
	case map[string]any:
		for k, v := range x {
			if err := retainedScalars(k); err != nil {
				return err
			}
			if err := retainedScalars(v); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range x {
			if err := retainedScalars(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// Frozen private schemas come from the hash-verified D1 archive, not from caller
// metadata. They bound retained inputs, including their nested closed objects.
var retainedSchemas = func() map[string]any {
	v, err := canonjson.DecodeObject([]byte(retainedSchemaJSON))
	if err != nil {
		panic(err)
	}
	return v
}()

func retainedShape(v any, s map[string]any) error {
	fail := func() error { return errors.New("catalog retained shape") }
	if c, ok := s["const"]; ok && !retainedEqual(c, v) {
		return fail()
	}
	if a, ok := s["enum"].([]any); ok {
		found := false
		for _, c := range a {
			found = found || retainedEqual(c, v)
		}
		if !found {
			return fail()
		}
	}
	if a, ok := s["oneOf"].([]any); ok {
		n := 0
		for _, c := range a {
			if retainedShape(v, retainedObject(c)) == nil {
				n++
			}
		}
		if n != 1 {
			return fail()
		}
	}
	bound := func(k string, n int64, lower bool) bool {
		x, ok := s[k].(json.Number)
		if !ok {
			return true
		}
		limit, err := x.Int64()
		return err == nil && (lower && n >= limit || !lower && n <= limit)
	}
	switch s["type"] {
	case "object":
		x, ok := v.(map[string]any)
		if !ok {
			return fail()
		}
		props := retainedObject(s["properties"])
		for _, k := range retainedArray(s["required"]) {
			if _, ok := x[retainedString(k)]; !ok {
				return fail()
			}
		}
		for k, c := range x {
			child, ok := props[k]
			if !ok {
				if s["additionalProperties"] == false {
					return fail()
				}
				child = s["additionalProperties"]
			}
			if schema, ok := child.(map[string]any); ok {
				if err := retainedShape(c, schema); err != nil {
					return err
				}
			}
		}
	case "array":
		x, ok := v.([]any)
		if !ok || !bound("minItems", int64(len(x)), true) || !bound("maxItems", int64(len(x)), false) {
			return fail()
		}
		seen := map[string]bool{}
		for _, c := range x {
			if child, ok := s["items"].(map[string]any); ok {
				if err := retainedShape(c, child); err != nil {
					return err
				}
			}
			if s["uniqueItems"] == true {
				b, _ := canonjson.Compact(c)
				if seen[string(b)] {
					return fail()
				}
				seen[string(b)] = true
			}
		}
	case "string":
		x, ok := v.(string)
		if !ok || !bound("minLength", int64(utf8.RuneCountInString(x)), true) || !bound("maxLength", int64(utf8.RuneCountInString(x)), false) {
			return fail()
		}
		if p, ok := s["pattern"].(string); ok {
			re, err := regexp.Compile(p)
			if err != nil {
				// The sole non-RE2 pattern in this frozen schema is the relative
				// path lookahead. An unknown schema pattern must fail closed.
				if p != `^(?!/)(?!.*(?:^|/)\.\.?(?:/|$))(?!.*\\)[^\u0000]+$` || !validPath(x) {
					return fail()
				}
			} else if !re.MatchString(x) {
				return fail()
			}
		}
	case "integer":
		x, ok := v.(json.Number)
		if !ok {
			return fail()
		}
		n, err := strconv.ParseInt(string(x), 10, 64)
		if err != nil || !bound("minimum", n, true) || !bound("maximum", n, false) {
			return fail()
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fail()
		}
	}
	return nil
}
func retainedDecode(b []byte, schema string, limit int) (map[string]any, error) {
	var v map[string]any
	if err := strictJSONLimit(b, &v, true, limit); err != nil {
		return nil, err
	}
	if err := retainedScalars(v); err != nil {
		return nil, err
	}
	if err := retainedShape(v, retainedObject(retainedSchemas[schema])); err != nil {
		return nil, fmt.Errorf("%s: %w", schema, err)
	}
	return v, nil
}

// validateCatalogPayload checks relationships with bytes acquired through the
// common held-descriptor closed tree. A declaration of source or producer
// provenance remains a declaration, even when its retained content hash matches.
func validateCatalogPayload(r Receipt, data map[string][]byte) error {
	fail := func(s string) error { return fmt.Errorf("catalog retained binding: %s", s) }
	docs := map[string]map[string]any{}
	schemas := map[string]string{
		"inputs/tool-guidance-manifest.json": "tool_guidance_manifest_v1",
		// Receipt5 adds C07 grammar bounds to the historical canonical policy
		// check; owning policy semantics still belong to genuine preparation.
		"inputs/route-policy.json":           "",
		"inputs/guidance-profile.json":       "guidance_profile_v1",
		"inputs/guidance-observation.json":   "guidance_observation_v1",
		"inputs/guidance-layout.json":        "layout_v4",
		"inputs/required-global-policy.json": "required_global_policy_v1",
		"inputs/tool-catalog.json":           "catalog_v1",
		"inputs/tool-producer-input.json":    "producer_input_v1",
		"inputs/tool-producer-receipt.json":  "producer_receipt_v1",
		"metadata/guidance/menu.json":        "guidance_menu_v1",
	}
	for _, id := range []string{"fixture-cli", "fixture-mcp", "fixture-third-party", "huyang"} {
		schemas["inputs/tool-guidance/"+id+".json"] = "tool_guidance_v1"
	}
	for path, schema := range schemas {
		limit := metadataLimit
		if path == "inputs/guidance-layout.json" {
			limit = 131072
		}
		if strings.HasPrefix(path, "metadata/") {
			limit = 65536
		}
		v, err := retainedDecode(data[path], schema, limit)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		docs[path] = v
	}
	for _, f := range r.Files {
		if len(data[f.Path]) == 0 && f.Path != "metadata/guidance/fragments/fixture-mcp.md" && f.Path != "metadata/guidance/fragments/fixture-third-party.md" {
			return fail("empty required payload")
		}
	}
	var c, g map[string]any
	if err := json.Unmarshal(r.Catalog, &c); err != nil {
		return err
	}
	// UseNumber preserves sizes for comparisons with canonical retained documents.
	d := json.NewDecoder(bytes.NewReader(r.Guidance))
	d.UseNumber()
	if err := d.Decode(&g); err != nil {
		return err
	}
	projection := retainedObject(g["projection"])
	for _, entry := range retainedArray(projection["files"]) {
		f := retainedObject(entry)
		path := retainedString(f["path"])
		if path == "discovery.txt" {
			continue
		} // audit-only output is not a payload
		if strings.HasPrefix(path, "final-instructions/") {
			path = strings.TrimPrefix(path, "final-instructions/")
		} else {
			path = "metadata/guidance/" + path
		}
		if !retainedEqual(f["bytes"], json.Number(strconv.Itoa(len(data[path])))) {
			return fail("projection byte count " + path)
		}
	}
	manifest := docs["inputs/tool-guidance-manifest.json"]
	profile := docs["inputs/guidance-profile.json"]
	observation := docs["inputs/guidance-observation.json"]
	global := docs["inputs/required-global-policy.json"]
	layout := docs["inputs/guidance-layout.json"]
	catalog := docs["inputs/tool-catalog.json"]
	producer := docs["inputs/tool-producer-receipt.json"]
	input := docs["inputs/tool-producer-input.json"]
	menu := docs["metadata/guidance/menu.json"]
	for k, want := range map[string]any{"catalog_sha256": c["catalog_sha256"], "producer_receipt_sha256": c["producer_receipt_sha256"], "policy_sha256": r.Policy.CanonicalPolicySHA256, "records": g["packages"]} {
		if !retainedEqual(manifest[k], want) {
			return fail("manifest " + k)
		}
	}
	if !retainedEqual(manifest["required_global_policy"], map[string]any{"path": "guidance/required-global-policy.json", "sha256": g["global_policy_sha256"]}) {
		return fail("manifest global")
	}
	if !retainedEqual(profile["evidence"], map[string]any{"path": "inputs/guidance-observation.json", "sha256": g["observation_sha256"]}) {
		return fail("profile observation")
	}
	if !pin(retainedString(observation["source_commit"]), 40) || !pin(retainedString(observation["source_tree"]), 40) {
		return fail("observation identity")
	}
	// Read receipts carry actual source bytes: verify all14 imported +3 descriptor
	// claims, inventories and lengths. No live source lookup or approval follows.
	for name, count := range map[string]int{"source_reads": 14, "descriptor_reads": 3} {
		read := retainedObject(observation[name])
		rd := retainedObject(read["data"])
		files, entries := retainedArray(rd["files"]), retainedArray(rd["entries"])
		if read["outcome"] != "ok" || len(files) != count || len(entries) != count {
			return fail("observation closure")
		}
		seen := map[string]bool{}
		for i, e := range files {
			f := retainedObject(e)
			path := retainedString(f["path"])
			content := retainedString(f["content"])
			entry := retainedObject(entries[i])
			allowed := strings.HasPrefix(path, "shared/agent-instructions/")
			if name == "descriptor_reads" {
				allowed = strings.HasPrefix(path, "internal/toolproducer/testdata/descriptors/")
			}
			if !allowed || seen[path] || !validPath(path) || r.SourceSHA256[path] != digest([]byte(content)) || entry["path"] != path ||
				!retainedEqual(entry["bytes"], json.Number(strconv.Itoa(len(content)))) || !retainedEqual(entry["lines"], f["lines"]) ||
				!retainedEqual(f["lines"], json.Number(strconv.Itoa(strings.Count(content, "\n")))) {
				return fail("observation content " + path)
			}
			seen[path] = true
		}
	}
	if producer["input_sha256"] != c["input_sha256"] || producer["catalog_sha256"] != c["catalog_sha256"] ||
		producer["platform"] != c["platform"] || input["platform"] != c["platform"] ||
		producer["artifact_manifest_sha256"] != c["artifact_manifest_sha256"] || input["artifact_manifest_sha256"] != c["artifact_manifest_sha256"] ||
		!retainedEqual(input["descriptors"], producer["descriptors"]) {
		return fail("producer/input/catalog")
	}
	// Artifact/source declarations are retained rather than reacquired. Match the
	// catalog projection of the producer's contained records exactly.
	artifacts := []any{}
	for _, a := range retainedArray(producer["artifacts"]) {
		f := retainedObject(a)
		projected := map[string]any{}
		for _, k := range []string{"id", "source_id", "path", "sha256", "mode", "required"} {
			projected[k] = f[k]
		}
		artifacts = append(artifacts, projected)
	}
	if !retainedEqual(artifacts, catalog["artifacts"]) {
		return fail("catalog artifacts")
	}
	sources := []any{}
	for _, a := range retainedArray(producer["sources"]) {
		f := retainedObject(a)
		sources = append(sources, map[string]any{"id": f["id"], "repository": f["repository"], "revision": f["commit"], "revision_kind": "git-sha1"})
	}
	if !retainedEqual(sources, catalog["sources"]) {
		return fail("catalog sources")
	}
	for _, a := range retainedArray(input["descriptors"]) {
		f := retainedObject(a)
		id := retainedString(f["id"])
		if id != "huyang" && f["sha256"] != r.SourceSHA256["internal/toolproducer/testdata/descriptors/"+id+".json"] {
			return fail("descriptor source")
		}
	}
	// Check contained source-file relationships without implying that the
	// declared external acquisition or producer build was approved.
	sourceFiles := map[string]map[string]any{}
	for _, e := range retainedArray(producer["sources"]) {
		s := retainedObject(e)
		id := retainedString(s["id"])
		if _, exists := sourceFiles[id]; exists {
			return fail("duplicate producer source")
		}
		files := map[string]any{}
		for _, e := range retainedArray(s["files"]) {
			f := retainedObject(e)
			path := retainedString(f["path"])
			if !validPath(path) || files[path] != nil {
				return fail("producer source file")
			}
			files[path] = f["sha256"]
		}
		sourceFiles[id] = files
	}
	for _, name := range []string{"descriptors", "artifacts"} {
		seen := map[string]bool{}
		for _, e := range retainedArray(producer[name]) {
			f := retainedObject(e)
			id := retainedString(f["id"])
			if seen[id] || sourceFiles[retainedString(f["source_id"])][retainedString(f["source_path"])] != f["sha256"] {
				return fail("producer contained " + name)
			}
			seen[id] = true
		}
	}
	bindings := retainedObject(layout["bindings"])
	for k, want := range map[string]any{"guidance_manifest_sha256": g["manifest_sha256"], "required_global_policy_sha256": g["global_policy_sha256"],
		"policy_sha256": r.Policy.CanonicalPolicySHA256, "import_commit": r.Policy.UpstreamCommit, "import_tree": r.Policy.UpstreamTree, "import_repository": r.Policy.UpstreamRepository,
		"imported_owner_sha256": r.SourceSHA256["shared/agent-instructions/owner.json"]} {
		if !retainedEqual(bindings[k], want) {
			return fail("layout " + k)
		}
	}
	for path, hash := range retainedObject(bindings["imported_source_sha256"]) {
		if hash != r.SourceSHA256["shared/agent-instructions/"+path] {
			return fail("layout source " + path)
		}
	}
	provenance := retainedObject(global["provenance"])
	for _, prefix := range []string{"", "claude_"} {
		path := retainedString(provenance[prefix+"source_path"])
		if r.SourceSHA256[path] != provenance[prefix+"source_sha256"] {
			return fail("global policy source")
		}
	}
	if !retainedEqual(provenance["baseline_main_sha256"], bindings["baseline_main_sha256"]) {
		return fail("global/layout baseline")
	}
	if retainedObject(global["deferred_owner_seam"])["status"] != projection["owner_handoff"] {
		return fail("required owner handoff")
	}
	reference := []byte(retainedString(global["operational_reference"]))
	if !bytes.Equal(reference, data[".config/agents/operational-guidance.md"]) || bindings["operational_reference_sha256"] != digest(reference) {
		return fail("operational reference")
	}
	for k, want := range map[string]any{"catalog_sha256": c["catalog_sha256"], "guidance_manifest_sha256": g["manifest_sha256"], "profile_sha256": g["profile_sha256"], "status": projection["status"], "profile_scope": "fixture-only"} {
		if !retainedEqual(menu[k], want) {
			return fail("menu " + k)
		}
	}
	decisions, tools, records, rows := retainedArray(projection["decisions"]), retainedArray(profile["tools"]), retainedArray(catalog["tools"]), retainedArray(menu["tools"])
	ids := []any{}
	blockers := []any{}
	if len(tools) != 4 || len(records) != 4 || len(rows) != 4 {
		return fail("tool closure")
	}
	for i, e := range decisions {
		decision := retainedObject(e)
		id := retainedString(decision["tool_id"])
		ids = append(ids, id)
		tool, record, row := retainedObject(tools[i]), retainedObject(records[i]), retainedObject(rows[i])
		pack := docs["inputs/tool-guidance/"+id+".json"]
		if tool["tool_id"] != id || record["id"] != id || pack["tool_id"] != id || row["tool_id"] != id {
			return fail("tool order")
		}
		if !retainedEqual(pack["owner"], decision["source_owner"]) || !retainedEqual(pack["capability_ids"], record["capabilities"]) {
			return fail("package owner/capabilities")
		}
		ruleIDs := []any{}
		for _, rule := range retainedArray(pack["rules"]) {
			ruleIDs = append(ruleIDs, retainedObject(rule)["id"])
		}
		if !retainedEqual(ruleIDs, decision["rule_ids"]) {
			return fail("package rules")
		}
		for _, k := range []string{"owner", "purpose", "rules", "help", "workflow"} {
			if !retainedEqual(row[k], pack[k]) {
				return fail("menu package " + k)
			}
		}
		for _, k := range []string{"availability", "visibility", "authorized", "enforced"} {
			if !retainedEqual(row[k], tool[k]) {
				return fail("menu profile " + k)
			}
		}
		for _, k := range []string{"decision", "fragment_sha256", "guidance_sha256"} {
			if !retainedEqual(row[k], decision[k]) {
				return fail("menu decision " + k)
			}
		}
		if !retainedEqual(row["catalog_record"], record) {
			return fail("menu catalog record")
		}
		required, _ := record["required"].(bool)
		wantDecision := "visible-observed"
		if tool["visibility"] == "hidden" {
			wantDecision = "hidden"
		} else if tool["availability"] == "unavailable" {
			wantDecision = "unavailable"
		} else if tool["availability"] == "unknown" {
			wantDecision = "declaration-only"
		}
		if required && wantDecision != "visible-observed" {
			wantDecision = "required-interface-blocked"
		}
		if wantDecision == "required-interface-blocked" {
			blockers = append(blockers, id)
		}
		if decision["decision"] != wantDecision {
			return fail("profile decision")
		}
		help := retainedObject(pack["help"])
		var detail []byte
		if help["kind"] == "pinned-reference" {
			path := retainedString(help["path"])
			if path != ".config/agents/huyang.md" || help["sha256"] != digest(data[path]) {
				return fail("help reference")
			}
			detail = data[path]
		} else {
			if help["path"] != "inputs/tool-catalog.json" || help["tool_id"] != id || help["sha256"] != c["catalog_sha256"] {
				return fail("help catalog record")
			}
			detail, _ = canonjson.MarshalFile(record)
		}
		if digest(detail) != decision["help_sha256"] || row["detail"] != string(detail) {
			return fail("help detail")
		}
	}
	if !retainedEqual(menu["blockers"], blockers) {
		return fail("menu blockers")
	}
	if provenance["reference_sha256"] != digest(reference) {
		return fail("global reference pin")
	}
	if !retainedEqual(input["tool_ids"], ids) {
		return fail("producer tool IDs")
	}
	return nil
}

const retainedSchemaJSON = `{"catalog_v1": {"additionalProperties": false, "properties": {"artifacts": {"items": {"additionalProperties": false, "properties": {"id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "mode": {"maximum": 511, "minimum": 0, "type": "integer"}, "path": {"maxLength": 512, "minLength": 1, "type": "string"}, "required": {"type": "boolean"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "source_id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}}, "required": ["id", "source_id", "path", "sha256", "mode", "required"], "type": "object"}, "maxItems": 1024, "minItems": 0, "type": "array", "uniqueItems": true}, "schema_version": {"const": 1, "type": "integer"}, "sources": {"items": {"additionalProperties": false, "properties": {"id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "repository": {"type": "string"}, "revision": {"type": "string"}, "revision_kind": {"enum": ["git-sha1", "content-sha256"]}}, "required": ["id", "repository", "revision_kind", "revision"], "type": "object"}, "maxItems": 128, "minItems": 0, "type": "array", "uniqueItems": true}, "state": {"const": "candidate-inert", "type": "string"}, "tools": {"items": {"additionalProperties": false, "properties": {"aliases": {"items": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "maxItems": 4, "minItems": 0, "type": "array", "uniqueItems": true}, "artifact_ids": {"items": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "maxItems": 8, "minItems": 1, "type": "array", "uniqueItems": true}, "canonical_tool": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "capabilities": {"items": {"enum": ["repository-operations", "file-read", "file-search", "transactional-edit", "fixture-inspect", "fixture-metadata", "fixture-reference"]}, "maxItems": 8, "minItems": 1, "type": "array", "uniqueItems": true}, "id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "kind": {"enum": ["cli", "mcp-only", "third-party"]}, "platforms": {"items": {"enum": ["linux-amd64", "linux-arm64"]}, "maxItems": 2, "minItems": 1, "type": "array", "uniqueItems": true}, "primary_route": {"additionalProperties": false, "properties": {"kind": {"enum": ["cli", "mcp", "external"]}, "name": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}}, "required": ["kind", "name"], "type": "object"}, "purpose": {"maxLength": 160, "minLength": 1, "type": "string"}, "required": {"type": "boolean"}}, "required": ["id", "canonical_tool", "aliases", "kind", "purpose", "capabilities", "primary_route", "artifact_ids", "required", "platforms"], "type": "object"}, "maxItems": 256, "minItems": 0, "type": "array", "uniqueItems": true}}, "required": ["schema_version", "state", "sources", "artifacts", "tools"], "type": "object"}, "guidance_menu_v1": {"additionalProperties": false, "properties": {"blockers": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "catalog_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "guidance_manifest_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "profile_scope": {"maxLength": 65536, "minLength": 1, "type": "string"}, "profile_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "schema": {"const": "guidance-menu/v1"}, "schema_version": {"const": 1, "type": "integer"}, "state": {"maxLength": 65536, "minLength": 1, "type": "string"}, "status": {"maxLength": 65536, "minLength": 1, "type": "string"}, "tools": {"items": {"additionalProperties": false, "properties": {"authorized": {"maxLength": 65536, "minLength": 1, "type": "string"}, "availability": {"maxLength": 65536, "minLength": 1, "type": "string"}, "catalog_record": {"additionalProperties": false, "properties": {"aliases": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "artifact_ids": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "canonical_tool": {"maxLength": 65536, "minLength": 1, "type": "string"}, "capabilities": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "id": {"maxLength": 65536, "minLength": 1, "type": "string"}, "kind": {"maxLength": 65536, "minLength": 1, "type": "string"}, "platforms": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "primary_route": {"additionalProperties": false, "properties": {"kind": {"maxLength": 65536, "minLength": 1, "type": "string"}, "name": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["kind", "name"], "type": "object"}, "purpose": {"maxLength": 65536, "minLength": 1, "type": "string"}, "required": {"type": "boolean"}}, "required": ["aliases", "artifact_ids", "canonical_tool", "capabilities", "id", "kind", "platforms", "primary_route", "purpose", "required"], "type": "object"}, "decision": {"maxLength": 65536, "minLength": 1, "type": "string"}, "detail": {"maxLength": 65536, "minLength": 1, "type": "string"}, "enforced": {"maxLength": 65536, "minLength": 1, "type": "string"}, "fragment_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "guidance_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "help": {"oneOf": [{"additionalProperties": false, "properties": {"display_path": {"const": "~/.config/agents/huyang.md"}, "kind": {"const": "pinned-reference"}, "path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}}, "required": ["display_path", "kind", "path", "sha256"], "type": "object"}, {"additionalProperties": false, "properties": {"kind": {"const": "catalog-record"}, "path": {"const": "inputs/tool-catalog.json"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "tool_id": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["kind", "path", "sha256", "tool_id"], "type": "object"}]}, "owner": {"additionalProperties": false, "properties": {"evidence_path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "evidence_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "kind": {"enum": ["catalog-fixture", "imported-guidance-proposal"]}, "owning_path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "source_path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "source_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "status": {"const": "hypothetical-new-owner-input-unapproved"}}, "required": ["evidence_path", "evidence_sha256", "kind", "owning_path", "source_path", "source_sha256", "status"], "type": "object"}, "purpose": {"maxLength": 65536, "minLength": 1, "type": "string"}, "rules": {"items": {"additionalProperties": false, "properties": {"evidence_quote_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "id": {"pattern": "^[a-z][a-z0-9-]*\\.[a-z-]+\\.v1$", "type": "string"}, "placement": {"const": "before-tool-choice"}, "text": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["evidence_quote_sha256", "id", "placement", "text"], "type": "object"}, "maxItems": 1024, "type": "array"}, "tool_id": {"maxLength": 65536, "minLength": 1, "type": "string"}, "visibility": {"maxLength": 65536, "minLength": 1, "type": "string"}, "workflow": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}}, "required": ["authorized", "availability", "catalog_record", "decision", "detail", "enforced", "fragment_sha256", "guidance_sha256", "help", "owner", "purpose", "rules", "tool_id", "visibility", "workflow"], "type": "object"}, "maxItems": 1024, "type": "array"}}, "required": ["blockers", "catalog_sha256", "guidance_manifest_sha256", "profile_scope", "profile_sha256", "schema", "schema_version", "state", "status", "tools"], "type": "object"}, "guidance_observation_v1": {"additionalProperties": false, "properties": {"descriptor_reads": {"additionalProperties": false, "properties": {"api_version": {"maxLength": 65536, "minLength": 1, "type": "string"}, "data": {"additionalProperties": false, "properties": {"entries": {"items": {"additionalProperties": false, "properties": {"bytes": {"type": "integer"}, "lines": {"type": "integer"}, "path": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["bytes", "lines", "path"], "type": "object"}, "maxItems": 1024, "type": "array"}, "files": {"items": {"additionalProperties": false, "properties": {"content": {"maxLength": 65536, "minLength": 1, "type": "string"}, "lines": {"type": "integer"}, "path": {"maxLength": 65536, "minLength": 1, "type": "string"}, "revision_id": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["content", "lines", "path", "revision_id"], "type": "object"}, "maxItems": 1024, "type": "array"}}, "required": ["entries", "files"], "type": "object"}, "next": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "outcome": {"maxLength": 65536, "minLength": 1, "type": "string"}, "request_id": {"maxLength": 65536, "minLength": 1, "type": "string"}, "summary": {"maxLength": 65536, "minLength": 1, "type": "string"}, "warnings": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "workspace": {"additionalProperties": false, "properties": {"id": {"maxLength": 65536, "minLength": 1, "type": "string"}, "revision": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["id", "revision"], "type": "object"}}, "required": ["api_version", "data", "next", "outcome", "request_id", "summary", "warnings", "workspace"], "type": "object"}, "schema": {"const": "guidance-observation/v1"}, "schema_version": {"const": 1, "type": "integer"}, "source_commit": {"maxLength": 65536, "minLength": 1, "type": "string"}, "source_reads": {"additionalProperties": false, "properties": {"api_version": {"maxLength": 65536, "minLength": 1, "type": "string"}, "data": {"additionalProperties": false, "properties": {"entries": {"items": {"additionalProperties": false, "properties": {"bytes": {"type": "integer"}, "lines": {"type": "integer"}, "path": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["bytes", "lines", "path"], "type": "object"}, "maxItems": 1024, "type": "array"}, "files": {"items": {"additionalProperties": false, "properties": {"content": {"maxLength": 65536, "minLength": 1, "type": "string"}, "lines": {"type": "integer"}, "path": {"maxLength": 65536, "minLength": 1, "type": "string"}, "revision_id": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["content", "lines", "path", "revision_id"], "type": "object"}, "maxItems": 1024, "type": "array"}}, "required": ["entries", "files"], "type": "object"}, "next": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "outcome": {"maxLength": 65536, "minLength": 1, "type": "string"}, "request_id": {"maxLength": 65536, "minLength": 1, "type": "string"}, "summary": {"maxLength": 65536, "minLength": 1, "type": "string"}, "warnings": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "workspace": {"additionalProperties": false, "properties": {"id": {"maxLength": 65536, "minLength": 1, "type": "string"}, "revision": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["id", "revision"], "type": "object"}}, "required": ["api_version", "data", "next", "outcome", "request_id", "summary", "warnings", "workspace"], "type": "object"}, "source_tree": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["descriptor_reads", "schema", "schema_version", "source_commit", "source_reads", "source_tree"], "type": "object"}, "guidance_profile_v1": {"additionalProperties": false, "properties": {"evidence": {"additionalProperties": false, "properties": {"path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}}, "required": ["path", "sha256"], "type": "object"}, "schema": {"const": "guidance-profile/v1"}, "schema_version": {"const": 1, "type": "integer"}, "scope": {"const": "fixture-only"}, "snapshot_id": {"maxLength": 65536, "minLength": 1, "type": "string"}, "tools": {"items": {"additionalProperties": false, "properties": {"authorized": {"enum": ["unknown", "denied", "declaration-only"]}, "availability": {"enum": ["observed", "unavailable", "unknown"]}, "enforced": {"const": "unqualified"}, "tool_id": {"maxLength": 65536, "minLength": 1, "type": "string"}, "visibility": {"enum": ["visible", "hidden"]}}, "required": ["authorized", "availability", "enforced", "tool_id", "visibility"], "type": "object"}, "maxItems": 1024, "type": "array"}}, "required": ["evidence", "schema", "schema_version", "scope", "snapshot_id", "tools"], "type": "object"}, "layout_v4": {"additionalProperties": false, "properties": {"bindings": {"additionalProperties": false, "properties": {"baseline_main_sha256": {"additionalProperties": false, "properties": {".claude/omarchy-setup/CLAUDE.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, ".codex/AGENTS.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, ".config/agents/AGENTS.md": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": [".claude/omarchy-setup/CLAUDE.md", ".codex/AGENTS.md", ".config/agents/AGENTS.md"], "type": "object"}, "guidance_manifest_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "import_commit": {"maxLength": 65536, "minLength": 1, "type": "string"}, "import_repository": {"maxLength": 65536, "minLength": 1, "type": "string"}, "import_tree": {"maxLength": 65536, "minLength": 1, "type": "string"}, "imported_owner_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "imported_source_sha256": {"additionalProperties": false, "properties": {"config/bin/agents-instructions-gen": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/bin/agents-role-project": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/CLAUDE.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/skills/huyang/SKILL.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/skills/implementation-session/SKILL.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/skills/implementation-session/references/prompt-template.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/skills/ov-memory-curation/SKILL.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/skills/ov-memory/SKILL.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/skills/publish-agent-tooling/SKILL.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/skills/t3-campaign/SKILL.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/skills/t3-campaign/references/executor-briefs.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/skills/t3-task/SKILL.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, "config/claude/skills/t3-wait/SKILL.md": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["config/bin/agents-instructions-gen", "config/bin/agents-role-project", "config/claude/CLAUDE.md", "config/claude/skills/huyang/SKILL.md", "config/claude/skills/implementation-session/SKILL.md", "config/claude/skills/implementation-session/references/prompt-template.md", "config/claude/skills/ov-memory-curation/SKILL.md", "config/claude/skills/ov-memory/SKILL.md", "config/claude/skills/publish-agent-tooling/SKILL.md", "config/claude/skills/t3-campaign/SKILL.md", "config/claude/skills/t3-campaign/references/executor-briefs.md", "config/claude/skills/t3-task/SKILL.md", "config/claude/skills/t3-wait/SKILL.md"], "type": "object"}, "model_block_sha256": {"additionalProperties": false, "properties": {"claude": {"maxLength": 65536, "minLength": 1, "type": "string"}, "codex": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["claude", "codex"], "type": "object"}, "operational_reference_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "policy_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "required_global_policy_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["baseline_main_sha256", "guidance_manifest_sha256", "import_commit", "import_repository", "import_tree", "imported_owner_sha256", "imported_source_sha256", "model_block_sha256", "operational_reference_sha256", "policy_sha256", "required_global_policy_sha256"], "type": "object"}, "schema_version": {"const": 4, "type": "integer"}, "texts": {"additionalProperties": false, "properties": {"claude": {"maxLength": 65536, "minLength": 1, "type": "string"}, "codex": {"maxLength": 65536, "minLength": 1, "type": "string"}, "discovery": {"maxLength": 65536, "minLength": 1, "type": "string"}, "shared": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["claude", "codex", "discovery", "shared"], "type": "object"}}, "required": ["bindings", "schema_version", "texts"], "type": "object"}, "producer_input_v1": {"additionalProperties": false, "properties": {"artifact_manifest_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "descriptors": {"items": {"additionalProperties": false, "properties": {"id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "path": {"maxLength": 512, "minLength": 1, "type": "string"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "source_id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "source_path": {"maxLength": 512, "minLength": 1, "type": "string"}}, "required": ["id", "path", "sha256", "source_id", "source_path"], "type": "object"}, "maxItems": 4, "minItems": 1, "type": "array", "uniqueItems": true}, "platform": {"enum": ["linux-amd64", "linux-arm64"]}, "schema_version": {"const": 1, "type": "integer"}, "state": {"const": "candidate-inert", "type": "string"}, "tool_ids": {"items": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "maxItems": 4, "minItems": 1, "type": "array", "uniqueItems": true}}, "required": ["schema_version", "state", "platform", "artifact_manifest_sha256", "descriptors", "tool_ids"], "type": "object"}, "producer_receipt_v1": {"additionalProperties": false, "properties": {"artifact_manifest_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "artifacts": {"items": {"additionalProperties": false, "properties": {"id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "mode": {"enum": [420, 493], "type": "integer"}, "path": {"maxLength": 512, "minLength": 1, "type": "string"}, "required": {"type": "boolean"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "source_id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "source_path": {"maxLength": 512, "minLength": 1, "type": "string"}, "transform": {"const": "identity-v1"}}, "required": ["id", "source_id", "source_path", "path", "sha256", "mode", "required", "transform"], "type": "object"}, "maxItems": 32, "minItems": 1, "type": "array", "uniqueItems": true}, "catalog_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "decisions": {"items": {"additionalProperties": false, "properties": {"result": {"enum": ["verified", "optional-missing", "optional-incompatible"]}, "tool_id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}}, "required": ["tool_id", "result"], "type": "object"}, "maxItems": 4, "minItems": 1, "type": "array", "uniqueItems": true}, "descriptors": {"items": {"additionalProperties": false, "properties": {"id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "path": {"maxLength": 512, "minLength": 1, "type": "string"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "source_id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "source_path": {"maxLength": 512, "minLength": 1, "type": "string"}}, "required": ["id", "path", "sha256", "source_id", "source_path"], "type": "object"}, "maxItems": 4, "minItems": 1, "type": "array", "uniqueItems": true}, "input_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "platform": {"enum": ["linux-amd64", "linux-arm64"]}, "producer_source": {"additionalProperties": false, "properties": {"commit": {"pattern": "^[0-9a-f]{40}$", "type": "string"}, "files": {"items": {"additionalProperties": false, "properties": {"mode": {"enum": [420, 493], "type": "integer"}, "path": {"maxLength": 512, "minLength": 1, "type": "string"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}}, "required": ["path", "sha256", "mode"], "type": "object"}, "maxItems": 32, "minItems": 1, "type": "array", "uniqueItems": true}, "repository": {"const": "https://github.com/iryzhkov/UpKeeper"}, "tree": {"pattern": "^[0-9a-f]{40}$", "type": "string"}}, "required": ["repository", "commit", "tree", "files"], "type": "object"}, "schema_version": {"const": 1, "type": "integer"}, "sources": {"items": {"additionalProperties": false, "properties": {"commit": {"pattern": "^[0-9a-f]{40}$", "type": "string"}, "files": {"items": {"additionalProperties": false, "properties": {"mode": {"enum": [420, 493], "type": "integer"}, "path": {"maxLength": 512, "minLength": 1, "type": "string"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}}, "required": ["path", "sha256", "mode"], "type": "object"}, "maxItems": 32, "minItems": 1, "type": "array", "uniqueItems": true}, "id": {"pattern": "^[a-z][a-z0-9-]{0,63}$", "type": "string"}, "repository": {"pattern": "^https://[^/?#@]+/[^?#]+$", "type": "string"}, "tree": {"pattern": "^[0-9a-f]{40}$", "type": "string"}}, "required": ["id", "repository", "commit", "tree", "files"], "type": "object"}, "maxItems": 8, "minItems": 1, "type": "array", "uniqueItems": true}, "state": {"const": "candidate-inert", "type": "string"}}, "required": ["schema_version", "state", "producer_source", "input_sha256", "artifact_manifest_sha256", "descriptors", "sources", "artifacts", "catalog_sha256", "platform", "decisions"], "type": "object"}, "required_global_policy_v1": {"additionalProperties": false, "properties": {"deferred_owner_seam": {"additionalProperties": false, "properties": {"catalog_membership": {"maxLength": 65536, "minLength": 1, "type": "string"}, "current_bound_owner": {"maxLength": 65536, "minLength": 1, "type": "string"}, "handoff": {"maxLength": 65536, "minLength": 1, "type": "string"}, "required_interface": {"maxLength": 65536, "minLength": 1, "type": "string"}, "required_rules": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "resident_quotes": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "schema": {"maxLength": 65536, "minLength": 1, "type": "string"}, "status": {"maxLength": 65536, "minLength": 1, "type": "string"}, "tool_id": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["catalog_membership", "current_bound_owner", "handoff", "required_interface", "required_rules", "resident_quotes", "schema", "status", "tool_id"], "type": "object"}, "operational_reference": {"maxLength": 65536, "minLength": 1, "type": "string"}, "provenance": {"additionalProperties": false, "properties": {"baseline_main_sha256": {"additionalProperties": false, "properties": {".claude/omarchy-setup/CLAUDE.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, ".codex/AGENTS.md": {"maxLength": 65536, "minLength": 1, "type": "string"}, ".config/agents/AGENTS.md": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": [".claude/omarchy-setup/CLAUDE.md", ".codex/AGENTS.md", ".config/agents/AGENTS.md"], "type": "object"}, "claude_source_path": {"maxLength": 65536, "minLength": 1, "type": "string"}, "claude_source_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "prior_template_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "reference_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}, "source_path": {"maxLength": 65536, "minLength": 1, "type": "string"}, "source_sha256": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["baseline_main_sha256", "claude_source_path", "claude_source_sha256", "prior_template_sha256", "reference_sha256", "source_path", "source_sha256"], "type": "object"}, "schema": {"const": "required-global-policy/v1"}, "schema_version": {"const": 1, "type": "integer"}, "texts": {"additionalProperties": false, "properties": {"claude": {"maxLength": 65536, "minLength": 1, "type": "string"}, "shared": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["claude", "shared"], "type": "object"}}, "required": ["deferred_owner_seam", "operational_reference", "provenance", "schema", "schema_version", "texts"], "type": "object"}, "tool_guidance_manifest_v1": {"additionalProperties": false, "properties": {"catalog_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "policy_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "producer_receipt_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "records": {"items": {"additionalProperties": false, "properties": {"path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "tool_id": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["path", "sha256", "tool_id"], "type": "object"}, "maxItems": 1024, "type": "array"}, "required_global_policy": {"additionalProperties": false, "properties": {"path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}}, "required": ["path", "sha256"], "type": "object"}, "schema": {"const": "tool-guidance-manifest/v1"}, "schema_version": {"const": 1, "type": "integer"}}, "required": ["catalog_sha256", "policy_sha256", "producer_receipt_sha256", "records", "required_global_policy", "schema", "schema_version"], "type": "object"}, "tool_guidance_v1": {"additionalProperties": false, "properties": {"capability_ids": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}, "help": {"oneOf": [{"additionalProperties": false, "properties": {"display_path": {"const": "~/.config/agents/huyang.md"}, "kind": {"const": "pinned-reference"}, "path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}}, "required": ["display_path", "kind", "path", "sha256"], "type": "object"}, {"additionalProperties": false, "properties": {"kind": {"const": "catalog-record"}, "path": {"const": "inputs/tool-catalog.json"}, "sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "tool_id": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["kind", "path", "sha256", "tool_id"], "type": "object"}]}, "owner": {"additionalProperties": false, "properties": {"evidence_path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "evidence_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "kind": {"enum": ["catalog-fixture", "imported-guidance-proposal"]}, "owning_path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "source_path": {"maxLength": 4096, "minLength": 1, "pattern": "^(?!/)(?!.*(?:^|/)\\.\\.?(?:/|$))(?!.*\\\\)[^\\u0000]+$", "type": "string"}, "source_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "status": {"const": "hypothetical-new-owner-input-unapproved"}}, "required": ["evidence_path", "evidence_sha256", "kind", "owning_path", "source_path", "source_sha256", "status"], "type": "object"}, "purpose": {"maxLength": 65536, "minLength": 1, "type": "string"}, "rules": {"items": {"additionalProperties": false, "properties": {"evidence_quote_sha256": {"pattern": "^[0-9a-f]{64}$", "type": "string"}, "id": {"pattern": "^[a-z][a-z0-9-]*\\.[a-z-]+\\.v1$", "type": "string"}, "placement": {"const": "before-tool-choice"}, "text": {"maxLength": 65536, "minLength": 1, "type": "string"}}, "required": ["evidence_quote_sha256", "id", "placement", "text"], "type": "object"}, "maxItems": 1024, "type": "array"}, "schema": {"const": "tool-guidance/v1"}, "schema_version": {"const": 1, "type": "integer"}, "tool_id": {"maxLength": 65536, "minLength": 1, "type": "string"}, "workflow": {"items": {"maxLength": 65536, "minLength": 1, "type": "string"}, "maxItems": 1024, "type": "array"}}, "required": ["capability_ids", "help", "owner", "purpose", "rules", "schema", "schema_version", "tool_id", "workflow"], "type": "object"}}`
