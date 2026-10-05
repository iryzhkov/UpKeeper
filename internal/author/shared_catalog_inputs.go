package author

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// CatalogGuidanceInputKind selects only the frozen inert input branches.
type CatalogGuidanceInputKind string

const (
	CatalogGuidanceManifest     CatalogGuidanceInputKind = "manifest"
	CatalogGuidancePackage      CatalogGuidanceInputKind = "package"
	CatalogGuidanceGlobalPolicy CatalogGuidanceInputKind = "global-policy"
	CatalogGuidanceLayout       CatalogGuidanceInputKind = "layout"
	CatalogGuidanceProfile      CatalogGuidanceInputKind = "profile"
	CatalogGuidanceObservation  CatalogGuidanceInputKind = "observation"
)

// CatalogGuidanceInput is validated encoding, never permission or preparation.
type CatalogGuidanceInput struct {
	kind     CatalogGuidanceInputKind
	encoding []byte
}

func (v CatalogGuidanceInput) Kind() CatalogGuidanceInputKind { return v.kind }
func (v CatalogGuidanceInput) Encoding() []byte               { return bytes.Clone(v.encoding) }

func ValidateCatalogGuidanceInputEncoding(kind CatalogGuidanceInputKind, raw []byte) error {
	_, err := ParseCatalogGuidanceInputEncoding(kind, raw)
	return err
}
func ParseCatalogGuidanceInputEncoding(kind CatalogGuidanceInputKind, raw []byte) (CatalogGuidanceInput, error) {
	name := ""
	limit := 262144
	switch kind {
	case CatalogGuidanceManifest:
		name = "tool_guidance_manifest_v1"
	case CatalogGuidancePackage:
		name = "tool_guidance_v1"
	case CatalogGuidanceGlobalPolicy:
		name = "required_global_policy_v1"
	case CatalogGuidanceLayout:
		name = "layout_v4"
		limit = 131072
	case CatalogGuidanceProfile:
		name = "guidance_profile_v1"
	case CatalogGuidanceObservation:
		name = "guidance_observation_v1"
	default:
		return CatalogGuidanceInput{}, errors.New("unknown catalog guidance input kind")
	}
	if len(raw) == 0 || len(raw) > limit {
		return CatalogGuidanceInput{}, errors.New("catalog guidance input byte bound")
	}
	if err := snapshotCanonical(raw); err != nil {
		return CatalogGuidanceInput{}, err
	}
	v, err := canonjson.DecodeObject(raw)
	if err != nil {
		return CatalogGuidanceInput{}, err
	}
	if err = catalogShape(v, catalogObject(guidanceSchemas[name]), name); err != nil {
		return CatalogGuidanceInput{}, err
	}
	if err = guidanceSemantics(kind, v); err != nil {
		return CatalogGuidanceInput{}, err
	}
	return CatalogGuidanceInput{kind, bytes.Clone(raw)}, nil
}

func guidanceRequire(ok bool, what string) error {
	if !ok {
		return fmt.Errorf("catalog guidance: %s", what)
	}
	return nil
}
func guidanceIDs(rows []any, key string) bool {
	if len(rows) != len(catalogToolIDs) {
		return false
	}
	for i, row := range rows {
		if catalogObject(row)[key] != catalogToolIDs[i] {
			return false
		}
	}
	return true
}
func guidanceSemantics(kind CatalogGuidanceInputKind, v map[string]any) error {
	switch kind {
	case CatalogGuidanceManifest:
		rows := catalogArray(v["records"])
		if err := guidanceRequire(guidanceIDs(rows, "tool_id"), "manifest ordered IDs"); err != nil {
			return err
		}
		for i, r := range rows {
			if err := guidanceRequire(catalogObject(r)["path"] == "guidance/"+catalogToolIDs[i]+".json", "manifest package path"); err != nil {
				return err
			}
		}
		return guidanceRequire(catalogObject(v["required_global_policy"])["path"] == "guidance/required-global-policy.json", "manifest global path")
	case CatalogGuidancePackage:
		// These four packages are frozen source evidence, including all rules,
		// workflow, capabilities and owner/help labels. Changes need a new contract.
		id, _ := v["tool_id"].(string)
		return guidanceRequire(guidancePackages[id] != nil && catalogEqual(v, guidancePackages[id]), "frozen package identity/rules/workflow/owner/help")
	case CatalogGuidanceProfile:
		return guidanceRequire(guidanceIDs(catalogArray(v["tools"]), "tool_id") && catalogObject(v["evidence"])["path"] == "inputs/guidance-observation.json", "profile ordered IDs/evidence")
	case CatalogGuidanceObservation:
		if err := guidanceRequire(v["source_commit"] == "0ff4f4c6882294a59992baf2fed6607ff2467c50" && v["source_tree"] == "bd26c0446428e4268e0895c8918f5aeacde8639e", "historical observation tuple"); err != nil {
			return err
		}
		for _, key := range []string{"source_reads", "descriptor_reads"} {
			r := catalogObject(v[key])
			expected := catalogObject(guidanceObservationMetadata[key])
			if err := guidanceRequire(r["api_version"] == "huyang.workspace/v1alpha1" && r["outcome"] == "ok" && r["request_id"] == expected["request_id"] && r["summary"] == expected["summary"] && catalogEqual(r["workspace"], expected["workspace"]) && len(catalogArray(r["next"])) == 0 && len(catalogArray(r["warnings"])) == 0, "frozen observation metadata"); err != nil {
				return err
			}
			data := catalogObject(r["data"])
			entries, files := catalogArray(data["entries"]), catalogArray(data["files"])
			wanted := catalogArray(expected["files"])
			if err := guidanceRequire(len(entries) == len(wanted) && len(files) == len(wanted), "observation inventory"); err != nil {
				return err
			}
			for i, e := range wanted {
				w := catalogObject(e)
				entry, file := catalogObject(entries[i]), catalogObject(files[i])
				content := file["content"].(string)
				if err := guidanceRequire(catalogPath(file["path"].(string), false) && file["path"] == w["path"] && entry["path"] == w["path"] && file["revision_id"] == w["revision_id"] && catalogEqual(file["lines"], w["lines"]) && catalogEqual(entry["lines"], w["lines"]) && catalogNumber(entry["bytes"]) == int64(len(content)) && catalogNumber(file["lines"]) == int64(strings.Count(content, "\n")) && digestOf([]byte(content)) == w["sha256"], "observation source/descriptor record"); err != nil {
					return err
				}
			}
		}
	case CatalogGuidanceGlobalPolicy:
		seam := catalogObject(v["deferred_owner_seam"])
		if err := guidanceRequire(catalogEqual(seam, guidanceSeam), "required owner handoff"); err != nil {
			return err
		}
		texts := catalogObject(v["texts"])
		for _, q := range catalogArray(seam["resident_quotes"]) {
			for _, k := range []string{"shared", "claude"} {
				if err := guidanceRequire(strings.Contains(texts[k].(string), q.(string)), "seam residency"); err != nil {
					return err
				}
			}
		}
		p := catalogObject(v["provenance"])
		for _, k := range []string{"source_sha256", "claude_source_sha256", "prior_template_sha256", "reference_sha256"} {
			if err := guidanceRequire(catalogPin(p[k].(string), 64), "global provenance digest"); err != nil {
				return err
			}
		}
		for _, k := range []string{"source_path", "claude_source_path"} {
			if err := guidanceRequire(catalogPath(p[k].(string), false), "global provenance path"); err != nil {
				return err
			}
		}
		for _, h := range catalogObject(p["baseline_main_sha256"]) {
			if err := guidanceRequire(catalogPin(h.(string), 64), "baseline digest syntax"); err != nil {
				return err
			}
		}
		if err := guidanceRequire(p["source_path"] == "shared/agent-instructions/config/bin/agents-instructions-gen" && p["claude_source_path"] == "shared/agent-instructions/config/claude/CLAUDE.md", "global source paths"); err != nil {
			return err
		}
		if err := guidanceRequire(digestOf([]byte(v["operational_reference"].(string))) == p["reference_sha256"] && p["reference_sha256"] == "6c0d6ae65c7aebda0d4785115841e7cf2e65e3ddd2a71485b71cce0e488fa2d4", "retained operational reference bytes"); err != nil {
			return err
		}
		return guidanceRequire(digestOf([]byte(texts["shared"].(string))) == "ec5cf664a5d41302c71c58561b07057fa6acaef540402c2a2b909db30c4e1c5d" && digestOf([]byte(texts["claude"].(string))) == "ad185082d1b16d52bb2c2866a3750fb074df37a32df1c1c4f5303dec92e7d5ef", "frozen global policy text")
	case CatalogGuidanceLayout:
		b := catalogObject(v["bindings"])
		for _, key := range []string{"guidance_manifest_sha256", "required_global_policy_sha256", "policy_sha256", "operational_reference_sha256", "imported_owner_sha256"} {
			if err := guidanceRequire(catalogPin(b[key].(string), 64), "layout digest syntax"); err != nil {
				return err
			}
		}
		for _, key := range []string{"baseline_main_sha256", "model_block_sha256", "imported_source_sha256"} {
			for _, h := range catalogObject(b[key]) {
				if err := guidanceRequire(catalogPin(h.(string), 64), "layout map digest syntax"); err != nil {
					return err
				}
			}
		}
		if err := guidanceRequire(b["import_repository"] == "github.com/iryzhkov/omarchy-setup" && b["import_commit"] == "b32c0b6f48e12b7d74a60f8a010f84bab73d299f" && b["import_tree"] == "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52", "layout import tuple"); err != nil {
			return err
		}
		texts := catalogObject(v["texts"])
		for key, wanted := range map[string][]string{"shared": {"GLOBAL_SHARED", "FRAGMENTS", "MODEL_CODEX", "DISCOVERY"}, "claude": {"GLOBAL_CLAUDE", "FRAGMENTS", "MODEL_CLAUDE", "DISCOVERY"}, "codex": {"SHARED_MAIN"}, "discovery": {"MENU_ROWS", "CATALOG_SHA256", "PRODUCER_RECEIPT_SHA256"}} {
			s := texts[key].(string)
			for _, token := range wanted {
				marker := "{{" + token + "}}"
				if err := guidanceRequire(strings.Count(s, marker) == 1, "layout token inventory"); err != nil {
					return err
				}
				s = strings.Replace(s, marker, "", 1)
			}
			if err := guidanceRequire(!strings.ContainsAny(s, "{}"), "malformed/unknown layout token"); err != nil {
				return err
			}
		}
	}
	return nil
}

