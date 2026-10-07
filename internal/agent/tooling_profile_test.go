package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/rpcprobe"
)

// agentVMProfile is the profile the inventory gives agent-a and agent-b.
var agentVMProfile = []string{"homelab-cli", "ov-memory"}

func profileSpec(t *testing.T) map[string]any {
	t.Helper()
	spec := validToolingSpec(t)
	spec["files"] = append(spec["files"].([]any), map[string]any{
		"path": "homelab-cli/bin/feed", "content": "Zml4dHVyZQo=", "mode": 493,
		"sha256": agentenv.Digest([]byte("fixture\n")),
	})
	return spec
}

func containsAny(lines []string, fragment string) []string {
	found := []string{}
	for _, line := range lines {
		if strings.Contains(line, fragment) {
			found = append(found, line)
		}
	}
	return found
}

// Acceptance 2: a host with the agent-VM profile is never given local-file-llm:
// nothing of it is staged, linked or probed, so its Ollama server is never
// contacted.
func TestToolingProfileStagesLinksAndProbesOnlyItsTools(t *testing.T) {
	h := newProfileHost(t)
	spec := profileSpec(t)
	state, err := h.converge(agentVMProfile, spec)
	if err != nil || state != "updated" {
		t.Fatalf("converge = %s, %v", state, err)
	}
	if found := containsAny(h.commands, "local-file-llm"); len(found) > 0 {
		t.Errorf("local-file-llm commands ran: %v", found)
	}
	if found := containsAny(h.commands, "npm"); len(found) > 0 {
		t.Errorf("npm ran: %v", found)
	}
	if found := containsAny(h.exchanges, "local-file-llm"); len(found) > 0 {
		t.Errorf("local-file-llm was probed: %v", found)
	}
	if len(containsAny(h.exchanges, "ov-memory/run")) == 0 || len(containsAny(h.commands, "pip install")) != 1 ||
		len(containsAny(h.commands, "homelab-cli/bin/feed --help")) == 0 {
		t.Errorf("a selected tool was not staged and probed: %v %v", h.commands, h.exchanges)
	}
	want := []string{
		"feed -> ~/.local/share/upkeeper-tools/current/homelab-cli/bin/feed",
		"ov-memory-mcp -> ~/.local/share/upkeeper-tools/current/ov-memory/run",
	}
	if got := h.links(); !reflect.DeepEqual(got, want) {
		t.Errorf("links = %v, want %v", got, want)
	}
	current := mustReadlink(t, filepath.Join(h.home, ".local/share/upkeeper-tools/current"))
	if filepath.Base(current) == baseToolingIdentity {
		t.Error("a profile did not change the staged release identity")
	}
	if _, err := os.Lstat(filepath.Join(current, "local-file-llm")); !os.IsNotExist(err) {
		t.Errorf("local-file-llm was staged: %v", err)
	}
	after, err := toolingObserve(h.context(agentVMProfile), spec)
	if err != nil || len(after["drift"].([]string)) != 0 {
		t.Fatalf("observe after converge: %v %v", after["drift"], err)
	}
	if state, err := h.converge(agentVMProfile, spec); err != nil || state != "converged" {
		t.Fatalf("repeat = %s, %v", state, err)
	}
}

