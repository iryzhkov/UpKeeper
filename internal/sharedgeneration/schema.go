// Package sharedgeneration stores inert candidates in explicit private scratch
// directories. It is deliberately unreachable from production writers.
package sharedgeneration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

const metadataLimit = 256 << 10
const fileLimit = 16 << 20
const aggregateLimit = 64 << 20

var ErrUnsupported = errors.New("sharedgeneration: Linux is required")
var ErrRecoveryRequired = errors.New("sharedgeneration: recovery-required")

type File struct {
	Path               string `json:"path"`
	SHA256             string `json:"sha256"`
	Mode               int    `json:"mode"`
	Kind               string `json:"kind"`
	LogicalDestination string `json:"logical_destination"`
}
type Receipt struct {
	SchemaVersion         int                  `json:"schema_version"`
	State                 string               `json:"state"`
	Owner                 string               `json:"owner"`
	SourceRepository      string               `json:"source_repository"`
	SourceCommit          string               `json:"source_commit"`
	OwnerDescriptorSHA256 string               `json:"owner_descriptor_sha256"`
	SourceSHA256          map[string]string    `json:"source_sha256"`
	Files                 []File               `json:"files"`
	Policy                *author.SharedPolicy `json:"policy,omitempty"`
	// Catalog and Guidance are retained canonical receipt5 objects only.
	Catalog  json.RawMessage `json:"catalog,omitempty"`
	Guidance json.RawMessage `json:"guidance,omitempty"`
}
type Inspection struct {
	GenerationID string
	Receipt      *Receipt
	TrustLabel   string
}
type identity struct {
	SchemaVersion int    `json:"schema_version"`
	State         string `json:"state"`
	Owner         string `json:"owner"`
}