// CatalogGuidanceAudit is an immutable inert audit. A zero value carries no
// verified categories. It cannot enter a preparation or generation store.
type CatalogGuidanceAudit struct {
	commit, tree               string
	verified, blockers, future []string
	catalog, profile           []byte
}

func (a CatalogGuidanceAudit) Identity() (string, string) { return a.commit, a.tree }
func (a CatalogGuidanceAudit) VerifiedCategories() []string {
	return append([]string(nil), a.verified...)
}
func (a CatalogGuidanceAudit) Blockers() []string       { return append([]string(nil), a.blockers...) }
func (a CatalogGuidanceAudit) FutureRequired() []string { return append([]string(nil), a.future...) }

// FullCatalogEncoding preserves all retained metadata; it is not authorization.
func (a CatalogGuidanceAudit) FullCatalogEncoding() []byte { return bytes.Clone(a.catalog) }
func (a CatalogGuidanceAudit) ProfileEncoding() []byte     { return bytes.Clone(a.profile) }

// InspectCatalogGuidanceSnapshot serializes with Close and verifies actual
// custody twice. No input is reacquired, executed, projected or granted authority.
func InspectCatalogGuidanceSnapshot(s *CatalogSnapshot) (CatalogGuidanceAudit, error) {
	if s == nil {
		return CatalogGuidanceAudit{}, errors.New("catalog guidance nil snapshot")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return inspectCatalogGuidanceLocked(context.Background(), s)
}

// inspectCatalogGuidanceLocked is the one semantic body behind both guidance
// frontdoors. The caller holds s.mu; ctx is checked between bounded stages.
func inspectCatalogGuidanceLocked(ctx context.Context, s *CatalogSnapshot) (CatalogGuidanceAudit, error) {
	fail := func(err error) (CatalogGuidanceAudit, error) { return CatalogGuidanceAudit{}, err }
	if s.closed || s.verify == nil || s.close == nil || !catalogPin(s.commit, 40) || !catalogPin(s.tree, 40) || len(s.sources) != 26 || len(s.inputs) != 13 {
		return fail(errors.New("catalog guidance snapshot closed/uninitialized/incomplete"))
	}
	if err := s.verify(ctx); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	for p := range snapshotSourceSpecs() {
		b := s.sources[p]
		if len(b) == 0 || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
			return fail(fmt.Errorf("catalog guidance retained source %s", p))
		}
	}
	values := map[string]map[string]any{}
	for p := range snapshotInputSpecs() {
		if len(s.inputs[p]) == 0 {
			return fail(fmt.Errorf("catalog guidance retained input %s", p))
		}
	}
	for p, k := range guidanceInputKinds() {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		raw := s.inputs[p]
		if err := ValidateCatalogGuidanceInputEncoding(k, raw); err != nil {
			return fail(fmt.Errorf("%s: %w", p, err))
		}
		v, err := canonjson.DecodeObject(raw)
		if err != nil {
			return fail(err)
		}
		values[p] = v
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := guidanceSnapshotRelationships(s.sources, s.inputs, values); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := s.verify(ctx); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	blockers := []string{"t3-steward: blocked-owner-handoff; catalog-absent"}
	profile := values["inputs/guidance-profile.json"]
	for _, row := range catalogArray(profile["tools"]) {
		r := catalogObject(row)
		id := r["tool_id"].(string)
		if (id == "fixture-cli" || id == "huyang") && (r["availability"] != "observed" || r["visibility"] != "visible") {
			blockers = append(blockers, id+": required-interface-blocked")
		}
	}
	return CatalogGuidanceAudit{
		commit: s.commit, tree: s.tree,
		verified: []string{"retained physical custody and complete source26/input13 inventory", "nine frozen closed input grammars and finite identities", "seven source/input byte parities", "retained catalog full four-row identities and input digest crosspins", "historical observation17 content/inventory/metadata and retained source parity", "package owning-source/capability/catalog-help bindings and frozen rule/workflow labels", "retained global text/reference and required owner seam", "layout syntax and retained import/source/policy/manifest/global crosspins", "fixture profile observation digest and non-authorizing labels"},
		blockers: blockers,
		future:   []string{"four-old-input full semantic grammars; configured catalog authorization; release-manifest semantics", "source approval/build lineage and independent candidate review/exact CI", "Stage A rerun and expected producer receipt authenticity", "authentic b32 baseline_main, prior_template and model_block generated-byte derivation", "Huyang pinned-reference generated target bytes, evidence baseline and installed accessibility", "tokenizer/interpreter/helper acquisition and execution lineage", "projection/output semantics, protocol/recovery, preservation and unchanged token budgets", "genuine preparation/store/installer/fleet and verified Steward owning interface; Stage B NOT_MET"},
		catalog:  bytes.Clone(s.inputs["inputs/tool-catalog.json"]), profile: bytes.Clone(s.inputs["inputs/guidance-profile.json"]),
	}, nil
}

func guidanceInputKinds() map[string]CatalogGuidanceInputKind {
	m := map[string]CatalogGuidanceInputKind{
		"inputs/tool-guidance-manifest.json": CatalogGuidanceManifest, "inputs/required-global-policy.json": CatalogGuidanceGlobalPolicy, "inputs/guidance-layout.json": CatalogGuidanceLayout, "inputs/guidance-profile.json": CatalogGuidanceProfile, "inputs/guidance-observation.json": CatalogGuidanceObservation}
	for _, id := range catalogToolIDs {
		m["inputs/tool-guidance/"+id+".json"] = CatalogGuidancePackage
	}
	return m
}
func guidanceSnapshotRelationships(sources, inputs map[string][]byte, v map[string]map[string]any) error {
	parity := map[string]string{"inputs/tool-guidance-manifest.json": "guidance/manifest.json", "inputs/required-global-policy.json": "guidance/required-global-policy.json", "inputs/guidance-layout.json": "templates/discovery.txt"}
	for _, id := range catalogToolIDs {
		parity["inputs/tool-guidance/"+id+".json"] = "guidance/" + id + ".json"
	}
	for p, q := range parity {
		if err := guidanceRequire(bytes.Equal(inputs[p], sources["shared/catalog-guidance/"+q]), "source/input parity "+p); err != nil {
			return err
		}
	}
	old := map[string]map[string]any{}
	for _, name := range []string{"tool-catalog", "tool-producer-input", "tool-producer-receipt", "route-policy"} {
		raw := inputs["inputs/"+name+".json"]
		if err := snapshotCanonical(raw); err != nil {
			return err
		}
		o, err := canonjson.DecodeObject(raw)
		if err != nil {
			return err
		}
		old[name] = o
	}
	cat, receipt := old["tool-catalog"], old["tool-producer-receipt"]
	// Full frozen rows include capabilities, required flags, aliases, artifact IDs,
	// platform and primary route identities. This does not approve a catalog.
	if err := guidanceRequire(catalogEqual(cat["tools"], guidanceCatalogTools), "full canonical catalog rows"); err != nil {
		return err
	}
	manifest := v["inputs/tool-guidance-manifest.json"]
	if err := guidanceRequire(manifest["catalog_sha256"] == digestOf(inputs["inputs/tool-catalog.json"]) && manifest["producer_receipt_sha256"] == digestOf(inputs["inputs/tool-producer-receipt.json"]) && manifest["policy_sha256"] == digestOf(inputs["inputs/route-policy.json"]) && receipt["catalog_sha256"] == digestOf(inputs["inputs/tool-catalog.json"]) && receipt["input_sha256"] == digestOf(inputs["inputs/tool-producer-input.json"]), "retained catalog/producer/policy digest crosspins"); err != nil {
		return err
	}
	global := v["inputs/required-global-policy.json"]
	layout := v["inputs/guidance-layout.json"]
	bind := catalogObject(layout["bindings"])
	provenance := catalogObject(global["provenance"])
	required := catalogObject(manifest["required_global_policy"])
	if err := guidanceRequire(required["sha256"] == digestOf(inputs["inputs/required-global-policy.json"]) && bind["guidance_manifest_sha256"] == digestOf(inputs["inputs/tool-guidance-manifest.json"]) && bind["required_global_policy_sha256"] == required["sha256"] && bind["policy_sha256"] == manifest["policy_sha256"] && bind["operational_reference_sha256"] == provenance["reference_sha256"] && catalogEqual(bind["baseline_main_sha256"], provenance["baseline_main_sha256"]), "layout/global/manifest/reference crosslinks"); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"source_path", "source_sha256"}, {"claude_source_path", "claude_source_sha256"}} {
		p := provenance[pair[0]].(string)
		if err := guidanceRequire(len(sources[p]) != 0 && provenance[pair[1]] == digestOf(sources[p]), "global retained source binding"); err != nil {
			return err
		}
	}
	if err := guidanceRequire(bind["imported_owner_sha256"] == digestOf(sources["shared/agent-instructions/owner.json"]), "imported owner bytes"); err != nil {
		return err
	}
	for p, h := range catalogObject(bind["imported_source_sha256"]) {
		if err := guidanceRequire(len(sources["shared/agent-instructions/"+p]) != 0 && h == digestOf(sources["shared/agent-instructions/"+p]), "imported source bytes"); err != nil {
			return err
		}
	}
	observation := v["inputs/guidance-observation.json"]
	for _, key := range []string{"source_reads", "descriptor_reads"} {
		for _, row := range catalogArray(catalogObject(catalogObject(observation[key])["data"])["files"]) {
			f := catalogObject(row)
			p := f["path"].(string)
			if err := guidanceRequire(bytes.Equal(sources[p], []byte(f["content"].(string))), "actual observation source parity "+p); err != nil {
				return err
			}
		}
	}
	profile := v["inputs/guidance-profile.json"]
	if err := guidanceRequire(catalogObject(profile["evidence"])["sha256"] == digestOf(inputs["inputs/guidance-observation.json"]), "profile observation bytes"); err != nil {
		return err
	}
	for i, row := range catalogArray(manifest["records"]) {
		r := catalogObject(row)
		id := catalogToolIDs[i]
		p := "inputs/tool-guidance/" + id + ".json"
		pkg := v[p]
		owner := catalogObject(pkg["owner"])
		help := catalogObject(pkg["help"])
		if err := guidanceRequire(r["sha256"] == digestOf(inputs[p]) && owner["source_sha256"] == digestOf(sources[owner["source_path"].(string)]), "package manifest/source bytes"); err != nil {
			return err
		}
		if id == "huyang" {
			// The baseline and reference are generated outputs absent from source26.
			// Only available crosslinks and frozen descriptor labels are checked here.
			if err := guidanceRequire(owner["evidence_sha256"] == catalogObject(bind["baseline_main_sha256"])[".config/agents/AGENTS.md"] && owner["source_sha256"] == catalogObject(bind["imported_source_sha256"])["config/claude/skills/huyang/SKILL.md"], "Huyang retained source and baseline crosslink"); err != nil {
				return err
			}
		} else {
			if err := guidanceRequire(owner["evidence_sha256"] == digestOf(inputs["inputs/tool-catalog.json"]) && help["sha256"] == digestOf(inputs["inputs/tool-catalog.json"]) && help["tool_id"] == id, "catalog evidence and full-record help bytes"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Private frozen definitions transcribed from archive 8ddcd067... after its
// complete regular-member/manifest audit. No caller-defined schema is evaluated.
var guidanceSchemas = guidanceFrozenObject(guidanceSchemaJSON)
var guidancePackages = guidanceFrozenObject(guidancePackageJSON)
var guidanceSeam = guidanceFrozenObject(guidanceSeamJSON)
var guidanceObservationMetadata = guidanceFrozenObject(guidanceObservationMetadataJSON)
var guidanceCatalogTools = func() any {
	v, err := canonjson.DecodeObject([]byte(guidanceCatalogToolsJSON))
	if err != nil {
		panic(err)
	}
	return v["tools"]
}()

func guidanceFrozenObject(s string) map[string]any {
	v, err := canonjson.DecodeObject([]byte(s))
	if err != nil {
		panic(err)
	}
	return v
}

const guidanceSchemaJSON = "{\"tool_guidance_manifest_v1\":{\"additionalProperties\":false,\"properties\":{\"catalog_sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"},\"policy_sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"},\"producer_receipt_sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"},\"records\":{\"items\":{\"additionalProperties\":false,\"properties\":{\"path\":{\"maxLength\":4096,\"minLength\":1,\"pattern\":\"^(?!/)(?!.*(?:^|/)\\\\.\\\\.?(?:/|$))(?!.*\\\\\\\\)[^\\\\u0000]+$\",\"type\":\"string\"},\"sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"},\"tool_id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"path\",\"sha256\",\"tool_id\"],\"type\":\"object\"},\"maxItems\":1024,\"type\":\"array\"},\"required_global_policy\":{\"additionalProperties\":false,\"properties\":{\"path\":{\"maxLength\":4096,\"minLength\":1,\"pattern\":\"^(?!/)(?!.*(?:^|/)\\\\.\\\\.?(?:/|$))(?!.*\\\\\\\\)[^\\\\u0000]+$\",\"type\":\"string\"},\"sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"}},\"required\":[\"path\",\"sha256\"],\"type\":\"object\"},\"schema\":{\"const\":\"tool-guidance-manifest/v1\"},\"schema_version\":{\"const\":1,\"type\":\"integer\"}},\"required\":[\"catalog_sha256\",\"policy_sha256\",\"producer_receipt_sha256\",\"records\",\"required_global_policy\",\"schema\",\"schema_version\"],\"type\":\"object\"},\"tool_guidance_v1\":{\"additionalProperties\":false,\"properties\":{\"capability_ids\":{\"items\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"maxItems\":1024,\"type\":\"array\"},\"help\":{\"oneOf\":[{\"additionalProperties\":false,\"properties\":{\"display_path\":{\"const\":\"~/.config/agents/huyang.md\"},\"kind\":{\"const\":\"pinned-reference\"},\"path\":{\"maxLength\":4096,\"minLength\":1,\"pattern\":\"^(?!/)(?!.*(?:^|/)\\\\.\\\\.?(?:/|$))(?!.*\\\\\\\\)[^\\\\u0000]+$\",\"type\":\"string\"},\"sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"}},\"required\":[\"display_path\",\"kind\",\"path\",\"sha256\"],\"type\":\"object\"},{\"additionalProperties\":false,\"properties\":{\"kind\":{\"const\":\"catalog-record\"},\"path\":{\"const\":\"inputs/tool-catalog.json\"},\"sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"},\"tool_id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"kind\",\"path\",\"sha256\",\"tool_id\"],\"type\":\"object\"}]},\"owner\":{\"additionalProperties\":false,\"properties\":{\"evidence_path\":{\"maxLength\":4096,\"minLength\":1,\"pattern\":\"^(?!/)(?!.*(?:^|/)\\\\.\\\\.?(?:/|$))(?!.*\\\\\\\\)[^\\\\u0000]+$\",\"type\":\"string\"},\"evidence_sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"},\"kind\":{\"enum\":[\"catalog-fixture\",\"imported-guidance-proposal\"]},\"owning_path\":{\"maxLength\":4096,\"minLength\":1,\"pattern\":\"^(?!/)(?!.*(?:^|/)\\\\.\\\\.?(?:/|$))(?!.*\\\\\\\\)[^\\\\u0000]+$\",\"type\":\"string\"},\"source_path\":{\"maxLength\":4096,\"minLength\":1,\"pattern\":\"^(?!/)(?!.*(?:^|/)\\\\.\\\\.?(?:/|$))(?!.*\\\\\\\\)[^\\\\u0000]+$\",\"type\":\"string\"},\"source_sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"},\"status\":{\"const\":\"hypothetical-new-owner-input-unapproved\"}},\"required\":[\"evidence_path\",\"evidence_sha256\",\"kind\",\"owning_path\",\"source_path\",\"source_sha256\",\"status\"],\"type\":\"object\"},\"purpose\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"rules\":{\"items\":{\"additionalProperties\":false,\"properties\":{\"evidence_quote_sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"},\"id\":{\"pattern\":\"^[a-z][a-z0-9-]*\\\\.[a-z-]+\\\\.v1$\",\"type\":\"string\"},\"placement\":{\"const\":\"before-tool-choice\"},\"text\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"evidence_quote_sha256\",\"id\",\"placement\",\"text\"],\"type\":\"object\"},\"maxItems\":1024,\"type\":\"array\"},\"schema\":{\"const\":\"tool-guidance/v1\"},\"schema_version\":{\"const\":1,\"type\":\"integer\"},\"tool_id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"workflow\":{\"items\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"maxItems\":1024,\"type\":\"array\"}},\"required\":[\"capability_ids\",\"help\",\"owner\",\"purpose\",\"rules\",\"schema\",\"schema_version\",\"tool_id\",\"workflow\"],\"type\":\"object\"},\"required_global_policy_v1\":{\"additionalProperties\":false,\"properties\":{\"deferred_owner_seam\":{\"additionalProperties\":false,\"properties\":{\"catalog_membership\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"current_bound_owner\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"handoff\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"required_interface\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"required_rules\":{\"items\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"maxItems\":1024,\"type\":\"array\"},\"resident_quotes\":{\"items\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"maxItems\":1024,\"type\":\"array\"},\"schema\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"status\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"tool_id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"catalog_membership\",\"current_bound_owner\",\"handoff\",\"required_interface\",\"required_rules\",\"resident_quotes\",\"schema\",\"status\",\"tool_id\"],\"type\":\"object\"},\"operational_reference\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"provenance\":{\"additionalProperties\":false,\"properties\":{\"baseline_main_sha256\":{\"additionalProperties\":false,\"properties\":{\".claude/omarchy-setup/CLAUDE.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\".codex/AGENTS.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\".config/agents/AGENTS.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\".claude/omarchy-setup/CLAUDE.md\",\".codex/AGENTS.md\",\".config/agents/AGENTS.md\"],\"type\":\"object\"},\"claude_source_path\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"claude_source_sha256\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"prior_template_sha256\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"reference_sha256\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"source_path\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"source_sha256\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"baseline_main_sha256\",\"claude_source_path\",\"claude_source_sha256\",\"prior_template_sha256\",\"reference_sha256\",\"source_path\",\"source_sha256\"],\"type\":\"object\"},\"schema\":{\"const\":\"required-global-policy/v1\"},\"schema_version\":{\"const\":1,\"type\":\"integer\"},\"texts\":{\"additionalProperties\":false,\"properties\":{\"claude\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"shared\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"claude\",\"shared\"],\"type\":\"object\"}},\"required\":[\"deferred_owner_seam\",\"operational_reference\",\"provenance\",\"schema\",\"schema_version\",\"texts\"],\"type\":\"object\"},\"layout_v4\":{\"additionalProperties\":false,\"properties\":{\"bindings\":{\"additionalProperties\":false,\"properties\":{\"baseline_main_sha256\":{\"additionalProperties\":false,\"properties\":{\".claude/omarchy-setup/CLAUDE.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\".codex/AGENTS.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\".config/agents/AGENTS.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\".claude/omarchy-setup/CLAUDE.md\",\".codex/AGENTS.md\",\".config/agents/AGENTS.md\"],\"type\":\"object\"},\"guidance_manifest_sha256\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"import_commit\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"import_repository\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"import_tree\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"imported_owner_sha256\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"imported_source_sha256\":{\"additionalProperties\":false,\"properties\":{\"config/bin/agents-instructions-gen\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/bin/agents-role-project\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/CLAUDE.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/skills/huyang/SKILL.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/skills/implementation-session/SKILL.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/skills/implementation-session/references/prompt-template.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/skills/ov-memory-curation/SKILL.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/skills/ov-memory/SKILL.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/skills/publish-agent-tooling/SKILL.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/skills/t3-campaign/SKILL.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/skills/t3-campaign/references/executor-briefs.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/skills/t3-task/SKILL.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"config/claude/skills/t3-wait/SKILL.md\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"config/bin/agents-instructions-gen\",\"config/bin/agents-role-project\",\"config/claude/CLAUDE.md\",\"config/claude/skills/huyang/SKILL.md\",\"config/claude/skills/implementation-session/SKILL.md\",\"config/claude/skills/implementation-session/references/prompt-template.md\",\"config/claude/skills/ov-memory-curation/SKILL.md\",\"config/claude/skills/ov-memory/SKILL.md\",\"config/claude/skills/publish-agent-tooling/SKILL.md\",\"config/claude/skills/t3-campaign/SKILL.md\",\"config/claude/skills/t3-campaign/references/executor-briefs.md\",\"config/claude/skills/t3-task/SKILL.md\",\"config/claude/skills/t3-wait/SKILL.md\"],\"type\":\"object\"},\"model_block_sha256\":{\"additionalProperties\":false,\"properties\":{\"claude\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"codex\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"claude\",\"codex\"],\"type\":\"object\"},\"operational_reference_sha256\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"policy_sha256\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"required_global_policy_sha256\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"baseline_main_sha256\",\"guidance_manifest_sha256\",\"import_commit\",\"import_repository\",\"import_tree\",\"imported_owner_sha256\",\"imported_source_sha256\",\"model_block_sha256\",\"operational_reference_sha256\",\"policy_sha256\",\"required_global_policy_sha256\"],\"type\":\"object\"},\"schema_version\":{\"const\":4,\"type\":\"integer\"},\"texts\":{\"additionalProperties\":false,\"properties\":{\"claude\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"codex\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"discovery\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"shared\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"claude\",\"codex\",\"discovery\",\"shared\"],\"type\":\"object\"}},\"required\":[\"bindings\",\"schema_version\",\"texts\"],\"type\":\"object\"},\"guidance_profile_v1\":{\"additionalProperties\":false,\"properties\":{\"evidence\":{\"additionalProperties\":false,\"properties\":{\"path\":{\"maxLength\":4096,\"minLength\":1,\"pattern\":\"^(?!/)(?!.*(?:^|/)\\\\.\\\\.?(?:/|$))(?!.*\\\\\\\\)[^\\\\u0000]+$\",\"type\":\"string\"},\"sha256\":{\"pattern\":\"^[0-9a-f]{64}$\",\"type\":\"string\"}},\"required\":[\"path\",\"sha256\"],\"type\":\"object\"},\"schema\":{\"const\":\"guidance-profile/v1\"},\"schema_version\":{\"const\":1,\"type\":\"integer\"},\"scope\":{\"const\":\"fixture-only\"},\"snapshot_id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"tools\":{\"items\":{\"additionalProperties\":false,\"properties\":{\"authorized\":{\"enum\":[\"unknown\",\"denied\",\"declaration-only\"]},\"availability\":{\"enum\":[\"observed\",\"unavailable\",\"unknown\"]},\"enforced\":{\"const\":\"unqualified\"},\"tool_id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"visibility\":{\"enum\":[\"visible\",\"hidden\"]}},\"required\":[\"authorized\",\"availability\",\"enforced\",\"tool_id\",\"visibility\"],\"type\":\"object\"},\"maxItems\":1024,\"type\":\"array\"}},\"required\":[\"evidence\",\"schema\",\"schema_version\",\"scope\",\"snapshot_id\",\"tools\"],\"type\":\"object\"},\"guidance_observation_v1\":{\"additionalProperties\":false,\"properties\":{\"descriptor_reads\":{\"additionalProperties\":false,\"properties\":{\"api_version\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"data\":{\"additionalProperties\":false,\"properties\":{\"entries\":{\"items\":{\"additionalProperties\":false,\"properties\":{\"bytes\":{\"type\":\"integer\"},\"lines\":{\"type\":\"integer\"},\"path\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"bytes\",\"lines\",\"path\"],\"type\":\"object\"},\"maxItems\":1024,\"type\":\"array\"},\"files\":{\"items\":{\"additionalProperties\":false,\"properties\":{\"content\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"lines\":{\"type\":\"integer\"},\"path\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"revision_id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"content\",\"lines\",\"path\",\"revision_id\"],\"type\":\"object\"},\"maxItems\":1024,\"type\":\"array\"}},\"required\":[\"entries\",\"files\"],\"type\":\"object\"},\"next\":{\"items\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"maxItems\":1024,\"type\":\"array\"},\"outcome\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"request_id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"summary\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"warnings\":{\"items\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"maxItems\":1024,\"type\":\"array\"},\"workspace\":{\"additionalProperties\":false,\"properties\":{\"id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"revision\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"id\",\"revision\"],\"type\":\"object\"}},\"required\":[\"api_version\",\"data\",\"next\",\"outcome\",\"request_id\",\"summary\",\"warnings\",\"workspace\"],\"type\":\"object\"},\"schema\":{\"const\":\"guidance-observation/v1\"},\"schema_version\":{\"const\":1,\"type\":\"integer\"},\"source_commit\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"source_reads\":{\"additionalProperties\":false,\"properties\":{\"api_version\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"data\":{\"additionalProperties\":false,\"properties\":{\"entries\":{\"items\":{\"additionalProperties\":false,\"properties\":{\"bytes\":{\"type\":\"integer\"},\"lines\":{\"type\":\"integer\"},\"path\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"bytes\",\"lines\",\"path\"],\"type\":\"object\"},\"maxItems\":1024,\"type\":\"array\"},\"files\":{\"items\":{\"additionalProperties\":false,\"properties\":{\"content\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"lines\":{\"type\":\"integer\"},\"path\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"revision_id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"content\",\"lines\",\"path\",\"revision_id\"],\"type\":\"object\"},\"maxItems\":1024,\"type\":\"array\"}},\"required\":[\"entries\",\"files\"],\"type\":\"object\"},\"next\":{\"items\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"maxItems\":1024,\"type\":\"array\"},\"outcome\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"request_id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"summary\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"warnings\":{\"items\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"maxItems\":1024,\"type\":\"array\"},\"workspace\":{\"additionalProperties\":false,\"properties\":{\"id\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"},\"revision\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"id\",\"revision\"],\"type\":\"object\"}},\"required\":[\"api_version\",\"data\",\"next\",\"outcome\",\"request_id\",\"summary\",\"warnings\",\"workspace\"],\"type\":\"object\"},\"source_tree\":{\"maxLength\":65536,\"minLength\":1,\"type\":\"string\"}},\"required\":[\"descriptor_reads\",\"schema\",\"schema_version\",\"source_commit\",\"source_reads\",\"source_tree\"],\"type\":\"object\"}}"

