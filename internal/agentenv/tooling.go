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