func storeIdentity() identity { return identity{1, "candidate-inert", "upkeeper"} }
func digest(b []byte) string  { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func pin(s string, n int) bool {
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
func validPath(s string) bool {
	if len(s) == 0 || len(s) > 512 || !utf8.ValidString(s) || strings.Contains(s, "\\") {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." || p == ".." || len(p) > 255 {
			return false
		}
	}
	return true
}

// Token validation rejects duplicate keys, excessive nesting and oversized
// containers before typed decoding; canonical readback also rejects alternate
// numeric/string encodings and omitted/null fields.
func strictJSON(b []byte, value any, canonical bool) error {
	return strictJSONLimit(b, value, canonical, metadataLimit)
}
func strictJSONLimit(b []byte, value any, canonical bool, limit int) error {
	if len(b) > limit || !utf8.Valid(b) {
		return errors.New("metadata size or encoding")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 12 {
			return errors.New("metadata depth")
		}
		t, e := d.Token()
		if e != nil {
			return e
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		if delim != '{' && delim != '[' {
			return errors.New("metadata token")
		}
		keys := map[string]bool{}
		n := 0
		for d.More() {
			n++
			if n > 1024 {
				return errors.New("metadata container limit")
			}
			if delim == '{' {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok || keys[s] {
					return errors.New("duplicate metadata key")
				}
				keys[s] = true
			}
			if e := walk(depth + 1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil {
			return e
		}
		if delim == '{' && end != json.Delim('}') || delim == '[' && end != json.Delim(']') {
			return errors.New("metadata delimiter")
		}
		return nil
	}
	if e := walk(1); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing metadata")
	}
	if _, ok := value.(*author.SharedOwner); ok {
		if e := author.ValidateSharedOwnerEncoding(b); e != nil {
			return e
		}
	}
	if receipt, ok := value.(*Receipt); ok {
		var envelope map[string]json.RawMessage
		if e := json.Unmarshal(b, &envelope); e != nil {
			return e
		}
		var version int
		if e := json.Unmarshal(envelope["schema_version"], &version); e != nil {
			return e
		}
		if version == 5 {
			return decodeCatalogReceipt(b, receipt)
		}
		if _, ok := envelope["catalog"]; ok {
			return errors.New("historical receipt catalog field")
		}
		if _, ok := envelope["guidance"]; ok {
			return errors.New("historical receipt guidance field")
		}
		policy, present := envelope["policy"]
		if version == 1 && present || version == 2 && (!present || bytes.Equal(policy, []byte("null"))) {
			return errors.New("receipt policy version")
		}
		if version == 1 && len(envelope) != 8 || version == 2 && len(envelope) != 9 {
			return errors.New("receipt fields")
		}
		for _, k := range []string{"schema_version", "state", "owner", "source_repository", "source_commit", "owner_descriptor_sha256", "source_sha256", "files"} {
			v, ok := envelope[k]
			if !ok || bytes.Equal(v, []byte("null")) {
				return errors.New("missing or null receipt field")
			}
		}
		if version == 2 {
			var fields map[string]json.RawMessage
			if e := json.Unmarshal(policy, &fields); e != nil {
				return e
			}
			keys := []string{"input_manifest_sha256", "input_contract", "canonical_policy_sha256", "policy_format", "derivation", "upstream_repository", "upstream_commit", "upstream_tree", "interpretation", "configured_catalog_validation", "current_authorization", "readiness"}
			if len(fields) != len(keys) {
				return errors.New("policy fields")
			}
			for _, k := range keys {
				v, ok := fields[k]
				if !ok || bytes.Equal(v, []byte("null")) {
					return errors.New("missing or null policy field")
				}
			}
		}
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	d.DisallowUnknownFields()
	if e := d.Decode(value); e != nil {
		return e
	}
	if canonical {
		want, e := canonjson.MarshalFile(value)
		if e != nil {
			return e
		}
		if !bytes.Equal(b, want) {
			return errors.New("noncanonical metadata")
		}
	}
	return nil
}
func validateReceipt(r Receipt) error {
	if r.SchemaVersion == 5 {
		return validateCatalogReceipt(r)
	}
	if len(r.Catalog) != 0 || len(r.Guidance) != 0 {
		return errors.New("historical receipt catalog fields")
	}
	if (r.SchemaVersion != 1 && r.SchemaVersion != 2) || r.State != "candidate-inert" || r.Owner != "upkeeper" ||
		r.SourceRepository != "github.com/iryzhkov/UpKeeper" || !pin(r.SourceCommit, 40) ||
		!pin(r.OwnerDescriptorSHA256, 64) || r.SourceSHA256["owner.json"] != r.OwnerDescriptorSHA256 {
		return errors.New("receipt identity")
	}
	if len(r.SourceSHA256) < 2 || len(r.SourceSHA256) > 1024 || len(r.Files) == 0 || len(r.Files) > 1024 {
		return errors.New("receipt inventory")
	}
	if r.SchemaVersion == 1 && r.Policy != nil || r.SchemaVersion == 2 && !validPolicy(r.Policy) {
		return errors.New("receipt policy")
	}
	if r.SchemaVersion == 2 {
		paths := author.SharedSourcePaths()
		if len(r.SourceSHA256) != len(paths)+1 {
			return errors.New("v2 source closure")
		}
		for _, p := range paths {
			if _, ok := r.SourceSHA256[p]; !ok {
				return errors.New("v2 source closure")
			}
		}
	}
	skills := map[string]bool{}
	instructions := 0
	policyInputs := 0
	mandatory := map[string]bool{}
	if r.SchemaVersion == 2 {
		for _, p := range author.PolicyInstructionPaths() {
			mandatory[p] = true
		}
	}
	for p, h := range r.SourceSHA256 {
		if !validPath(p) || !pin(h, 64) {
			return errors.New("source inventory")
		}
		if strings.HasPrefix(p, "config/claude/skills/") {
			rel := strings.TrimPrefix(p, "config/claude/skills/")
			if !validPath(rel) || !strings.Contains(rel, "/") {
				return errors.New("skill path")
			}
			skills[rel] = true
		} else if p != "owner.json" && p != "config/bin/agents-instructions-gen" && p != "config/claude/CLAUDE.md" && !(r.SchemaVersion == 2 && p == "config/bin/agents-role-project") {
			return errors.New("unknown source")
		}
	}
	if _, ok := r.SourceSHA256["config/bin/agents-instructions-gen"]; !ok {
		return errors.New("missing generator")
	}
	if _, ok := r.SourceSHA256["config/claude/CLAUDE.md"]; !ok {
		return errors.New("missing instruction source")
	}
	prev := ""
	dest := map[string]bool{}
	for _, f := range r.Files {
		if !validPath(f.Path) || !validPath(f.LogicalDestination) || !pin(f.SHA256, 64) || !allowedFileMode(r.SchemaVersion, f) || f.Path <= prev {
			return errors.New("file inventory")
		}
		prev = f.Path
		k := f.Kind + "/" + f.LogicalDestination
		if dest[k] {
			return errors.New("duplicate destination")
		}
		dest[k] = true
		switch f.Kind {
		case "instruction":
			if f.Path != "instructions/"+f.LogicalDestination {
				return errors.New("instruction path")
			}
			if r.SchemaVersion == 2 {
				if !mandatory[f.LogicalDestination] {
					return errors.New("unknown instruction")
				}
				delete(mandatory, f.LogicalDestination)
			}
			instructions++
		case "skill":
			if f.Path != "skills/"+f.LogicalDestination || !skills[f.LogicalDestination] ||
				f.SHA256 != r.SourceSHA256["config/claude/skills/"+f.LogicalDestination] {
				return errors.New("skill inventory")
			}
			delete(skills, f.LogicalDestination)
		case "policy-input":
			if r.SchemaVersion != 2 || f.Path != "inputs/route-policy.json" || f.LogicalDestination != "route-policy" || f.SHA256 != r.Policy.CanonicalPolicySHA256 {
				return errors.New("policy input inventory")
			}
			policyInputs++
		default:
			return errors.New("file kind")
		}
	}
	if len(skills) != 0 || instructions == 0 || r.SchemaVersion == 2 && (len(mandatory) != 0 || policyInputs != 1 || r.SourceSHA256["config/bin/agents-role-project"] == "") {
		return errors.New("incomplete closure")
	}
	return nil
}
func sortedKeys[V any](m map[string]V) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}
func recovery(e error) error { return fmt.Errorf("%w: %v", ErrRecoveryRequired, e) }