const guidancePackageJSON = "{\"fixture-cli\":{\"capability_ids\":[\"fixture-inspect\"],\"help\":{\"kind\":\"catalog-record\",\"path\":\"inputs/tool-catalog.json\",\"sha256\":\"e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a\",\"tool_id\":\"fixture-cli\"},\"owner\":{\"evidence_path\":\"inputs/tool-catalog.json\",\"evidence_sha256\":\"e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a\",\"kind\":\"catalog-fixture\",\"owning_path\":\"shared/catalog-guidance/guidance/fixture-cli.json\",\"source_path\":\"internal/toolproducer/testdata/descriptors/fixture-cli.json\",\"source_sha256\":\"a0e6c6a8f4f1643c8a039ff97c0ff7a854edaba182bf9cdbfd7c52f3d279d74e\",\"status\":\"hypothetical-new-owner-input-unapproved\"},\"purpose\":\"inspection declaration\",\"rules\":[],\"schema\":\"tool-guidance/v1\",\"schema_version\":1,\"tool_id\":\"fixture-cli\",\"workflow\":[]},\"fixture-mcp\":{\"capability_ids\":[\"fixture-metadata\"],\"help\":{\"kind\":\"catalog-record\",\"path\":\"inputs/tool-catalog.json\",\"sha256\":\"e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a\",\"tool_id\":\"fixture-mcp\"},\"owner\":{\"evidence_path\":\"inputs/tool-catalog.json\",\"evidence_sha256\":\"e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a\",\"kind\":\"catalog-fixture\",\"owning_path\":\"shared/catalog-guidance/guidance/fixture-mcp.json\",\"source_path\":\"internal/toolproducer/testdata/descriptors/fixture-mcp.json\",\"source_sha256\":\"5d061d829dab6e36d6c1899e2d6f2780fb129ae25efc28fe688d59cc41a6ef5b\",\"status\":\"hypothetical-new-owner-input-unapproved\"},\"purpose\":\"metadata declaration\",\"rules\":[],\"schema\":\"tool-guidance/v1\",\"schema_version\":1,\"tool_id\":\"fixture-mcp\",\"workflow\":[]},\"fixture-third-party\":{\"capability_ids\":[\"fixture-reference\"],\"help\":{\"kind\":\"catalog-record\",\"path\":\"inputs/tool-catalog.json\",\"sha256\":\"e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a\",\"tool_id\":\"fixture-third-party\"},\"owner\":{\"evidence_path\":\"inputs/tool-catalog.json\",\"evidence_sha256\":\"e52120624ef2db6914e88c903c2decdd061daf56d5ca5421a061cbb57eceb77a\",\"kind\":\"catalog-fixture\",\"owning_path\":\"shared/catalog-guidance/guidance/fixture-third-party.json\",\"source_path\":\"internal/toolproducer/testdata/descriptors/fixture-third-party.json\",\"source_sha256\":\"e0923229f54efe7396e9eb396c8f51179c56b94a9756b0316fb767411ae7950e\",\"status\":\"hypothetical-new-owner-input-unapproved\"},\"purpose\":\"reference declaration\",\"rules\":[],\"schema\":\"tool-guidance/v1\",\"schema_version\":1,\"tool_id\":\"fixture-third-party\",\"workflow\":[]},\"huyang\":{\"capability_ids\":[\"file-read\",\"file-search\",\"repository-operations\",\"transactional-edit\"],\"help\":{\"display_path\":\"~/.config/agents/huyang.md\",\"kind\":\"pinned-reference\",\"path\":\".config/agents/huyang.md\",\"sha256\":\"d325c443031af38500a36f89d964e0f6f240e5acc4f2dabe6e0b011f5dfce8e4\"},\"owner\":{\"evidence_path\":\"baseline/.config/agents/AGENTS.md\",\"evidence_sha256\":\"4a1f076064ad4ed6a86c66c52e5bdf670bdc077574d202696bbcb225fade81d9\",\"kind\":\"imported-guidance-proposal\",\"owning_path\":\"shared/catalog-guidance/guidance/huyang.json\",\"source_path\":\"shared/agent-instructions/config/claude/skills/huyang/SKILL.md\",\"source_sha256\":\"3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f\",\"status\":\"hypothetical-new-owner-input-unapproved\"},\"purpose\":\"repository IO via MCP\",\"rules\":[{\"evidence_quote_sha256\":\"4208847d09477ead6a6bb93f532bda43c3beee5b2ae63abb8e790dc7247f94a9\",\"id\":\"huyang.repository-boundary.v1\",\"placement\":\"before-tool-choice\",\"text\":\"Repository IO (`huyang`, huyang.md): first `workspace_open` root before any source operation/survey/scaffolding/creation; retain ID/revision. Use Huyang semantic reads/transactional edits in every permission mode; results bind revision. Fix/report stale/conflict/incomplete-evidence refusals, never bypass. Shell only Git, builds/tests/programs whose output matters within verification-policy limits, or cross-directory unknown-owner searches; once located use Huyang. No direct repository filesystem IO/refusal evasion.\"}],\"schema\":\"tool-guidance/v1\",\"schema_version\":1,\"tool_id\":\"huyang\",\"workflow\":[\"Read the exact pinned reference before repository work. Detailed tool matrix and examples remain on demand.\"]}}"

