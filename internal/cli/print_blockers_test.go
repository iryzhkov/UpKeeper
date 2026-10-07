package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestDoctorTextNamesEachBlockerWithItsInstallCommand(t *testing.T) {
	var out bytes.Buffer
	printDoctor(&out, map[string]any{
		"run_id": "20261006T090000Z-0a1b2c3d", "controller": "omarchy-pc",
		"result": "healthy", "exit_code": 0,
		"hosts": []any{map[string]any{
			"name": "agent-a", "reachable": true, "result": "healthy",
			"blockers": []any{map[string]any{
				"component": "agent-tooling", "name": "python3-venv",
				"install": "sudo apt-get install -y python3-venv",
			}},
		}},
	})
	expected := strings.Join([]string{
		"run 20261006T090000Z-0a1b2c3d controller=omarchy-pc doctor=healthy exit=0",
		"agent-a: healthy",
		"  blocker: agent-tooling needs python3-venv; sudo apt-get install -y python3-venv",
		"",
	}, "\n")
	if out.String() != expected {
		t.Errorf("produced\n%s\nexpected\n%s", out.String(), expected)
	}
}
