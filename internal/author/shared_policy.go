package author

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

const sharedUpstreamCommit = "33cb805b5482d511071be7b344140e2ccb8c87da"
const sharedUpstreamTree = "3439bd874573cb88bbad852c5b66771f2ab80eeb"
const sharedInputContract = "canonical-release-manifest-v1"
const sharedManifestLimit = 16 << 20

// SharedPolicyOptions binds an explicit local declaration and source object.
type SharedPolicyOptions struct {
	Repository                    string
	Commit                        string
	ReleaseManifestPath           string
	ExpectedReleaseManifestSHA256 string
}

// SharedPolicy records completed derivation, never a current authority grant.
type SharedPolicy struct {
	InputManifestSHA256         string `json:"input_manifest_sha256"`
	InputContract               string `json:"input_contract"`
	CanonicalPolicySHA256       string `json:"canonical_policy_sha256"`
	PolicyFormat                string `json:"policy_format"`
	Derivation                  string `json:"derivation"`
	UpstreamRepository          string `json:"upstream_repository"`
	UpstreamCommit              string `json:"upstream_commit"`
	UpstreamTree                string `json:"upstream_tree"`
	Interpretation              string `json:"interpretation"`
	ConfiguredCatalogValidation string `json:"configured_catalog_validation"`
	CurrentAuthorization        string `json:"current_authorization"`
	Readiness                   string `json:"readiness"`
}

type sharedPolicyInput struct {
	policy []byte
	digest string
	verify func() error
}

// PolicyInstructionPaths returns the closed mandatory inventory without sharing its backing array.
func PolicyInstructionPaths() []string { return append([]string(nil), policyInstructions...) }

// SharedSourcePaths is the frozen v2 copied source closure, excluding owner.json.
func SharedSourcePaths() []string {
	return []string{
		"config/bin/agents-instructions-gen", "config/bin/agents-role-project", "config/claude/CLAUDE.md",
		"config/claude/skills/huyang/SKILL.md", "config/claude/skills/implementation-session/SKILL.md",
		"config/claude/skills/implementation-session/references/prompt-template.md",
		"config/claude/skills/ov-memory-curation/SKILL.md", "config/claude/skills/ov-memory/SKILL.md",
		"config/claude/skills/publish-agent-tooling/SKILL.md", "config/claude/skills/t3-campaign/SKILL.md",
		"config/claude/skills/t3-campaign/references/executor-briefs.md",
		"config/claude/skills/t3-task/SKILL.md", "config/claude/skills/t3-wait/SKILL.md",
	}
}

// ValidateSharedOwnerEncoding enforces the version-specific closed wire shape;
// identity checks remain with preparation and store validation.
func ValidateSharedOwnerEncoding(raw []byte) error {
	if len(raw) == 0 || len(raw) > 256<<10 || !utf8.Valid(raw) {
		return errors.New("shared owner size or encoding")
	}
	if err := boundedSharedJSON(raw, 12, 1024); err != nil {
		return err
	}
	value, err := canonjson.DecodeObject(raw)
	if err != nil {
		return err
	}
	version := value["schema_version"]
	keys := []string{"schema_version", "owner", "state", "source_repository", "legacy_source_repository", "legacy_source_commit", "generator", "source_files", "instruction_files", "skill_roots", "future_merges", "retired_reference"}
	if version == json.Number("2") {
		keys = append(keys, "policy_input_contract", "legacy_source_tree")
	} else if version != json.Number("1") {
		return errors.New("shared owner version")
	}
	if len(value) != len(keys) {
		return errors.New("shared owner fields")
	}
	for _, key := range keys {
		if value[key] == nil {
			return errors.New("shared owner missing or null field")
		}
	}
	return nil
}

