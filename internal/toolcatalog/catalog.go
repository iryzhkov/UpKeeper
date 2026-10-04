// Package toolcatalog inspects operator-pinned candidate declarations, never runtime authority.
package toolcatalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const metadataLimit = 256 << 10
const fileLimit int64 = 256 << 20
const aggregateLimit int64 = 1 << 30

type Options struct{ Metadata, Root, SHA256, Platform string }
type Error struct {
	Code  string `json:"code"`
	Stage string `json:"stage"`
}

func (e *Error) Error() string    { return e.Code + ": " + e.Stage }
func refusal(stage string) *Error { return &Error{"verification-refused", stage} }

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var hexPattern = regexp.MustCompile(`^[0-9a-f]+$`)

func Pin(s string, n int) bool { return len(s) == n && hexPattern.MatchString(s) }
func Platform(s string) bool   { return s == "linux-amd64" || s == "linux-arm64" }
func digest(b []byte) string   { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type Source struct {
	ID           string `json:"id"`
	Repository   string `json:"repository"`
	RevisionKind string `json:"revision_kind"`
	Revision     string `json:"revision"`
}
type Artifact struct {
	ID       string `json:"id"`
	SourceID string `json:"source_id"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Mode     int    `json:"mode"`
	Required bool   `json:"required"`
}
type Route struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type Tool struct {
	ID            string   `json:"id"`
	CanonicalTool string   `json:"canonical_tool"`
	Aliases       []string `json:"aliases"`
	Kind          string   `json:"kind"`
	Purpose       string   `json:"purpose"`
	Capabilities  []string `json:"capabilities"`
	PrimaryRoute  Route    `json:"primary_route"`
	ArtifactIDs   []string `json:"artifact_ids"`
	Required      bool     `json:"required"`
	Platforms     []string `json:"platforms"`
}
type document struct {
	SchemaVersion int        `json:"schema_version"`
	State         string     `json:"state"`
	Sources       []Source   `json:"sources"`
	Artifacts     []Artifact `json:"artifacts"`
	Tools         []Tool     `json:"tools"`
}

// tokenValue rejects duplicates before typed decoding, including nested objects.
func tokenValue(d *json.Decoder, depth int) (any, error) {
	if depth > 12 {
		return nil, refusal("schema")
	}
	t, e := d.Token()
	if e != nil {
		return nil, refusal("schema")
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, refusal("schema")
				}
				s, ok := k.(string)
				if !ok {
					return nil, refusal("schema")
				}
				if _, ok = m[s]; ok {
					return nil, refusal("schema")
				}
				v, e := tokenValue(d, depth+1)
				if e != nil {
					return nil, e
				}
				m[s] = v
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return nil, refusal("schema")
			}
			return m, nil
		case '[':
			a := []any{}
			for d.More() {
				if len(a) >= 1024 {
					return nil, refusal("schema")
				}
				v, e := tokenValue(d, depth+1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return nil, refusal("schema")
			}
			return a, nil
		}
		return nil, refusal("schema")
	}
	if s, ok := t.(string); ok && !safeString(s) {
		return nil, refusal("schema")
	}
	return t, nil
}
func safeString(s string) bool {
	return utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}
func fields(v any, spec map[string]string) bool {
	m, ok := v.(map[string]any)
	if !ok || len(m) != len(spec) {
		return false
	}
	for k, typ := range spec {
		v, ok := m[k]
		if !ok {
			return false
		}
		switch typ {
		case "s":
			if _, ok := v.(string); !ok {
				return false
			}
		case "n":
			if _, ok := v.(json.Number); !ok {
				return false
			}
		case "b":
			if _, ok := v.(bool); !ok {
				return false
			}
		case "a":
			if _, ok := v.([]any); !ok {
				return false
			}
		case "o":
			if _, ok := v.(map[string]any); !ok {
				return false
			}
		}
	}
	return true
}
func decode(raw []byte) (*document, error) {
	if !utf8.Valid(raw) || len(raw) > metadataLimit {
		return nil, refusal("schema")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, e := tokenValue(d, 0)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, refusal("schema")
	}
	if !fields(v, map[string]string{"schema_version": "n", "state": "s", "sources": "a", "artifacts": "a", "tools": "a"}) {
		return nil, refusal("schema")
	}
	m := v.(map[string]any)
	specs := map[string]map[string]string{
		"sources":   {"id": "s", "repository": "s", "revision_kind": "s", "revision": "s"},
		"artifacts": {"id": "s", "source_id": "s", "path": "s", "sha256": "s", "mode": "n", "required": "b"},
		"tools":     {"id": "s", "canonical_tool": "s", "aliases": "a", "kind": "s", "purpose": "s", "capabilities": "a", "primary_route": "o", "artifact_ids": "a", "required": "b", "platforms": "a"},
	}
	for name, spec := range specs {
		a := m[name].([]any)
		if name == "sources" && len(a) > 128 || name == "tools" && len(a) > 256 {
			return nil, refusal("schema")
		}
		for _, v := range a {
			if !fields(v, spec) {
				return nil, refusal("schema")
			}
			if name == "tools" && !fields(v.(map[string]any)["primary_route"], map[string]string{"kind": "s", "name": "s"}) {
				return nil, refusal("schema")
			}
		}
	}
	var doc document
	typed := json.NewDecoder(bytes.NewReader(raw))
	typed.DisallowUnknownFields()
	if typed.Decode(&doc) != nil {
		return nil, refusal("schema")
	}
	if validate(&doc) != nil {
		return nil, refusal("schema")
	}
	return &doc, nil
}
func validPath(p string) bool {
	if p == "" || len(p) > 512 || strings.Contains(p, "\\") || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." || len(s) > 255 {
			return false
		}
	}
	return safeString(p)
}
func unique(list []string, valid func(string) bool, nonempty bool) bool {
	if nonempty && len(list) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, s := range list {
		if !valid(s) || seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}
func validate(d *document) error {
	bad := refusal("schema")
	if d.SchemaVersion != 1 || d.State != "candidate-inert" {
		return bad
	}
	sources := map[string]bool{}
	for _, s := range d.Sources {
		u, e := url.Parse(s.Repository)
		if !idPattern.MatchString(s.ID) || sources[s.ID] || e != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.Path == "" || u.Path == "/" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != s.Repository || strings.Contains(s.Repository, "#") {
			return bad
		}
		if s.RevisionKind == "git-sha1" {
			if !Pin(s.Revision, 40) {
				return bad
			}
		} else if s.RevisionKind == "content-sha256" {
			if !Pin(s.Revision, 64) {
				return bad
			}
		} else {
			return bad
		}
		sources[s.ID] = true
	}
	artifacts := map[string]Artifact{}
	paths := map[string]bool{}
	for _, a := range d.Artifacts {
		if !idPattern.MatchString(a.ID) || !sources[a.SourceID] || !validPath(a.Path) || !Pin(a.SHA256, 64) || a.Mode < 0 || a.Mode > 511 || paths[a.Path] {
			return bad
		}
		if _, ok := artifacts[a.ID]; ok {
			return bad
		}
		artifacts[a.ID] = a
		paths[a.Path] = true
	}
	owners := map[string]int{}
	routes := map[Route]bool{}
	used := map[string]bool{}
	for i, t := range d.Tools {
		names := append([]string{t.ID, t.CanonicalTool}, t.Aliases...)
		if !unique(t.Aliases, idPattern.MatchString, false) {
			return bad
		}
		for _, s := range names {
			if !idPattern.MatchString(s) || s == "agent99" {
				return bad
			}
			if owner, ok := owners[s]; ok && owner != i {
				return bad
			}
			owners[s] = i
		}
		// Only id=canonical_tool may repeat within one owner.
		for _, a := range t.Aliases {
			if a == t.ID || a == t.CanonicalTool {
				return bad
			}
		}
		if !idPattern.MatchString(t.PrimaryRoute.Name) || t.PrimaryRoute.Name == "agent99" || routes[t.PrimaryRoute] {
			return bad
		}
		routes[t.PrimaryRoute] = true
		switch t.Kind {
		case "cli":
			if t.PrimaryRoute.Kind != "cli" {
				return bad
			}
		case "mcp-only":
			if t.PrimaryRoute.Kind != "mcp" {
				return bad
			}
		case "third-party":
			if t.PrimaryRoute.Kind != "cli" && t.PrimaryRoute.Kind != "mcp" && t.PrimaryRoute.Kind != "external" {
				return bad
			}
		default:
			return bad
		}
		if t.Purpose == "" || len(t.Purpose) > 512 || !unique(t.Capabilities, idPattern.MatchString, true) || !unique(t.Platforms, Platform, true) || !unique(t.ArtifactIDs, idPattern.MatchString, true) {
			return bad
		}
		for _, c := range t.Capabilities {
			if c == "repository-operations" && t.CanonicalTool != "huyang" {
				return bad
			}
		}
		for _, id := range t.ArtifactIDs {
			a, ok := artifacts[id]
			if !ok || t.Required && !a.Required {
				return bad
			}
			used[id] = true
		}
	}
	if len(used) != len(artifacts) {
		return bad
	}
	return nil
}
func common() map[string]any {
	return map[string]any{
		"schema_version": 1, "command": "environment-tools", "catalog_mode": "candidate-inspection", "generation_state": "candidate-inert", "trust_mode": "operator-sha256-pin", "sources_provenance": "declaration-only", "observation_scope": "per-file-no-atomic-snapshot",
	}
}
func Failure(err *Error, pin string) map[string]any {
	r := common()
	r["result"] = "invalid-input"
	r["exit_code"] = 30
	r["error"] = err
	if pin != "" {
		r["generation_sha256"] = pin
	}
	return r
}
func success(d *document, pin, platform string, observations []map[string]any, missing map[string]bool) map[string]any {
	r := common()
	r["result"] = "content-verified"
	r["exit_code"] = 0
	r["generation_sha256"] = pin
	r["sources"] = d.Sources
	r["artifacts"] = observations
	tools := []map[string]any{}
	for _, t := range d.Tools {
		content := "verified"
		for _, id := range t.ArtifactIDs {
			if missing[id] {
				content = "optional-missing"
			}
		}
		compat := "unassessed"
		if platform != "" {
			compat = "incompatible"
			for _, p := range t.Platforms {
				if p == platform {
					compat = "compatible"
				}
			}
		}
		o := map[string]any{"declaration": t, "selected": "declared", "content_state": content, "installed": "unknown", "configured": "unknown", "authorized": "unknown", "enforcement": "unqualified", "readiness": "unassessed", "platform_compatibility": compat}
		if platform != "" {
			o["requested_platform"] = platform
		}
		tools = append(tools, o)
	}
	r["tools"] = tools
	return r
}
