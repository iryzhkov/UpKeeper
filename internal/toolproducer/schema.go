// Package toolproducer compiles pinned offline declarations into inert candidates.
package toolproducer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/toolcatalog"
)

type Inputs struct {
	InputPath, ExpectedInputSHA256, ManifestPath, ExpectedManifestSHA256 string
	DescriptorRoot, ArtifactRoot, ProducerRepository, ProducerCommit     string
	SourceRepositories                                                   map[string]string
}
type Result struct {
	CatalogBytes, ReceiptBytes   []byte
	CatalogSHA256, ReceiptSHA256 string
}
type descriptorPin struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	SourceID   string `json:"source_id"`
	SourcePath string `json:"source_path"`
}
type inputDocument struct {
	SchemaVersion  int             `json:"schema_version"`
	State          string          `json:"state"`
	Platform       string          `json:"platform"`
	ManifestSHA256 string          `json:"artifact_manifest_sha256"`
	Descriptors    []descriptorPin `json:"descriptors"`
	ToolIDs        []string        `json:"tool_ids"`
}
type sourceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   int    `json:"mode"`
}
type source struct {
	ID         string       `json:"id"`
	Repository string       `json:"repository"`
	Commit     string       `json:"commit"`
	Tree       string       `json:"tree"`
	Files      []sourceFile `json:"files"`
}
type artifact struct {
	ID         string `json:"id"`
	SourceID   string `json:"source_id"`
	SourcePath string `json:"source_path"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Mode       int    `json:"mode"`
	Required   bool   `json:"required"`
	Transform  string `json:"transform"`
}
type manifest struct {
	SchemaVersion int        `json:"schema_version"`
	State         string     `json:"state"`
	Sources       []source   `json:"sources"`
	Artifacts     []artifact `json:"artifacts"`
}
type descriptor struct {
	SchemaVersion int              `json:"schema_version"`
	State         string           `json:"state"`
	Tool          toolcatalog.Tool `json:"tool"`
}
type decision struct {
	ToolID string `json:"tool_id"`
	Result string `json:"result"`
}
type producerSource struct {
	Repository string       `json:"repository"`
	Commit     string       `json:"commit"`
	Tree       string       `json:"tree"`
	Files      []sourceFile `json:"files"`
}
type receipt struct {
	SchemaVersion  int             `json:"schema_version"`
	State          string          `json:"state"`
	ProducerSource producerSource  `json:"producer_source"`
	InputSHA256    string          `json:"input_sha256"`
	ManifestSHA256 string          `json:"artifact_manifest_sha256"`
	Descriptors    []descriptorPin `json:"descriptors"`
	Sources        []source        `json:"sources"`
	Artifacts      []artifact      `json:"artifacts"`
	CatalogSHA256  string          `json:"catalog_sha256"`
	Platform       string          `json:"platform"`
	Decisions      []decision      `json:"decisions"`
}
type catalog struct {
	SchemaVersion int                    `json:"schema_version"`
	State         string                 `json:"state"`
	Sources       []toolcatalog.Source   `json:"sources"`
	Artifacts     []toolcatalog.Artifact `json:"artifacts"`
	Tools         []toolcatalog.Tool     `json:"tools"`
}

const metadataLimit = 262144
const fileLimit = 16 << 20
const aggregateLimit = 64 << 20

func refuse(stage string) error { return fmt.Errorf("toolproducer-refused: %s", stage) }
func digest(b []byte) string    { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

var idRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var integerRE = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

func safe(s string) bool { return utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0 }
func relative(p string) bool {
	if len(p) == 0 || len(p) > 512 || !safe(p) || strings.Contains(p, "\\") || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." || len(s) > 255 {
			return false
		}
	}
	return true
}
func repository(s string) bool {
	u, e := url.Parse(s)
	return e == nil && len(s) <= 512 && safe(s) && u.Scheme == "https" && u.Host != "" && u.Hostname() != "" && u.Port() == "" && !strings.Contains(u.Host, ":") && u.User == nil && u.Path != "" && u.Path != "/" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.Contains(s, "#") && u.String() == s
}
func sorted[T any](a []T, key func(T) string) bool {
	for i := 1; i < len(a); i++ {
		if key(a[i-1]) >= key(a[i]) {
			return false
		}
	}
	return true
}
func sortedStrings(a []string) bool { return sorted(a, func(s string) string { return s }) }

// The accepted Stage A closed wire schemas are data, with no runtime discovery.
const wireSchemas = `{"producer_input_v1":{"additionalProperties":false,"properties":{"artifact_manifest_sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"descriptors":{"items":{"additionalProperties":false,"properties":{"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"path":{"maxLength":512,"minLength":1,"type":"string"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"source_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"source_path":{"maxLength":512,"minLength":1,"type":"string"}},"required":["id","path","sha256","source_id","source_path"],"type":"object"},"maxItems":4,"minItems":1,"type":"array","uniqueItems":true},"platform":{"enum":["linux-amd64","linux-arm64"]},"schema_version":{"const":1,"type":"integer"},"state":{"const":"candidate-inert","type":"string"},"tool_ids":{"items":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"maxItems":4,"minItems":1,"type":"array","uniqueItems":true}},"required":["schema_version","state","platform","artifact_manifest_sha256","descriptors","tool_ids"],"type":"object"},"descriptor_v1":{"additionalProperties":false,"properties":{"schema_version":{"const":1,"type":"integer"},"state":{"const":"candidate-inert","type":"string"},"tool":{"additionalProperties":false,"properties":{"aliases":{"items":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"maxItems":4,"minItems":0,"type":"array","uniqueItems":true},"artifact_ids":{"items":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true},"canonical_tool":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"capabilities":{"items":{"enum":["repository-operations","file-read","file-search","transactional-edit","fixture-inspect","fixture-metadata","fixture-reference"]},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true},"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"kind":{"enum":["cli","mcp-only","third-party"]},"platforms":{"items":{"enum":["linux-amd64","linux-arm64"]},"maxItems":2,"minItems":1,"type":"array","uniqueItems":true},"primary_route":{"additionalProperties":false,"properties":{"kind":{"enum":["cli","mcp","external"]},"name":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"}},"required":["kind","name"],"type":"object"},"purpose":{"maxLength":160,"minLength":1,"type":"string"},"required":{"type":"boolean"}},"required":["id","canonical_tool","aliases","kind","purpose","capabilities","primary_route","artifact_ids","required","platforms"],"type":"object"}},"required":["schema_version","state","tool"],"type":"object"},"artifact_manifest_v1":{"additionalProperties":false,"properties":{"artifacts":{"items":{"additionalProperties":false,"properties":{"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"mode":{"enum":[420,493],"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"required":{"type":"boolean"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"source_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"source_path":{"maxLength":512,"minLength":1,"type":"string"},"transform":{"const":"identity-v1"}},"required":["id","source_id","source_path","path","sha256","mode","required","transform"],"type":"object"},"maxItems":32,"minItems":1,"type":"array","uniqueItems":true},"schema_version":{"const":1,"type":"integer"},"sources":{"items":{"additionalProperties":false,"properties":{"commit":{"pattern":"^[0-9a-f]{40}$","type":"string"},"files":{"items":{"additionalProperties":false,"properties":{"mode":{"enum":[420,493],"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"}},"required":["path","sha256","mode"],"type":"object"},"maxItems":32,"minItems":1,"type":"array","uniqueItems":true},"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"repository":{"pattern":"^https://[^/?#@]+/[^?#]+$","type":"string"},"tree":{"pattern":"^[0-9a-f]{40}$","type":"string"}},"required":["id","repository","commit","tree","files"],"type":"object"},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true},"state":{"const":"candidate-inert","type":"string"}},"required":["schema_version","state","sources","artifacts"],"type":"object"},"producer_receipt_v1":{"additionalProperties":false,"properties":{"artifact_manifest_sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"artifacts":{"items":{"additionalProperties":false,"properties":{"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"mode":{"enum":[420,493],"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"required":{"type":"boolean"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"source_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"source_path":{"maxLength":512,"minLength":1,"type":"string"},"transform":{"const":"identity-v1"}},"required":["id","source_id","source_path","path","sha256","mode","required","transform"],"type":"object"},"maxItems":32,"minItems":1,"type":"array","uniqueItems":true},"catalog_sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"decisions":{"items":{"additionalProperties":false,"properties":{"result":{"enum":["verified","optional-missing","optional-incompatible"]},"tool_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"}},"required":["tool_id","result"],"type":"object"},"maxItems":4,"minItems":1,"type":"array","uniqueItems":true},"descriptors":{"items":{"additionalProperties":false,"properties":{"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"path":{"maxLength":512,"minLength":1,"type":"string"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"source_id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"source_path":{"maxLength":512,"minLength":1,"type":"string"}},"required":["id","path","sha256","source_id","source_path"],"type":"object"},"maxItems":4,"minItems":1,"type":"array","uniqueItems":true},"input_sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"},"platform":{"enum":["linux-amd64","linux-arm64"]},"producer_source":{"additionalProperties":false,"properties":{"commit":{"pattern":"^[0-9a-f]{40}$","type":"string"},"files":{"items":{"additionalProperties":false,"properties":{"mode":{"enum":[420,493],"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"}},"required":["path","sha256","mode"],"type":"object"},"maxItems":32,"minItems":1,"type":"array","uniqueItems":true},"repository":{"const":"https://github.com/iryzhkov/UpKeeper"},"tree":{"pattern":"^[0-9a-f]{40}$","type":"string"}},"required":["repository","commit","tree","files"],"type":"object"},"schema_version":{"const":1,"type":"integer"},"sources":{"items":{"additionalProperties":false,"properties":{"commit":{"pattern":"^[0-9a-f]{40}$","type":"string"},"files":{"items":{"additionalProperties":false,"properties":{"mode":{"enum":[420,493],"type":"integer"},"path":{"maxLength":512,"minLength":1,"type":"string"},"sha256":{"pattern":"^[0-9a-f]{64}$","type":"string"}},"required":["path","sha256","mode"],"type":"object"},"maxItems":32,"minItems":1,"type":"array","uniqueItems":true},"id":{"pattern":"^[a-z][a-z0-9-]{0,63}$","type":"string"},"repository":{"pattern":"^https://[^/?#@]+/[^?#]+$","type":"string"},"tree":{"pattern":"^[0-9a-f]{40}$","type":"string"}},"required":["id","repository","commit","tree","files"],"type":"object"},"maxItems":8,"minItems":1,"type":"array","uniqueItems":true},"state":{"const":"candidate-inert","type":"string"}},"required":["schema_version","state","producer_source","input_sha256","artifact_manifest_sha256","descriptors","sources","artifacts","catalog_sha256","platform","decisions"],"type":"object"}}`

// walkJSON bounds nesting and containers and rejects recursive duplicate keys.
func walkJSON(d *json.Decoder, depth int) (any, error) {
	if depth > 12 {
		return nil, refuse("schema-depth")
	}
	t, e := d.Token()
	if e != nil {
		return nil, e
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			m := map[string]any{}
			for d.More() {
				if len(m) >= 1024 {
					return nil, refuse("schema-container")
				}
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				s, ok := k.(string)
				if !ok || !safe(s) {
					return nil, refuse("schema-key")
				}
				if _, ok = m[s]; ok {
					return nil, refuse("schema-duplicate")
				}
				v, e := walkJSON(d, depth+1)
				if e != nil {
					return nil, e
				}
				m[s] = v
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return nil, refuse("schema-object")
			}
			return m, nil
		case '[':
			a := []any{}
			for d.More() {
				if len(a) >= 1024 {
					return nil, refuse("schema-container")
				}
				v, e := walkJSON(d, depth+1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return nil, refuse("schema-array")
			}
			return a, nil
		}
		return nil, refuse("schema-delimiter")
	}
	if s, ok := t.(string); ok && !safe(s) {
		return nil, refuse("schema-string")
	}
	if n, ok := t.(json.Number); ok && !integerRE.MatchString(n.String()) {
		return nil, refuse("schema-integer")
	}
	if t == nil {
		return nil, refuse("schema-null")
	}
	return t, nil
}
func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func validateShape(v any, s map[string]any) bool {
	if c, ok := s["const"]; ok && !sameJSON(v, c) {
		return false
	}
	if a, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range a {
			found = found || sameJSON(v, x)
		}
		if !found {
			return false
		}
	}
	typ, _ := s["type"].(string)
	switch typ {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		props := s["properties"].(map[string]any)
		required := s["required"].([]any)
		if len(m) != len(required) {
			return false
		}
		for _, k := range required {
			key := k.(string)
			val, ok := m[key]
			if !ok || !validateShape(val, props[key].(map[string]any)) {
				return false
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return false
		}
		if n, ok := s["minItems"].(float64); ok && len(a) < int(n) {
			return false
		}
		if n, ok := s["maxItems"].(float64); ok && len(a) > int(n) {
			return false
		}
		for i, x := range a {
			if !validateShape(x, s["items"].(map[string]any)) {
				return false
			}
			if unique, _ := s["uniqueItems"].(bool); unique {
				for j := 0; j < i; j++ {
					if sameJSON(x, a[j]) {
						return false
					}
				}
			}
		}
	case "string":
		x, ok := v.(string)
		if !ok {
			return false
		}
		if n, ok := s["minLength"].(float64); ok && len(x) < int(n) {
			return false
		}
		if n, ok := s["maxLength"].(float64); ok && len(x) > int(n) {
			return false
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			return false
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok || !integerRE.MatchString(n.String()) {
			return false
		}
		if _, e := strconv.ParseUint(n.String(), 10, 64); e != nil {
			return false
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return false
		}
	}
	return true
}

// unsupportedTransformOnly classifies a refusal; it never decodes or accepts
// normalized input. The unchanged schema must validate every other field.
func unsupportedTransformOnly(v any, shape map[string]any) bool {
	object, ok := v.(map[string]any)
	if !ok {
		return false
	}
	artifacts, ok := object["artifacts"].([]any)
	if !ok {
		return false
	}
	normalized := make(map[string]any, len(object))
	for key, value := range object {
		normalized[key] = value
	}
	copies := make([]any, len(artifacts))
	changed := false
	for i, value := range artifacts {
		artifact, ok := value.(map[string]any)
		if !ok {
			return false
		}
		transform, ok := artifact["transform"].(string)
		if !ok {
			return false
		}
		copy := make(map[string]any, len(artifact))
		for key, value := range artifact {
			copy[key] = value
		}
		if transform != "identity-v1" {
			copy["transform"] = "identity-v1"
			changed = true
		}
		copies[i] = copy
	}
	normalized["artifacts"] = copies
	return changed && validateShape(normalized, shape)
}
func decode(raw []byte, name string, out any) error {
	if len(raw) < 1 || len(raw) > metadataLimit || !utf8.Valid(raw) {
		return refuse("schema-size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, e := walkJSON(d, 0)
	if e != nil {
		return refuse("schema")
	}
	if _, e = d.Token(); e != io.EOF {
		return refuse("schema-trailing")
	}
	canonical, e := canonjson.MarshalFile(v)
	if e != nil || !bytes.Equal(raw, canonical) {
		return refuse("schema-canonical")
	}
	var schemas map[string]map[string]any
	if json.Unmarshal([]byte(wireSchemas), &schemas) != nil {
		return refuse("schema-shape")
	}
	if !validateShape(v, schemas[name]) {
		if name == "artifact_manifest_v1" && unsupportedTransformOnly(v, schemas[name]) {
			return refuse("mapping-transform-unsupported")
		}
		return refuse("schema-shape")
	}
	if json.Unmarshal(raw, out) != nil {
		return refuse("schema-type")
	}
	return nil
}