func sharedHex(s string, size int) bool {
	if len(s) != size {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func sharedPhysicalPath(s string) bool {
	if !filepath.IsAbs(s) || s == "/" || filepath.Clean(s) != s || strings.Contains(s, "\\") || len(s) > 4096 || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	for _, p := range strings.Split(strings.TrimPrefix(s, "/"), "/") {
		if p == "" || len(p) > 255 {
			return false
		}
	}
	return true
}

// PrepareSharedWithPolicy validates through the owning release/fleet APIs in
// normal configured context. Only derivation receives a private HOME/TMPDIR.
func PrepareSharedWithPolicy(o SharedPolicyOptions) (*SharedPreparation, error) {
	if !sharedPhysicalPath(o.Repository) || !sharedPhysicalPath(o.ReleaseManifestPath) ||
		!sharedHex(o.Commit, 40) || !sharedHex(o.ExpectedReleaseManifestSHA256, 64) {
		return nil, errors.New("shared policy requires absolute physical paths and full lowercase pins")
	}
	verifyRepository, closeRepository, err := acquireSharedRepository(o.Repository)
	if err != nil {
		return nil, err
	}
	defer closeRepository()
	raw, verify, closeInput, err := acquireSharedManifest(o.ReleaseManifestPath)
	if err != nil {
		return nil, err
	}
	defer closeInput()
	if digestOf(raw) != o.ExpectedReleaseManifestSHA256 {
		return nil, errors.New("shared manifest SHA256 mismatch")
	}
	manifest, err := decodeSharedManifest(raw)
	if err != nil {
		return nil, err
	}
	validated, err := release.Validate(manifest)
	if err != nil {
		return nil, err
	}
	document, present, err := validated.FleetIntent()
	if err != nil {
		return nil, err
	}
	if !present || document == nil {
		return nil, errors.New("shared manifest requires fleet policy")
	}
	policy := document.RoutePolicyBytes()
	if len(policy) == 0 || len(policy) > fleet.RoutePolicyMaxBytes {
		return nil, errors.New("shared canonical policy missing or oversized")
	}
	if err := verify(); err != nil {
		return nil, err
	}
	return prepareShared(o.Repository, o.Commit, &sharedPolicyInput{policy: policy, digest: o.ExpectedReleaseManifestSHA256, verify: func() error {
		if err := verifyRepository(); err != nil {
			return err
		}
		return verify()
	}})
}

func decodeSharedManifest(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > sharedManifestLimit || !utf8.Valid(raw) {
		return nil, errors.New("shared manifest size or encoding")
	}
	if err := boundedSharedJSON(raw, 64, 8192); err != nil {
		return nil, err
	}
	value, err := canonjson.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	if len(value) != 4 || value["schema_version"] != json.Number("1") || value["captured_at"] == nil || value["captured_from"] == nil || value["components"] == nil {
		return nil, errors.New("shared manifest envelope")
	}
	for key := range value {
		if key != "schema_version" && key != "captured_at" && key != "captured_from" && key != "components" {
			return nil, errors.New("shared manifest envelope key")
		}
	}
	encoded, err := canonjson.MarshalFile(value)
	if err != nil || !bytes.Equal(encoded, raw) {
		return nil, errors.New("shared manifest is not canonical")
	}
	return value, nil
}

func boundedSharedJSON(raw []byte, maxDepth, maxContainer int) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > maxDepth {
			return errors.New("shared manifest depth")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		if delimiter != '{' && delimiter != '[' {
			return errors.New("shared manifest delimiter")
		}
		keys := map[string]bool{}
		count := 0
		for d.More() {
			count++
			if count > maxContainer {
				return errors.New("shared manifest container bound")
			}
			if delimiter == '{' {
				token, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || keys[key] {
					return errors.New("shared manifest duplicate key")
				}
				keys[key] = true
			}
			if err := walk(depth + 1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil {
			return err
		}
		if delimiter == '{' && end != json.Delim('}') || delimiter == '[' && end != json.Delim(']') {
			return errors.New("shared manifest delimiter")
		}
		return nil
	}
	if err := walk(1); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("shared JSON trailing data")
	}
	return nil
}

func prepareSharedPolicy(p *SharedPreparation, runner procx.Runner, source, home string, input *sharedPolicyInput) (*SharedPreparation, error) {
	want := PolicyInstructionPaths()
	sort.Strings(want)
	if !reflect.DeepEqual(want, p.Owner.InstructionFiles) {
		return nil, errors.New("shared policy instruction inventory")
	}
	if err := validateSharedSource(p, source); err != nil {
		return nil, err
	}
	before, err := treeState(source)
	if err != nil {
		return nil, err
	}
	temporary := filepath.Join(p.scratch, "tmp")
	if err := os.Mkdir(temporary, 0700); err != nil {
		return nil, err
	}
	derived, err := derivePolicyRoot(runner, p.scratch, source, home, temporary, input.policy, true)
	if err != nil {
		return nil, err
	}
	// Refuse any additional output: the derivation HOME has exactly the 18 files.
	actual := []string{}
	err = filepath.WalkDir(home, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		actual = append(actual, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, want) {
		return nil, errors.New("shared policy unexpected generated output")
	}
	after, err := treeState(source)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(before, after) {
		return nil, errors.New("shared source changed during derivation")
	}
	if err := input.verify(); err != nil {
		return nil, err
	}
	p.Sources = derived
	p.PolicyBytes = append([]byte(nil), input.policy...)
	p.Policy = &SharedPolicy{input.digest, sharedInputContract, digestOf(input.policy), "route-policy/v1-canonical-json",
		"author-derive-policy-v1", "github.com/iryzhkov/omarchy-setup", sharedUpstreamCommit, sharedUpstreamTree,
		"steward-parser-and-pinned-generator", "completed-at-preparation", "unknown", "unassessed"}
	for path, file := range derived.Instructions {
		p.OutputSHA256[path] = digestOf(file.Content)
	}
	return p, nil
}

func validateSharedSource(p *SharedPreparation, source string) error {
	if !reflect.DeepEqual(p.Owner.SourceFiles, SharedSourcePaths()) {
		return errors.New("shared v2 source inventory")
	}
	if len(p.SourceSHA256) < 4 || len(p.SourceSHA256) > 1024 || len(p.Owner.SourceFiles) > 1023 {
		return errors.New("shared source bound")
	}
	var total int64
	for path, hash := range p.SourceSHA256 {
		if path != "owner.json" && path != Generator && path != "config/bin/agents-role-project" && path != "config/claude/CLAUDE.md" && !strings.HasPrefix(path, "config/claude/skills/") {
			return errors.New("shared source unknown path")
		}
		if path != "owner.json" && path != Generator && path != "config/bin/agents-role-project" && path != "config/claude/CLAUDE.md" {
			relative := strings.TrimPrefix(path, "config/claude/skills/")
			if !strings.Contains(relative, "/") {
				return errors.New("shared source skill path")
			}
		}
		info, err := regularOutput(source, path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0644)
		limit := int64(16 << 20)
		if path == Generator || path == "config/bin/agents-role-project" {
			mode = 0755
		}
		if path == "owner.json" {
			limit = 256 << 10
		}
		if info.Mode().Perm() != mode || info.Size() <= 0 || info.Size() > limit {
			return errors.New("shared source mode or size")
		}
		total += info.Size()
		if total > 64<<20 {
			return errors.New("shared source aggregate")
		}
		content, err := os.ReadFile(filepath.Join(source, path))
		if err != nil || digestOf(content) != hash {
			return errors.New("shared source binding")
		}
	}
	for _, path := range []string{Generator, "config/bin/agents-role-project", "config/claude/CLAUDE.md"} {
		if _, ok := p.SourceSHA256[path]; !ok {
			return errors.New("shared source missing required path")
		}
	}
	return nil
}
