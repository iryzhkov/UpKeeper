package agentenv

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// MCPEnvironment returns the environment the Codex registration will use after
// projection, including host-local fields retained by the normal merge.
// Values may contain credentials: callers must never log them.
func MCPEnvironment(raw map[string]any, home, name string, base []string) ([]string, error) {
	current := map[string]any{}
	data, err := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(data) > 0 {
		table, err := ParseTOML(string(data))
		if err != nil {
			return nil, err
		}
		current = table.Child("mcp_servers").Generic()
	}
	if raw != nil {
		component, err := DecodeComponent(raw)
		if err != nil {
			return nil, err
		}
		for _, file := range component.Files {
			if file.Path != ".codex/config.toml" {
				continue
			}
			owned, err := canonjson.DecodeObject(file.Content)
			if err != nil {
				return nil, err
			}
			if file.Mode == "codex-settings" {
				owned = objectOf(owned["mcp_servers"])
			}
			owned = rebase(owned, component.SourceHome, home).(map[string]any)
			current, err = MergeRegistrations(current, owned)
			if err != nil {
				return nil, err
			}
		}
	}
	values := map[string]string{}
	for _, item := range base {
		k, v, ok := strings.Cut(item, "=")
		if ok {
			values[k] = v
		}
	}
	server := objectOf(current[name])
	for k, v := range objectOf(server["env"]) {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("invalid %s MCP environment value", name)
		}
		values[k] = s
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}

// ToolingServers are the MCP servers the tooling component provides. Each is
// named for the agent tool that provides it.
var ToolingServers = []string{"ov-memory", "local-file-llm"}

// checkWithout refuses to drop any registration but a tooling server's: every
// other one belongs to the release or to the host.
func checkWithout(without []string) error {
	for _, name := range without {
		known := false
		for _, server := range ToolingServers {
			known = known || name == server
		}
		if !known {
			return fmt.Errorf("%q is not a tooling MCP server a profile can leave out", name)
		}
	}
	return nil
}

// dropping is what a host's agent tool profile leaves out of the shared
// release: the tooling MCP servers not to register, and the home whose
// wrappers mark a registration of one as UpKeeper's own. The zero value drops
// nothing, which is a host without a profile.
type dropping struct {
	names []string
	home  string
}

// owned removes the dropped servers from a release's owned registrations.
func (d dropping) owned(servers map[string]any) map[string]any {
	for _, name := range d.names {
		delete(servers, name)
	}
	return servers
}

// registrations removes from a harness's merged registrations each dropped
// server that UpKeeper registered: one that runs the tooling wrapper the
// tooling component binds in this home, and nothing else. A registration of
// the same name a person made, for another command, is theirs and stays. It
// returns the names it removed.
func (d dropping) registrations(servers map[string]any, opencode bool) []string {
	removed := []string{}
	for _, name := range d.names {
		server, ok := servers[name].(map[string]any)
		if !ok {
			continue
		}
		wrapper := filepath.Join(d.home, ".local/bin", name+"-mcp")
		owned := false
		if opencode {
			command, _ := server["command"].([]any)
			owned = len(command) == 1 && command[0] == wrapper
		} else {
			owned = server["command"] == wrapper
		}
		if owned {
			delete(servers, name)
			removed = append(removed, name)
		}
	}
	return removed
}

// WithTooling connects all three harnesses to the portable runtimes in the
// release. Only these two known local servers are normalized; remote endpoints
// and host-local credentials remain under the existing registration merge.
func WithTooling(raw map[string]any) (map[string]any, error) {
	c, err := DecodeComponent(raw)
	if err != nil {
		return nil, err
	}
	var shared map[string]any
	for _, f := range c.Files {
		if f.Path == ".codex/config.toml" {
			owned, err := canonjson.DecodeObject(f.Content)
			if err != nil {
				return nil, err
			}
			if f.Mode == "codex-settings" {
				shared = objectOf(owned["mcp_servers"])
			} else {
				shared = owned
			}
		}
	}
	for _, name := range []string{"ov-memory", "local-file-llm"} {
		if _, ok := shared[name].(map[string]any); !ok {
			return nil, fmt.Errorf("missing %s registration on publishing host", name)
		}
	}
	for i := range c.Files {
		f := &c.Files[i]
		if f.Mode != "claude-mcp" && f.Mode != "codex-settings" && f.Mode != "codex-mcp" && f.Mode != "opencode" {
			continue
		}
		owned, err := canonjson.DecodeObject(f.Content)
		if err != nil {
			return nil, err
		}
		servers := owned
		if f.Mode == "codex-settings" {
			servers = objectOf(owned["mcp_servers"])
		}
		if f.Mode == "opencode" {
			servers = objectOf(owned["mcp"])
		}
		for _, name := range []string{"ov-memory", "local-file-llm"} {
			original := objectOf(shared[name])
			wrapper := filepath.Join(c.SourceHome, ".local/bin", name+"-mcp")
			// The CLI is named ov-memory-mcp but local-file-llm-mcp; the same names
			// are bound atomically by the tooling adapter.
			server := objectOf(servers[name])
			if server == nil {
				server = map[string]any{}
			}
			if f.Mode == "opencode" {
				server["type"] = "local"
				server["command"] = []any{wrapper}
				if env, ok := original["env"]; ok {
					server["environment"] = env
				}
				delete(server, "args")
			} else {
				server["command"] = wrapper
				server["args"] = []any{}
				if env, ok := original["env"]; ok {
					server["env"] = env
				}
				if f.Mode == "claude-mcp" {
					server["type"] = "stdio"
				}
			}
			servers[name] = server
		}
		if f.Mode == "codex-settings" {
			owned["mcp_servers"] = servers
		}
		if f.Mode == "opencode" {
			owned["mcp"] = servers
		}
		f.Content, err = Canonical(owned)
		if err != nil {
			return nil, err
		}
		f.SHA256 = Digest(f.Content)
		f.SizeBytes = int64(len(f.Content))
	}
	desired := map[string][]byte{}
	for _, f := range c.Files {
		desired[f.Path] = f.Content
	}
	c.Spec, err = makeSpec(c.SourceHome, desired, desired, c.Files, c.Spec.ProjectInstructions[0], "capture")
	if err != nil {
		return nil, err
	}
	result, err := c.ToMap()
	if err != nil {
		return nil, err
	}
	return result, ValidateEnvironment(result)
}
