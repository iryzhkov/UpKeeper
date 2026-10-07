package agentenv

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRound3DroppedWrapperWithArguments(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned", false: "foreign"}[owned], func(t *testing.T) {
			component, home := toolingTarget(t)
			apply(t, component, home, "all", false, nil)
			path := ".config/opencode/opencode.json"
			var config map[string]any
			if err := json.Unmarshal([]byte(readHomeFile(t, home, path)), &config); err != nil {
				t.Fatal(err)
			}
			server := config["mcp"].(map[string]any)["local-file-llm"].(map[string]any)
			command := filepath.Join(home, ".local/bin/local-file-llm-mcp")
			if !owned {
				command = "/opt/foreign-llm"
			}
			server["command"] = []any{command, "--verbose"}
			server["environment"] = map[string]any{"API_KEY": "FAKE_HOST_SECRET"}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			writeHomeFile(t, home, path, string(data), 0600)
			applyWithout(t, component, home, "drop", []string{"local-file-llm"})
			got := readHomeFile(t, home, path)
			if owned {
				if strings.Contains(got, "local-file-llm") || strings.Contains(got, "FAKE_HOST_SECRET") {
					t.Fatalf("excluded managed registration remains: %s", got)
				}
			} else if !strings.Contains(got, command) || !strings.Contains(got, "--verbose") || !strings.Contains(got, "FAKE_HOST_SECRET") {
				t.Fatalf("foreign registration lost: %s", got)
			}
			if again := applyWithout(t, component, home, "drop-again", []string{"local-file-llm"}); again.State != "converged" {
				t.Fatalf("not idempotent: %s", again.State)
			}
		})
	}
}
