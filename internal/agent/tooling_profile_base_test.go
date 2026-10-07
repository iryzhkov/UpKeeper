package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/rpcprobe"
)

// profileHost converges the real tooling adapter against a temporary home. The
// host's programs answer through the runner and the MCP servers through the
// probe exchange, and both record what they were asked, so a test can compare
// one host's staging, links and probes with another's.
type profileHost struct {
	t         *testing.T
	home      string
	commands  []string
	exchanges []string
	fail      func(argv []string) *procx.Result
}

func newProfileHost(t *testing.T) *profileHost {
	t.Helper()
	h := &profileHost{t: t, home: t.TempDir()}
	priorExchange, priorModels := toolingExchange, toolingModels
	toolingExchange = func(argv, env []string, dir string, codex bool, calls []rpcprobe.Call, timeout time.Duration) ([]map[string]any, error) {
		names := []string{}
		for _, call := range calls {
			name := call.Method
			if params, ok := call.Params.(map[string]any); ok {
				if tool, ok := params["name"].(string); ok {
					name += " " + tool
				}
			}
			names = append(names, name)
		}
		h.exchanges = append(h.exchanges, h.relative(strings.Join(argv, " "))+": "+strings.Join(names, ", "))
		return []map[string]any{{"tools": []any{map[string]any{"name": "fixture"}}}, {}}, nil
	}
	toolingModels = func([]string, []string, string) ([]string, error) { return []string{"fixture"}, nil }
	t.Cleanup(func() { toolingExchange, toolingModels = priorExchange, priorModels })
	return h
}

func (h *profileHost) relative(text string) string {
	return strings.ReplaceAll(text, h.home, "~")
}

func (h *profileHost) run(command procx.Command) procx.Result {
	line := h.relative(strings.Join(command.Argv, " "))
	if command.Dir != "" && command.Dir != h.home {
		line += " (in " + h.relative(command.Dir) + ")"
	}
	h.commands = append(h.commands, line)
	if h.fail != nil {
		if result := h.fail(command.Argv); result != nil {
			return *result
		}
	}
	if len(command.Argv) >= 4 && command.Argv[len(command.Argv)-1] == "freeze" {
		return procx.Result{Stdout: "mcp==1.0.0\n"}
	}
	return procx.Result{}
}

func (h *profileHost) context(tools []string) *Context {
	return &Context{
		Home: h.home, RunID: "profile", Runner: h.run,
		Host: HostRef{Name: "agent-a", OSID: "debian", AgentTools: tools},
	}
}

// links lists every command binding in ~/.local/bin as name -> target.
func (h *profileHost) links() []string {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.home, ".local/bin"))
	if err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
	links := []string{}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(h.home, ".local/bin", entry.Name()))
		if err != nil {
			target = "(not a link)"
		}
		links = append(links, entry.Name()+" -> "+h.relative(target))
	}
	sort.Strings(links)
	return links
}

func (h *profileHost) converge(tools []string, spec map[string]any) (string, error) {
	h.t.Helper()
	c := h.context(tools)
	before, err := toolingObserve(c, spec)
	if err != nil {
		return "", err
	}
	actions := []string{}
	return toolingApply(c, spec, before, &actions)
}

// The identity, commands, links and probes below are what the base
// (b3a105d) produced for a host without a profile. A host that declares none
// must keep every one of them byte for byte, so that no converged host
// re-stages or changes when profiles are introduced.
const baseToolingIdentity = "a8e85c2b03e63e40826106785a34bae46385e5a254773a49ecddec296883f796"

const baseRelease = "~/.local/share/upkeeper-tools/releases/" + baseToolingIdentity

// baseCLICheck is the base's staging CLI check, which passed when any one
// of its names resolved. It is the one command a host without a profile no
// longer runs: the preflight checks each of those commands by name.
const baseCLICheck = "bash -c command -v bash curl jq python3 ssh readlink secret-tool >/dev/null"

