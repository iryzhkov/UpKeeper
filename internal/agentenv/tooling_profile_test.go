package agentenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// toolingTarget captures a release with both tooling registrations and gives
// a target home that already has a host-local credential for ov-memory.
func toolingTarget(t *testing.T) (map[string]any, string) {
	t.Helper()
	home := fakeHome(t)
	writeHomeFile(t, home, ".codex/config.toml", "[mcp_servers.huyang]\ncommand = \"huyang\"\n"+
		"[mcp_servers.ov-memory]\ncommand = \"old-python\"\n[mcp_servers.ov-memory.env]\nOV_AGENT = \"codex\"\n"+
		"[mcp_servers.local-file-llm]\ncommand = \"old-node\"\n[mcp_servers.local-file-llm.env]\nLOCAL_LLM_URL = \"http://example.invalid:11434\"\n", 0600)
	normalized, err := WithTooling(capture(t, home))
	if err != nil {
		t.Fatal(err)
	}
	target := fakeHome(t)
	writeHomeFile(t, target, ".codex/config.toml", "# host comment\n[mcp_servers.ov-memory]\ncommand = \"host-python\"\n[mcp_servers.ov-memory.env]\nOV_API_KEY = \"host-secret\"\n", 0600)
	return normalized, target
}

var toolingRegistrationFiles = []string{".codex/config.toml", ".claude.json", ".config/opencode/opencode.json"}

// baseRegistrationDigests are the digests, with the target home written as
// /HOME, of the files the base (b3a105d) projected for a host without a
// profile. A host that declares none must keep exactly these bytes.
var baseRegistrationDigests = map[string]string{
	".codex/config.toml":             "1485b9c45f5ba565416eae19fe9ae710a20edfae85d784bf2501c9bc610fa646",
	".claude.json":                   "ad865732b0738dfd267c41d6a9d794c72328520e1302296b5b94cdc5b8d76001",
	".config/opencode/opencode.json": "fcf2962802c132cc788ab020f033b828a3438f56ca6bb1312ff451000b22b103",
}

func normalizedDigest(t *testing.T, home, path string) string {
	t.Helper()
	return Digest([]byte(strings.ReplaceAll(readHomeFile(t, home, path), home, "/HOME")))
}

func applyWithout(t *testing.T, component map[string]any, home, runID string, without []string) *ApplyResult {
	t.Helper()
	result, err := ApplyEnvironmentWithout(component, home, runID, false, nil, without)
	if err != nil {
		t.Fatalf("apply without %v failed: %v", without, err)
	}
	return result
}

// A host that previously carried local-file-llm and drops it from its profile
// loses the registrations UpKeeper wrote in every harness, and keeps every
// other registration, the host's credentials and the host's own text.
func TestToolingProfileRemovesTheDroppedToolsOwnedRegistrations(t *testing.T) {
	normalized, target := toolingTarget(t)
	apply(t, normalized, target, "all-tools", false, nil)
	result := applyWithout(t, normalized, target, "profile", []string{"local-file-llm"})
	if result.State != "updated" {
		t.Fatalf("state %s", result.State)
	}
	for _, path := range toolingRegistrationFiles {
		content := readHomeFile(t, target, path)
		if strings.Contains(content, "local-file-llm") {
			t.Errorf("%s still registers local-file-llm:\n%s", path, content)
		}
		if !strings.Contains(content, target+"/.local/bin/ov-memory-mcp") || !strings.Contains(content, "huyang") {
			t.Errorf("%s lost a registration it keeps:\n%s", path, content)
		}
	}
	codex := readHomeFile(t, target, ".codex/config.toml")
	if !strings.Contains(codex, "host-secret") || !strings.HasPrefix(codex, "# host comment\n") {
		t.Fatalf("host text lost:\n%s", codex)
	}
	if again := applyWithout(t, normalized, target, "profile-again", []string{"local-file-llm"}); again.State != "converged" {
		t.Fatalf("not idempotent: %s", again.State)
	}
	observation, err := ObserveEnvironmentWithout(normalized, target, "observe", []string{"local-file-llm"})
	if err != nil {
		t.Fatal(err)
	}
	for _, drift := range observation.Drift {
		if drift.State != DriftMatch {
			t.Errorf("drift after convergence: %+v", drift)
		}
	}
	// Adding the tool back registers it again.
	apply(t, normalized, target, "all-tools-again", false, nil)
	for _, path := range toolingRegistrationFiles {
		if !strings.Contains(readHomeFile(t, target, path), target+"/.local/bin/local-file-llm-mcp") {
			t.Errorf("%s did not register local-file-llm again", path)
		}
	}
}

// A registration of the same name that UpKeeper did not write, such as one a
// person made by hand for another server, is never removed.
func TestToolingProfileKeepsAForeignRegistrationOfADroppedTool(t *testing.T) {
	normalized, target := toolingTarget(t)
	writeHomeFile(t, target, ".codex/config.toml", "[mcp_servers.local-file-llm]\ncommand = \"/opt/my-llm\"\n", 0600)
	writeHomeFile(t, target, ".claude.json", `{"mcpServers":{"local-file-llm":{"command":"/opt/my-llm"}}}`, 0600)
	writeHomeFile(t, target, ".config/opencode/opencode.json",
		`{"mcp":{"local-file-llm":{"type":"local","command":["/opt/my-llm"]}},"instructions":[]}`, 0600)
	applyWithout(t, normalized, target, "profile", []string{"local-file-llm"})
	for _, path := range toolingRegistrationFiles {
		content := readHomeFile(t, target, path)
		if !strings.Contains(content, "/opt/my-llm") || strings.Contains(content, "local-file-llm-mcp") {
			t.Errorf("%s did not keep the foreign registration alone:\n%s", path, content)
		}
	}
}

// A fresh host with a profile is never given the dropped tool's registration.
func TestToolingProfileOnAFreshHostRegistersOnlyItsTools(t *testing.T) {
	normalized, _ := toolingTarget(t)
	target := filepath.Join(t.TempDir(), "fresh")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	applyWithout(t, normalized, target, "fresh", []string{"local-file-llm", "ov-memory"})
	for _, path := range toolingRegistrationFiles {
		content := readHomeFile(t, target, path)
		if strings.Contains(content, "local-file-llm") || strings.Contains(content, "ov-memory") {
			t.Errorf("%s registers a dropped tool:\n%s", path, content)
		}
	}
}

func TestToolingProfileRefusesAnUnknownServer(t *testing.T) {
	normalized, target := toolingTarget(t)
	if _, err := ApplyEnvironmentWithout(normalized, target, "unknown", false, nil, []string{"huyang"}); err == nil ||
		!strings.Contains(err.Error(), "huyang") {
		t.Fatalf("dropping a server the tooling component does not own: %v", err)
	}
}

func TestToolingRegistrationsWithoutAProfileAreTheBase(t *testing.T) {
	normalized, target := toolingTarget(t)
	apply(t, normalized, target, "base", false, nil)
	for _, path := range toolingRegistrationFiles {
		if got := normalizedDigest(t, target, path); got != baseRegistrationDigests[path] {
			t.Errorf("%s digest = %s, want the base's %s\n%s", path, got, baseRegistrationDigests[path], readHomeFile(t, target, path))
		}
	}
}