// Acceptance 2: the same host previously carrying every tool has the bindings
// UpKeeper made for the dropped one removed, and a binding UpKeeper did not
// make kept.
func TestToolingProfileRemovesTheDroppedToolsOwnedBindings(t *testing.T) {
	for _, kind := range []string{"owned", "foreign link", "regular file"} {
		t.Run(kind, func(t *testing.T) {
			h := newProfileHost(t)
			spec := profileSpec(t)
			if _, err := h.converge(nil, spec); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(h.home, ".local/bin/local-file-llm-mcp")
			switch kind {
			case "foreign link":
				if err := swapLink(path, "/opt/my-llm/run"); err != nil {
					t.Fatal(err)
				}
			case "regular file":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			original, err := snapshotLink(path)
			if err != nil {
				t.Fatal(err)
			}
			before, err := toolingObserve(h.context(agentVMProfile), spec)
			if err != nil {
				t.Fatal(err)
			}
			drift := strings.Join(before["drift"].([]string), "; ")
			if kind == "owned" != strings.Contains(drift, "dropped tool binding remains: local-file-llm-mcp") {
				t.Errorf("drift before = %s", drift)
			}
			if state, err := h.converge(agentVMProfile, spec); err != nil || state != "updated" {
				t.Fatalf("converge = %s, %v", state, err)
			}
			now, err := snapshotLink(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "owned" && now.Exists {
				t.Fatal("the dropped tool's binding remains")
			}
			if kind != "owned" && !reflect.DeepEqual(now, original) {
				t.Fatalf("a binding UpKeeper did not make was changed: %+v", now)
			}
			if state, err := h.converge(agentVMProfile, spec); err != nil || state != "converged" {
				t.Fatalf("repeat = %s, %v", state, err)
			}
		})
	}
}

// Dropping homelab-cli removes every command binding UpKeeper made for it.
func TestToolingProfileWithoutHomelabCLIRemovesItsCommands(t *testing.T) {
	h := newProfileHost(t)
	spec := profileSpec(t)
	if _, err := h.converge(nil, spec); err != nil {
		t.Fatal(err)
	}
	h.commands = nil
	if state, err := h.converge([]string{"ov-memory"}, spec); err != nil || state != "updated" {
		t.Fatalf("converge = %s, %v", state, err)
	}
	if got, want := h.links(), []string{"ov-memory-mcp -> ~/.local/share/upkeeper-tools/current/ov-memory/run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
	if found := containsAny(h.commands, "homelab-cli"); len(found) > 0 {
		t.Fatalf("homelab-cli was probed: %v", found)
	}
}

// The identity is the effective profile's: order does not matter, and naming
// every tool is the same as naming none.
func TestToolingProfileIdentity(t *testing.T) {
	spec := profileSpec(t)
	identity := func(tools []string) string {
		id, err := toolingIdentityFor(&Context{Host: HostRef{AgentTools: tools}}, spec)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if identity(nil) != toolingIdentity(spec) {
		t.Error("no profile changed the identity")
	}
	if identity([]string{"local-file-llm", "ov-memory", "homelab-cli"}) != identity(nil) {
		t.Error("every tool is not the same as no profile")
	}
	if identity([]string{"ov-memory", "homelab-cli"}) != identity([]string{"homelab-cli", "ov-memory"}) {
		t.Error("the order of a profile changed the identity")
	}
	if identity([]string{"ov-memory"}) == identity(nil) || identity([]string{"ov-memory"}) == identity([]string{"homelab-cli"}) {
		t.Error("different profiles share an identity")
	}
}

// A payload whose profile is not one is refused before anything changes.
func TestToolingRefusesAMalformedProfileFromThePayload(t *testing.T) {
	for _, tools := range [][]string{{}, {"ollama"}, {"ov-memory", "ov-memory"}, {""}} {
		h := newProfileHost(t)
		_, err := h.converge(tools, profileSpec(t))
		if err == nil || !strings.Contains(err.Error(), "agent_tools") {
			t.Errorf("profile %q: %v", tools, err)
		}
		if len(h.commands) != 0 {
			t.Errorf("profile %q ran %v", tools, h.commands)
		}
	}
}

func profileExecute(t *testing.T, h *profileHost, tools []string, bin string) map[string]any {
	t.Helper()
	c := freshTestContext(t, h.home, bin, h.run, []string{release.ToolingComponent},
		map[string]any{release.ToolingComponent: profileSpec(t)})
	c.Host.AgentTools = tools
	c.Payload.Host.AgentTools = tools
	return Execute(c)
}

// Acceptance 4: with ov-memory in the profile a missing key blocks the
// component before anything is staged or probed; without it no key is needed.
func TestTheOVCredentialIsRequiredOnlyForOVMemory(t *testing.T) {
	h := newProfileHost(t)
	bin := freshTestTools(t, h.home, freshTestCommands...)
	result := profileExecute(t, h, agentVMProfile, bin)
	entry := resultEntry(t, result, release.ToolingComponent)
	if message, _ := entry["error"].(string); entry["state"] != "blocked" ||
		!strings.HasPrefix(message, "blocked: ") || !strings.Contains(message, "credential missing ov-memory API key") {
		t.Fatalf("entry = %v", entry)
	}
	if found := containsAny(h.commands, "upkeeper-tools"); len(found) > 0 || len(containsAny(h.commands, "mise upgrade")) > 0 || len(h.exchanges) > 0 {
		t.Fatalf("a blocked component staged or probed: %v %v", h.commands, h.exchanges)
	}

	h = newProfileHost(t)
	bin = freshTestTools(t, h.home, freshTestCommands...)
	result = profileExecute(t, h, []string{"homelab-cli"}, bin)
	entry = resultEntry(t, result, release.ToolingComponent)
	if entry["state"] != "updated" {
		t.Fatalf("entry = %v", entry)
	}
	for _, fragment := range []string{"secret-tool", "ov-memory", "venv", "pip", "npm"} {
		if found := containsAny(h.commands, fragment); len(found) > 0 {
			t.Errorf("a host without ov-memory ran %v", found)
		}
	}
	if len(h.exchanges) != 0 {
		t.Errorf("a host with only homelab-cli probed %v", h.exchanges)
	}
}

// Acceptance 6, the cause: in bash, command -v with several names succeeds
// when any one of them resolves, so the base's staging check passed on agent-a
// where secret-tool was not installed.
func TestTheBaseCLICheckPassedWithoutSecretTool(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	bin := t.TempDir()
	if err := os.Symlink(bash, filepath.Join(bin, "bash")); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(bash, "-c", strings.TrimPrefix(baseCLICheck, "bash -c "))
	command.Env = []string{"PATH=" + bin}
	if err := command.Run(); err != nil {
		t.Fatalf("the base check now fails without secret-tool (%v); its cause is no longer the one recorded", err)
	}
}

// Acceptance 6, the behaviour: secret-tool is required only when ov-memory is
// in the profile and the key could only come from the keyring; a missing one
// is then named, and nothing is staged.
func TestSecretToolIsRequiredOnlyWhenTheKeyringIsTheOnlySource(t *testing.T) {
	without := []string{}
	for _, name := range freshTestCommands {
		if name != "secret-tool" {
			without = append(without, name)
		}
	}
	h := newProfileHost(t)
	bin := freshTestTools(t, h.home, without...)
	result := profileExecute(t, h, agentVMProfile, bin)
	entry := resultEntry(t, result, release.ToolingComponent)
	if message, _ := entry["error"].(string); entry["state"] != "blocked" ||
		!strings.Contains(message, "missing OS prerequisite secret-tool") {
		t.Fatalf("agent-a without secret-tool or a key: %v", entry)
	}
	if len(containsAny(h.commands, "upkeeper-tools")) > 0 {
		t.Fatalf("staged without secret-tool: %v", h.commands)
	}

	for name, tools := range map[string][]string{"key file": agentVMProfile, "no ov-memory": {"homelab-cli"}} {
		h := newProfileHost(t)
		bin := freshTestTools(t, h.home, without...)
		if name == "key file" {
			adapterTestWrite(t, filepath.Join(h.home, ".config/ov-mcp/key"), "redacted\n")
		}
		entry := resultEntry(t, profileExecute(t, h, tools, bin), release.ToolingComponent)
		if entry["state"] != "updated" {
			t.Fatalf("%s without secret-tool: %v", name, entry)
		}
	}
}

// Without ov-memory the OV virtualenv is never built, so Debian's
// python3-venv is not a prerequisite either.
func TestTheVirtualenvModuleIsRequiredOnlyForOVMemory(t *testing.T) {
	h := newProfileHost(t)
	bin := freshTestTools(t, h.home, freshTestCommands...)
	h.fail = func(argv []string) *procx.Result {
		if len(argv) > 1 && argv[0] == "python3" && argv[1] == "-c" {
			return &procx.Result{Code: 1, Stderr: "No module named 'ensurepip'"}
		}
		return nil
	}
	entry := resultEntry(t, profileExecute(t, h, []string{"homelab-cli"}, bin), release.ToolingComponent)
	if entry["state"] != "updated" {
		t.Fatalf("entry = %v", entry)
	}
}

// The agent-environment component registers only the profile's MCP servers
// on a host, from the one release every host shares.
func TestTheEnvironmentRegistersOnlyTheProfilesServers(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	for path, text := range map[string]string{
		".codex/AGENTS.md":                "# Global agent instructions\n",
		".claude/omarchy-setup/CLAUDE.md": "# Shared rules\n",
		".claude/CLAUDE.md":               agentenv.ClaudeBegin + "\n@~/.claude/omarchy-setup/CLAUDE.md\n" + agentenv.ClaudeEnd,
		".claude/settings.json":           `{"model":"test","permissions":{"allow":[]}}`,
		".claude.json":                    `{"mcpServers":{"huyang":{"command":"huyang"}}}`,
		".codex/config.toml": "[mcp_servers.huyang]\ncommand = \"huyang\"\n[mcp_servers.ov-memory]\ncommand = \"python\"\n" +
			"[mcp_servers.local-file-llm]\ncommand = \"node\"\n",
		".config/opencode/opencode.json": `{"mcp":{"huyang":{"command":["huyang"]}},"instructions":[]}`,
	} {
		adapterTestWrite(t, filepath.Join(source, path), text)
	}
	captured, err := agentenv.CaptureEnvironment(source, agentenv.ProjectInstructionRef{
		Repository: "github.com/iryzhkov/dev-fleet", RepositoryRevision: strings.Repeat("a", 40),
		Path: "README.md", SHA256: strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := agentenv.WithTooling(captured)
	if err != nil {
		t.Fatal(err)
	}
	for name, tools := range map[string][]string{"default": nil, "agent VM": agentVMProfile} {
		t.Run(name, func(t *testing.T) {
			h := newProfileHost(t)
			c := h.context(tools)
			actions := []string{}
			before, err := environmentObserve(c, spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := environmentApply(c, spec, before, &actions); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{".codex/config.toml", ".claude.json", ".config/opencode/opencode.json"} {
				data, err := os.ReadFile(filepath.Join(h.home, path))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "local-file-llm") != (tools == nil) ||
					!strings.Contains(string(data), "ov-memory-mcp") {
					t.Errorf("%s:\n%s", path, data)
				}
			}
		})
	}
}

// Acceptance 5: a failed health check reports the tool's own error text.
func TestToolingProbeReportsTheToolsErrorText(t *testing.T) {
	h := newProfileHost(t)
	toolingExchange = rpcprobe.Exchange
	dir := t.TempDir()
	answer := `{"jsonrpc":"2.0","id":3,"result":{"isError":true,"content":[{"type":"text","text":"OpenViking API key not found (api_key=sk-` +
		strings.Repeat("Q7", 16) + `)"}]}}`
	adapterTestWrite(t, filepath.Join(dir, "ov-memory/answer"), answer+"\n")
	script := "#!/bin/sh\nread -r l\necho '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}'\n" +
		"read -r l\nread -r l\necho '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[{\"name\":\"memory_status\"}]}}'\n" +
		"read -r l\ncat \"$(dirname \"$0\")/answer\"\n"
	adapterTestWrite(t, filepath.Join(dir, "ov-memory/run"), script)
	if err := os.Chmod(filepath.Join(dir, "ov-memory/run"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := h.context([]string{"ov-memory"})
	c.Env = []string{"PATH=/usr/bin:/bin"}
	err := probeTooling(c, dir, &release.Tooling{Python: []string{"mcp==1.0.0"}})
	if err == nil {
		t.Fatal("a failed health check passed")
	}
	message := err.Error()
	if !strings.HasPrefix(message, "ov-memory health check: tools/call memory_status failed: OpenViking API key not found") {
		t.Fatalf("error = %q", message)
	}
	if strings.Contains(message, strings.Repeat("Q7", 16)) {
		t.Fatalf("the key leaked: %q", message)
	}
}
