package agent

import (
	"os"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// HarnessReport records, read-only, which agent harnesses have Huyang
// registered. UpKeeper reports the gap; it does not edit a harness's own
// configuration behind the operator's back.
//
// A nil entry means the file could not be read, which is different from a file
// that exists and does not register Huyang.
func HarnessReport(c *Context) map[string]any {
	report := map[string]any{}

	if data, err := os.ReadFile(c.Path(".claude.json")); err != nil {
		report["claude"] = nil
	} else if document, err := canonjson.DecodeObject(data); err != nil {
		report["claude"] = nil
	} else {
		servers, _ := document["mcpServers"].(map[string]any)
		_, registered := servers["huyang"]
		report["claude"] = registered
	}

	for _, probe := range []struct {
		name   string
		path   string
		needle string
	}{
		{"codex", c.Path(".codex/config.toml"), "[mcp_servers.huyang]"},
		{"opencode", c.Path(".config/opencode/opencode.json"), `"huyang"`},
	} {
		data, err := os.ReadFile(probe.path)
		if err != nil {
			report[probe.name] = nil
			continue
		}
		report[probe.name] = strings.Contains(string(data), probe.needle)
	}
	return report
}
