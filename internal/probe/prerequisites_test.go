package probe

import (
	"os"
	"path/filepath"
	"testing"
)

// Doctor reports what a fresh host lacks before a converge runs into it, and
// looks where the agent will look: ~/.local/bin and the mise shims count even
// though a non-interactive SSH session's PATH has neither.
func TestPrerequisitesNameWhatIsMissingWithTheInstallCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"jq", "mise"} {
		path := filepath.Join(home, ".local/bin", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	report := prerequisites(home, map[string]string{"ID": "debian"}, scriptedRunner(nil))
	if report["family"] != "debian" {
		t.Fatalf("family = %v", report["family"])
	}
	missing := report["missing"].(map[string]any)
	names := map[string]string{}
	for _, item := range missing["agent-tooling"].([]any) {
		entry := item.(map[string]any)
		names[entry["name"].(string)] = entry["install"].(string)
	}
	if names["python3"] != "sudo apt-get install -y python3" ||
		names["secret-tool"] != "sudo apt-get install -y libsecret-tools" {
		t.Fatalf("agent-tooling blockers = %v", names)
	}
	if _, found := names["jq"]; found {
		t.Fatal("jq in ~/.local/bin was reported missing")
	}
	if _, found := names["mise"]; found {
		t.Fatal("mise in ~/.local/bin was reported missing")
	}
	if _, found := missing["huyang"]; !found {
		t.Fatalf("huyang's build tools were not reported: %v", missing)
	}
}

func TestPrerequisitesUnderstandADerivedDistribution(t *testing.T) {
	report := prerequisites(t.TempDir(), map[string]string{"ID": "elementary", "ID_LIKE": "ubuntu debian"},
		scriptedRunner(nil))
	if report["family"] != "debian" {
		t.Fatalf("family = %v", report["family"])
	}
}
