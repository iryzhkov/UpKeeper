package release

import (
	"encoding/base64"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"testing"
)

func toolingFixture(t *testing.T) map[string]any {
	t.Helper()
	tool := Tooling{Kind: ToolingComponent, ModelPolicy: "discover", Python: []string{"mcp==1.0.0"}}
	for _, path := range []string{"homelab-cli/bin/ha", "homelab-cli/bin/toolfeedback", "homelab-cli/lib/common.sh", "ov-memory/server.py", "ov-memory/friction.py", "local-file-llm/server.mjs", "local-file-llm/package.json", "local-file-llm/package-lock.json"} {
		data := []byte("fixture\n")
		tool.Files = append(tool.Files, ToolFile{Path: path, Content: base64.StdEncoding.EncodeToString(data), SHA256: digestOf(data), Mode: 0644})
	}
	encoded, err := canonjson.Compact(tool)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := canonjson.DecodeObject(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestToolingValidationRejectsUnsafeArtifacts(t *testing.T) {
	if _, err := DecodeTooling(toolingFixture(t)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"traversal", func(raw map[string]any) { raw["files"].([]any)[0].(map[string]any)["path"] = "../escape" }},
		{"venv", func(raw map[string]any) {
			raw["files"].([]any)[0].(map[string]any)["path"] = "ov-memory/venv/bin/python"
		}},
		{"wrapper", func(raw map[string]any) { raw["files"].([]any)[0].(map[string]any)["path"] = "local-file-llm/run" }},
		{"digest", func(raw map[string]any) { raw["files"].([]any)[0].(map[string]any)["sha256"] = "bad" }},
		{"permissions", func(raw map[string]any) { raw["files"].([]any)[0].(map[string]any)["mode"] = 0777 }},
		{"dependency-url", func(raw map[string]any) { raw["python_requirements"] = []any{"mcp @ https://example.invalid/token"} }},
		{"model-pins", func(raw map[string]any) { raw["model_policy"] = "frozen" }},
		{"unknown-field", func(raw map[string]any) { raw["secret"] = "value" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := toolingFixture(t)
			test.mutate(raw)
			if _, err := DecodeTooling(raw); err == nil {
				t.Fatal("unsafe bundle accepted")
			}
		})
	}
}
