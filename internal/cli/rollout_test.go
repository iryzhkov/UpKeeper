package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRolloutReadOnlyEntryPointNeverSpools(t *testing.T) {
	dir := useSpool(t)
	for _, args := range [][]string{{"rollout", "check", "--config", "missing"}, {"rollout", "plan", "--config", "missing"}, {"rollout", "run", "--help"}} {
		runSpooled(args)
	}
	if len(spoolFiles(t, dir)) != 0 {
		t.Fatal("read-only command wrote feedback")
	}
}

func TestRolloutPlanIsReadOnlyThroughBinaryEntryPoint(t *testing.T) {
	dir := t.TempDir()
	spool := useSpool(t)
	cfg := map[string]any{
		"Version": "0.11.0-rc.119", "PreviousRelease": strings.Repeat("a", 40), "Checkout": dir, "StatePath": filepath.Join(dir, "state", "rollout.json"), "LocalHost": "last", "CoordinatorHost": "coord", "Canary": "canary", "LastHost": "last", "LaptopHost": "excluded", "Remotes": []string{"origin", "mirror"}, "FleetOrder": []string{"coord", "last"}, "TimerHosts": []string{"coord", "last", "excluded"}, "ActiveWorkers": []string{"canary", "last"}, "DrainedHosts": []string{"coord"}, "Targets": map[string]string{"last": "local", "coord": "coord", "canary": "canary", "excluded": "excluded"}, "QuietCommand": []string{"quiet"}, "BackupMaxAgeSeconds": 3600, "ReadinessSeconds": 180, "SoakSeconds": 1800,
	}
	raw, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runSpooled([]string{"rollout", "plan", "--config", path, "--json"})
	if code != 0 || !strings.Contains(out, "rollback") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatal("plan wrote files", files)
	}
	if len(spoolFiles(t, spool)) != 0 {
		t.Fatal("plan wrote feedback")
	}
}

func TestRolloutArgumentContract(t *testing.T) {
	for _, args := range [][]string{{"rollout"}, {"rollout", "run"}, {"rollout", "check", "--yes", "--config", "config.json"}, {"rollout", "unknown", "--config", "config.json"}, {"rollout", "run", "--config", "config.json", "--force"}} {
		if _, err := parse(args, nil); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, action := range []string{"check", "plan", "run"} {
		p, err := parse([]string{"rollout", action, "--config", "config.json"}, nil)
		if err != nil || p.rolloutAction != action {
			t.Fatalf("%s: %v", action, err)
		}
	}
	p, err := parse([]string{"rollout", "run", "--config", "config.json", "--yes"}, nil)
	if err != nil || !p.rolloutYes {
		t.Fatal(err)
	}
}
