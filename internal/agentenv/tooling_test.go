package agentenv

import (
	"strings"
	"testing"
)

func TestToolingNormalizationAndHostEnvironment(t *testing.T) {
	home := fakeHome(t)
	writeHomeFile(t, home, ".codex/config.toml", "[mcp_servers.ov-memory]\ncommand = \"old-python\"\n[mcp_servers.ov-memory.env]\nOV_AGENT = \"codex\"\n[mcp_servers.local-file-llm]\ncommand = \"old-node\"\n[mcp_servers.local-file-llm.env]\nLOCAL_LLM_URL = \"http://example.invalid:11434\"\n", 0600)
	normalized, err := WithTooling(capture(t, home))
	if err != nil {
		t.Fatal(err)
	}
	target := fakeHome(t)
	writeHomeFile(t, target, ".codex/config.toml", "[mcp_servers.ov-memory]\ncommand = \"host-python\"\n[mcp_servers.ov-memory.env]\nOV_API_KEY = \"host-secret\"\n", 0600)
	env, err := MCPEnvironment(normalized, target, "ov-memory", []string{"HOME=" + target, "OV_AGENT=old"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "OV_API_KEY=host-secret") || !strings.Contains(joined, "OV_AGENT=codex") {
		t.Fatal("lost host environment or release overlay")
	}
	result := apply(t, normalized, target, "tooling-projection", false, nil)
	if result.State != "updated" {
		t.Fatalf("unexpected state %s", result.State)
	}
	for _, path := range []string{".codex/config.toml", ".claude.json", ".config/opencode/opencode.json"} {
		content := readHomeFile(t, target, path)
		if !strings.Contains(content, target+"/.local/bin/ov-memory-mcp") || !strings.Contains(content, target+"/.local/bin/local-file-llm-mcp") {
			t.Fatalf("%s lacks rebased runtime wrappers", path)
		}
	}
	if !strings.Contains(readHomeFile(t, target, ".codex/config.toml"), "host-secret") {
		t.Fatal("host credential lost")
	}
	again := apply(t, normalized, target, "tooling-projection-again", false, nil)
	if again.State != "converged" {
		t.Fatalf("not idempotent: %s", again.State)
	}
}
