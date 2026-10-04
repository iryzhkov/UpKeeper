package toolcatalog

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/synthetic.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func fixtureMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if e := json.Unmarshal(fixture(t), &m); e != nil {
		t.Fatal(e)
	}
	return m
}
func source(m map[string]any) map[string]any   { return m["sources"].([]any)[0].(map[string]any) }
func artifact(m map[string]any) map[string]any { return m["artifacts"].([]any)[0].(map[string]any) }
func tool(m map[string]any) map[string]any     { return m["tools"].([]any)[0].(map[string]any) }
func TestStrictSchema(t *testing.T) {
	if _, e := decode(fixture(t)); e != nil {
		t.Fatal(e)
	}
	cases := map[string]func(map[string]any){
		"unknown-root":       func(m map[string]any) { m["installed"] = true },
		"unknown-source":     func(m map[string]any) { source(m)["ci"] = "passed" },
		"unknown-artifact":   func(m map[string]any) { artifact(m)["size"] = 1 },
		"unknown-tool":       func(m map[string]any) { tool(m)["authorized"] = true },
		"unknown-route":      func(m map[string]any) { tool(m)["primary_route"].(map[string]any)["endpoint"] = "secret" },
		"missing-field":      func(m map[string]any) { delete(artifact(m), "required") },
		"null-bool":          func(m map[string]any) { artifact(m)["required"] = nil },
		"null-array":         func(m map[string]any) { m["tools"] = nil },
		"bool-type":          func(m map[string]any) { artifact(m)["required"] = "true" },
		"mode-float":         func(m map[string]any) { artifact(m)["mode"] = 420.5 },
		"version-type":       func(m map[string]any) { m["schema_version"] = "1" },
		"version":            func(m map[string]any) { m["schema_version"] = 2 },
		"state":              func(m map[string]any) { m["state"] = "active" },
		"id":                 func(m map[string]any) { tool(m)["id"] = "Bad" },
		"source-ref":         func(m map[string]any) { artifact(m)["source_id"] = "absent" },
		"artifact-ref":       func(m map[string]any) { tool(m)["artifact_ids"] = []string{"absent"} },
		"duplicate-source":   func(m map[string]any) { m["sources"] = append(m["sources"].([]any), source(m)) },
		"duplicate-artifact": func(m map[string]any) { m["artifacts"] = append(m["artifacts"].([]any), artifact(m)) },
		"duplicate-path": func(m map[string]any) {
			a := map[string]any{}
			for k, v := range artifact(m) {
				a[k] = v
			}
			a["id"] = "other"
			m["artifacts"] = append(m["artifacts"].([]any), a)
		},
		"duplicate-tool":  func(m map[string]any) { m["tools"] = append(m["tools"].([]any), tool(m)) },
		"canonical-owner": func(m map[string]any) { m["tools"].([]any)[1].(map[string]any)["canonical_tool"] = "huyang" },
		"alias-owner":     func(m map[string]any) { m["tools"].([]any)[1].(map[string]any)["aliases"] = []string{"huyang"} },
		"alias-duplicate": func(m map[string]any) { tool(m)["aliases"] = []string{"x", "x"} },
		"alias-self":      func(m map[string]any) { tool(m)["aliases"] = []string{"huyang"} },
		"route-duplicate": func(m map[string]any) {
			m["tools"].([]any)[2].(map[string]any)["primary_route"] = tool(m)["primary_route"]
		},
		"route-kind":             func(m map[string]any) { tool(m)["primary_route"].(map[string]any)["kind"] = "cli" },
		"capability-owner":       func(m map[string]any) { tool(m)["canonical_tool"] = "pretender" },
		"capability-duplicate":   func(m map[string]any) { tool(m)["capabilities"] = []string{"example", "example"} },
		"capability-invalid":     func(m map[string]any) { tool(m)["capabilities"] = []string{"BAD"} },
		"empty-capability":       func(m map[string]any) { tool(m)["capabilities"] = []string{} },
		"empty-refs":             func(m map[string]any) { tool(m)["artifact_ids"] = []string{} },
		"ref-duplicate":          func(m map[string]any) { tool(m)["artifact_ids"] = []string{"payload", "payload"} },
		"required-optional":      func(m map[string]any) { artifact(m)["required"] = false },
		"orphan":                 func(m map[string]any) { m["tools"] = []any{} },
		"platform":               func(m map[string]any) { tool(m)["platforms"] = []string{"debian"} },
		"platform-duplicate":     func(m map[string]any) { tool(m)["platforms"] = []string{"linux-amd64", "linux-amd64"} },
		"agent-id":               func(m map[string]any) { tool(m)["id"] = "agent99" },
		"agent-canonical":        func(m map[string]any) { tool(m)["canonical_tool"] = "agent99" },
		"agent-alias":            func(m map[string]any) { tool(m)["aliases"] = []string{"agent99"} },
		"agent-route":            func(m map[string]any) { tool(m)["primary_route"].(map[string]any)["name"] = "agent99" },
		"path":                   func(m map[string]any) { artifact(m)["path"] = "../secret" },
		"path-backslash":         func(m map[string]any) { artifact(m)["path"] = "a\\b" },
		"path-empty-segment":     func(m map[string]any) { artifact(m)["path"] = "a//b" },
		"path-dot":               func(m map[string]any) { artifact(m)["path"] = "a/./b" },
		"path-absolute":          func(m map[string]any) { artifact(m)["path"] = "/etc/passwd" },
		"path-long":              func(m map[string]any) { artifact(m)["path"] = strings.Repeat("x", 256) },
		"hash-uppercase":         func(m map[string]any) { artifact(m)["sha256"] = strings.Repeat("A", 64) },
		"mode-special":           func(m map[string]any) { artifact(m)["mode"] = 4095 },
		"mode-negative":          func(m map[string]any) { artifact(m)["mode"] = -1 },
		"repository-credentials": func(m map[string]any) { source(m)["repository"] = "https://user:secret@example.invalid/a" },
		"repository-query":       func(m map[string]any) { source(m)["repository"] = "https://example.invalid/a?token=secret" },
		"repository-fragment":    func(m map[string]any) { source(m)["repository"] = "https://example.invalid/a#secret" },
		"repository-scheme":      func(m map[string]any) { source(m)["repository"] = "ssh://example.invalid/a" },
		"revision-kind":          func(m map[string]any) { source(m)["revision_kind"] = "branch" },
		"revision-pin":           func(m map[string]any) { source(m)["revision"] = "main" },
		"purpose-control":        func(m map[string]any) { tool(m)["purpose"] = "secret\x1b[31m" },
		"purpose-empty":          func(m map[string]any) { tool(m)["purpose"] = "" },
		"purpose-long":           func(m map[string]any) { tool(m)["purpose"] = strings.Repeat("x", 513) },
		"sources-limit": func(m map[string]any) {
			a := []any{}
			for i := 0; i < 129; i++ {
				a = append(a, source(m))
			}
			m["sources"] = a
		},
		"tools-limit": func(m map[string]any) {
			a := []any{}
			for i := 0; i < 257; i++ {
				a = append(a, tool(m))
			}
			m["tools"] = a
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := fixtureMap(t)
			change(m)
			b, e := json.Marshal(m)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = decode(b); e == nil {
				t.Fatal("accepted invalid declaration")
			}
		})
	}
}
func TestRecursiveDuplicateAndBounds(t *testing.T) {
	raw := string(fixture(t))
	cases := []string{
		strings.Replace(raw, `"schema_version": 1`, `"schema_version": 1, "schema_version": 1`, 1),
		strings.Replace(raw, `"repository":`, `"id":"duplicate", "repository":`, 1),
		strings.Replace(raw, `"path":`, `"id":"duplicate", "path":`, 1),
		strings.Replace(raw, `"canonical_tool":`, `"id":"duplicate", "canonical_tool":`, 1),
		strings.Replace(raw, `"kind": "mcp"`, `"name":"duplicate", "kind": "mcp"`, 1),
		strings.Replace(raw, `"schema_version": 1`, `"schema_version": 1.0`, 1),
		raw + " {}", strings.Repeat(" ", metadataLimit) + raw,
		"{\"x\":" + strings.Repeat("[", 13) + "0" + strings.Repeat("]", 13) + "}",
		"[" + strings.Repeat("0,", 1024) + "0]",
		string([]byte{0xff}),
	}
	for i, b := range cases {
		if _, e := decode([]byte(b)); e == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
func TestEmptyAndGitSource(t *testing.T) {
	b := []byte(`{"schema_version":1,"state":"candidate-inert","sources":[],"artifacts":[],"tools":[]}`)
	if _, e := decode(b); e != nil {
		t.Fatal(e)
	}
	m := fixtureMap(t)
	source(m)["revision_kind"] = "git-sha1"
	source(m)["revision"] = strings.Repeat("0", 40)
	b, _ = json.Marshal(m)
	if _, e := decode(b); e != nil {
		t.Fatal(e)
	}
}