const guidanceSeamJSON = "{\"catalog_membership\":\"absent\",\"current_bound_owner\":\"shared/agent-instructions\",\"handoff\":\"new separately reviewed source closure/version; remove bound policy quotes only after byte/semantic parity; never fabricate available row\",\"required_interface\":\"tool-guidance/v1 record from verified Steward owning help/guidance source with stable IDs, UTF8 hashes, actual help descriptor and provider restrictions\",\"required_rules\":[\"workflow-choice\",\"declared-independent-review\",\"subagent-required\",\"handoff-approval-notify\",\"task-ask-wait\",\"quarantine-gr3\"],\"resident_quotes\":[\"Steward (`t3-task`, `t3-campaign`, `t3-wait`, t3-steward.md): read matching guidance before first command. Explore/design/decide interactively; never hand off unagreed exploration. Use a subagent for quick same-provider reading or sub-work inside either an interactive session or a steward task; never replace declared tasks/reviews. Agree plan, write Jocasta, submit campaign with session notify identity without another approval; first ask for large cost/operator access/fleet release. Announce run/ledger; afterward retain only those, fetch details when needed. Independent/cross-provider work requires declared tasks/reviews. Recurrence uses schedules. Retired Markdown intake/wrappers remain disabled; retained diagnostics/fenced operator controls confer no intake/retry authority. Quarantine release is authenticated marker-only cleanup: creates nothing and cannot retry intake. The rollout lead retains file/quarantine evidence custody and owns immutable rollback bundles. Never reenable scanners or infer deployment from publication. Agreed planned dependent work uses campaigns; independent plan/diff reviews use declared `t3-steward review` within session/operator provider restrictions.\",\"Task decisions use only `t3-steward ask`: options/deadline/safe default, park/end, read answer on resume. Approver-required asks have no default, fail unanswered. Sessions use their question tool. Resolve notify identity before submission; intentional no-wake must be explicit. Follow closing wake instruction; collect ended runs immediately. Task-bound waits and thread waits immediately end the turn; never poll in agent loops.\"],\"schema\":\"required-owner-handoff/v1\",\"status\":\"blocked-owner-handoff\",\"tool_id\":\"t3-steward\"}"

