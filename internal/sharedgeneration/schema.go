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
	SchemaVersion         int               `json:"schema_version"`
	State                 string            `json:"state"`
	Owner                 string            `json:"owner"`
	SourceRepository      string            `json:"source_repository"`
	SourceCommit          string            `json:"source_commit"`
	OwnerDescriptorSHA256 string            `json:"owner_descriptor_sha256"`
	SourceSHA256          map[string]string `json:"source_sha256"`
	Files                 []File            `json:"files"`
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
	if len(b) > metadataLimit || !utf8.Valid(b) {
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
	d = json.NewDecoder(bytes.NewReader(b))
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
	if r.SchemaVersion != 1 || r.State != "candidate-inert" || r.Owner != "upkeeper" ||
		r.SourceRepository != "github.com/iryzhkov/UpKeeper" || !pin(r.SourceCommit, 40) ||
		!pin(r.OwnerDescriptorSHA256, 64) || r.SourceSHA256["owner.json"] != r.OwnerDescriptorSHA256 {
		return errors.New("receipt identity")
	}
	if len(r.SourceSHA256) < 2 || len(r.SourceSHA256) > 1024 || len(r.Files) == 0 || len(r.Files) > 1024 {
		return errors.New("receipt inventory")
	}
	skills := map[string]bool{}
	instructions := 0
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
		} else if p != "owner.json" && p != "config/bin/agents-instructions-gen" && p != "config/claude/CLAUDE.md" {
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
		if !validPath(f.Path) || !validPath(f.LogicalDestination) || !pin(f.SHA256, 64) || f.Mode != 0644 || f.Path <= prev {
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
			instructions++
		case "skill":
			if f.Path != "skills/"+f.LogicalDestination || !skills[f.LogicalDestination] ||
				f.SHA256 != r.SourceSHA256["config/claude/skills/"+f.LogicalDestination] {
				return errors.New("skill inventory")
			}
			delete(skills, f.LogicalDestination)
		default:
			return errors.New("file kind")
		}
	}
	if len(skills) != 0 || instructions == 0 {
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