var baseToolingCommands = []string{
	"~/.local/share/mise/shims/codex --version",
	"~/.local/share/mise/shims/claude --version",
	"~/.local/share/mise/shims/opencode --version",
	"~/.local/share/mise/shims/node --version",
	"mise upgrade --yes --no-prune",
	"~/.local/share/mise/shims/codex --version",
	"~/.local/share/mise/shims/claude --version",
	"~/.local/share/mise/shims/opencode --version",
	"~/.local/share/mise/shims/node --version",
	baseCLICheck,
	"python3 -m venv " + baseRelease + "/ov-memory/venv",
	baseRelease + "/ov-memory/venv/bin/python -m pip install --disable-pip-version-check --only-binary=:all: -r " + baseRelease + "/ov-memory/requirements.lock",
	"npm ci --ignore-scripts --no-audit --no-fund (in " + baseRelease + "/local-file-llm)",
	baseRelease + "/homelab-cli/bin/ha --help",
	baseRelease + "/homelab-cli/bin/toolfeedback --help",
	baseRelease + "/homelab-cli/bin/feed --help",
	baseRelease + "/homelab-cli/bin/pve --help",
	baseRelease + "/homelab-cli/bin/unifi --help",
	baseRelease + "/ov-memory/venv/bin/python -m pip check",
	baseRelease + "/ov-memory/venv/bin/python -m pip freeze",
	"~/.local/share/upkeeper-tools/current/homelab-cli/bin/ha --help",
	"~/.local/share/upkeeper-tools/current/homelab-cli/bin/toolfeedback --help",
	"~/.local/share/upkeeper-tools/current/homelab-cli/bin/feed --help",
	"~/.local/share/upkeeper-tools/current/homelab-cli/bin/pve --help",
	"~/.local/share/upkeeper-tools/current/homelab-cli/bin/unifi --help",
	"~/.local/share/upkeeper-tools/current/ov-memory/venv/bin/python -m pip check",
	"~/.local/share/upkeeper-tools/current/ov-memory/venv/bin/python -m pip freeze",
	baseRelease + "/homelab-cli/bin/ha --help",
	baseRelease + "/homelab-cli/bin/toolfeedback --help",
	baseRelease + "/homelab-cli/bin/feed --help",
	baseRelease + "/homelab-cli/bin/pve --help",
	baseRelease + "/homelab-cli/bin/unifi --help",
	baseRelease + "/ov-memory/venv/bin/python -m pip check",
	baseRelease + "/ov-memory/venv/bin/python -m pip freeze",
	"~/.local/share/mise/shims/codex --version",
	"~/.local/share/mise/shims/claude --version",
	"~/.local/share/mise/shims/opencode --version",
	"~/.local/share/mise/shims/node --version",
}

var baseToolingLinks = []string{
	"feed -> ~/.local/share/upkeeper-tools/current/homelab-cli/bin/feed",
	"local-file-llm-mcp -> ~/.local/share/upkeeper-tools/current/local-file-llm/run",
	"ov-memory-mcp -> ~/.local/share/upkeeper-tools/current/ov-memory/run",
}

var baseToolingExchanges = []string{
	baseRelease + "/ov-memory/run: tools/list, tools/call memory_status",
	baseRelease + "/local-file-llm/run: tools/list, tools/call local_status",
	"~/.local/share/upkeeper-tools/current/ov-memory/run: tools/list, tools/call memory_status",
	"~/.local/share/upkeeper-tools/current/local-file-llm/run: tools/list, tools/call local_status",
	baseRelease + "/ov-memory/run: tools/list, tools/call memory_status",
	baseRelease + "/local-file-llm/run: tools/list, tools/call local_status",
}

// withoutCommand is commands with every occurrence of one removed.
func withoutCommand(commands []string, removed string) []string {
	kept := []string{}
	for _, command := range commands {
		if command != removed {
			kept = append(kept, command)
		}
	}
	return kept
}

func TestToolingWithoutAProfileIsTheBaseBehaviour(t *testing.T) {
	spec := validToolingSpec(t)
	spec["files"] = append(spec["files"].([]any), map[string]any{
		"path": "homelab-cli/bin/feed", "content": "Zml4dHVyZQo=", "mode": 493,
		"sha256": agentenv.Digest([]byte("fixture\n")),
	})
	if _, err := release.DecodeTooling(spec); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	h := newProfileHost(t)
	state, err := h.converge(nil, spec)
	if err != nil || state != "updated" {
		t.Fatalf("converge = %s, %v", state, err)
	}
	identity := filepath.Base(mustReadlink(t, filepath.Join(h.home, ".local/share/upkeeper-tools/current")))
	if identity != baseToolingIdentity {
		t.Errorf("identity = %s, want the base's %s", identity, baseToolingIdentity)
	}
	if !reflect.DeepEqual(h.commands, withoutCommand(baseToolingCommands, baseCLICheck)) {
		t.Errorf("commands = %#v", h.commands)
	}
	if got := h.links(); !reflect.DeepEqual(got, baseToolingLinks) {
		t.Errorf("links = %#v", got)
	}
	if !reflect.DeepEqual(h.exchanges, baseToolingExchanges) {
		t.Errorf("exchanges = %#v", h.exchanges)
	}
}

func mustReadlink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	return target
}