const guidanceObservationMetadataJSON = "{\"source_reads\":{\"request_id\":\"req_9364\",\"summary\":\"Read 14 targets\",\"workspace\":{\"id\":\"ws_382d163465f14391122eb2efc963c14e\",\"revision\":\"wsrev_1\"},\"files\":[{\"path\":\"shared/agent-instructions/config/bin/agents-instructions-gen\",\"revision_id\":\"docrev_912fd047038248e6b9cd547c16fa77b83ab502b0acfbf99d562cec44029f5964\",\"lines\":524,\"sha256\":\"04f32b6a02ff9db34c54792d2de8b2b79fc014b7220ef70ffcbd6fc21bcdfaed\",\"newline_count\":524},{\"path\":\"shared/agent-instructions/config/bin/agents-role-project\",\"revision_id\":\"docrev_69b10a997a95df7d10b10a5f9f23850e47a4d242d186d2e98e062995831b035b\",\"lines\":305,\"sha256\":\"325ec02f4974a9ce102fd131ba6953ae48f4d45c1a2353fa02681088f1d113fd\",\"newline_count\":305},{\"path\":\"shared/agent-instructions/config/claude/CLAUDE.md\",\"revision_id\":\"docrev_c5590292675ccd1c7ff19952a6720c79dafa40d108a0b499fefad464b31db47a\",\"lines\":268,\"sha256\":\"59c21f214f8df452f1871375e061ba7d367a5a13e5dff7ece4f52260e2f61abd\",\"newline_count\":268},{\"path\":\"shared/agent-instructions/config/claude/skills/huyang/SKILL.md\",\"revision_id\":\"docrev_c8a99364eca488a8067a8d32d67df799cfee3f57be96cde39c225d8c75a0d0fd\",\"lines\":96,\"sha256\":\"3997f4d7ada96b36e88252ff4fde2fcf9657c50ae9d09d5b3bc85d0f594b092f\",\"newline_count\":96},{\"path\":\"shared/agent-instructions/config/claude/skills/implementation-session/SKILL.md\",\"revision_id\":\"docrev_3146f92afd8b9a4aad7e96a8f3d068de55692447ba526896f4ce12cf3843ab52\",\"lines\":74,\"sha256\":\"37e202ab9bd16de8fd7f776966079c71b6f554bcaa3eb152099507fad0f4a567\",\"newline_count\":74},{\"path\":\"shared/agent-instructions/config/claude/skills/implementation-session/references/prompt-template.md\",\"revision_id\":\"docrev_24cc64051d2ddf8e6e788431517288497cb06ec0b5d595fecb81fc9b4279b20d\",\"lines\":68,\"sha256\":\"4c46d852f6737f9c62b85b34275109c8f586b64fd60ae342a58047d35245f595\",\"newline_count\":68},{\"path\":\"shared/agent-instructions/config/claude/skills/ov-memory-curation/SKILL.md\",\"revision_id\":\"docrev_71e498b6736de418aa321f45ade35936ecc308e4faf220dd4fccbdc10a28f569\",\"lines\":84,\"sha256\":\"c9ae9a8555c7533d2cf24a9aaa235a165da35cf3215b194902363de31e53e82f\",\"newline_count\":84},{\"path\":\"shared/agent-instructions/config/claude/skills/ov-memory/SKILL.md\",\"revision_id\":\"docrev_c191cb13397251501e1688db03558f126683adafe9989350ed9f907600e63794\",\"lines\":136,\"sha256\":\"e0f282759a22b73c35e45fac382d9121ea60782533411a58552ae4b0aec47453\",\"newline_count\":136},{\"path\":\"shared/agent-instructions/config/claude/skills/publish-agent-tooling/SKILL.md\",\"revision_id\":\"docrev_0c78a80fbf194a09f47916adfb1ecdea3a859cdeb79d664bb2bb53d2cfb578a9\",\"lines\":89,\"sha256\":\"65a8b78f7ce01bf9b61ea0b89db395659ee0fdadde3111d2eb528d7b1c6292cf\",\"newline_count\":89},{\"path\":\"shared/agent-instructions/config/claude/skills/t3-campaign/SKILL.md\",\"revision_id\":\"docrev_d2d6e455d0f10501ad47f0747cefc15d94b306abc3e986f3310eec9b6d60ea3f\",\"lines\":428,\"sha256\":\"420fdd7e51cc689a1eea80db6444c2e8233f1fc657a7bc2d99ec2fa8ea25181d\",\"newline_count\":428},{\"path\":\"shared/agent-instructions/config/claude/skills/t3-campaign/references/executor-briefs.md\",\"revision_id\":\"docrev_4233c9921500fd072be95868861411eb5c41e8e011843602165e18aa24193c93\",\"lines\":209,\"sha256\":\"fb15f77d12c3895a4047fde6a34ddeb838f02a84a37bac14c0cd45c1589174b8\",\"newline_count\":209},{\"path\":\"shared/agent-instructions/config/claude/skills/t3-task/SKILL.md\",\"revision_id\":\"docrev_acd036fb276baf53773b02d407f1a5cc56f802b8f7ca08116ea00e623893993f\",\"lines\":139,\"sha256\":\"b3af2689fb0815f4bcdf052991a9db2d75b2a20f1c2736fc0cfc71254c59539b\",\"newline_count\":139},{\"path\":\"shared/agent-instructions/config/claude/skills/t3-wait/SKILL.md\",\"revision_id\":\"docrev_73763594ccaeb56b792c5623c55115f9d8e13aa9bf36a2284ae1eb8313006462\",\"lines\":117,\"sha256\":\"4695ba110bc471feff890f0b4c0429be09ded9824c0c1d04b6fefe075765d955\",\"newline_count\":117},{\"path\":\"shared/agent-instructions/owner.json\",\"revision_id\":\"docrev_af370b47b4589e249c0e058e7fa1f41d934141e7a13d5a35c3f63344885a0512\",\"lines\":55,\"sha256\":\"ff883da15c4877df6973bf9cada9c5fa33531bd96e4116dbc170eb3e5075d65e\",\"newline_count\":55}]},\"descriptor_reads\":{\"request_id\":\"req_9388\",\"summary\":\"Read 3 targets\",\"workspace\":{\"id\":\"ws_382d163465f14391122eb2efc963c14e\",\"revision\":\"wsrev_1\"},\"files\":[{\"path\":\"internal/toolproducer/testdata/descriptors/fixture-cli.json\",\"revision_id\":\"docrev_791dcb0abda006fd3fbf6d0bf646f08153f4951d3b0aae417790b78ca18a2f69\",\"lines\":26,\"sha256\":\"a0e6c6a8f4f1643c8a039ff97c0ff7a854edaba182bf9cdbfd7c52f3d279d74e\",\"newline_count\":26},{\"path\":\"internal/toolproducer/testdata/descriptors/fixture-mcp.json\",\"revision_id\":\"docrev_65e5d45295a507f12ab3cd5a8ae0664c0cd9ad1fd7926c9c9c3098668dcbde14\",\"lines\":26,\"sha256\":\"5d061d829dab6e36d6c1899e2d6f2780fb129ae25efc28fe688d59cc41a6ef5b\",\"newline_count\":26},{\"path\":\"internal/toolproducer/testdata/descriptors/fixture-third-party.json\",\"revision_id\":\"docrev_56004dae0e60bb347860552359ddf446c01bce750b4039e161193f5dce6c598f\",\"lines\":26,\"sha256\":\"e0923229f54efe7396e9eb396c8f51179c56b94a9756b0316fb767411ae7950e\",\"newline_count\":26}]}}"

const guidanceCatalogToolsJSON = "{\"tools\":[{\"aliases\":[],\"artifact_ids\":[\"fixture-cli-payload\"],\"canonical_tool\":\"fixture-cli\",\"capabilities\":[\"fixture-inspect\"],\"id\":\"fixture-cli\",\"kind\":\"cli\",\"platforms\":[\"linux-amd64\",\"linux-arm64\"],\"primary_route\":{\"kind\":\"cli\",\"name\":\"fixture-cli\"},\"purpose\":\"Synthetic CLI inspection declaration.\",\"required\":true},{\"aliases\":[],\"artifact_ids\":[\"fixture-mcp-payload\"],\"canonical_tool\":\"fixture-mcp\",\"capabilities\":[\"fixture-metadata\"],\"id\":\"fixture-mcp\",\"kind\":\"mcp-only\",\"platforms\":[\"linux-amd64\",\"linux-arm64\"],\"primary_route\":{\"kind\":\"mcp\",\"name\":\"fixture-mcp\"},\"purpose\":\"Synthetic MCP metadata declaration.\",\"required\":false},{\"aliases\":[],\"artifact_ids\":[\"fixture-third-party-payload\"],\"canonical_tool\":\"fixture-third-party\",\"capabilities\":[\"fixture-reference\"],\"id\":\"fixture-third-party\",\"kind\":\"third-party\",\"platforms\":[\"linux-amd64\",\"linux-arm64\"],\"primary_route\":{\"kind\":\"external\",\"name\":\"fixture-third-party\"},\"purpose\":\"Synthetic third-party reference declaration.\",\"required\":false},{\"aliases\":[\"repo-fixture\"],\"artifact_ids\":[\"huyang-payload\"],\"canonical_tool\":\"huyang\",\"capabilities\":[\"file-read\",\"file-search\",\"repository-operations\",\"transactional-edit\"],\"id\":\"huyang\",\"kind\":\"mcp-only\",\"platforms\":[\"linux-amd64\",\"linux-arm64\"],\"primary_route\":{\"kind\":\"mcp\",\"name\":\"huyang\"},\"purpose\":\"Repository source operations declaration.\",\"required\":true}]}"
