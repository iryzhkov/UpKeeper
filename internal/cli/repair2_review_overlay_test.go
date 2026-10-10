package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIndependentDiagnosticWrongEpochActualCLI(t *testing.T) {
	for _, kind := range []string{"activity", "haiku"} {
		for _, fail := range []string{""} {
			t.Run(kind+"/"+fail, func(t *testing.T) {
				dir := t.TempDir()
				spool := useSpool(t)
				now := time.Now().UTC()
				workers := map[string]any{"version": "backlog.admin/v1", "kind": "workers", "workers": []any{map[string]any{"requirement": map[string]any{"workerId": "coord", "workerEpoch": "new"}, "health": "ready", "state": "draining", "stale": false, "snapshot": map[string]any{"workerId": "coord", "workerEpoch": "old", "coordinatorEpoch": 280, "connected": true, "observedAt": now.Add(-time.Second), "validUntil": now.Add(time.Minute), "inventory": map[string]any{"id": "coord", "health": "ready", "capabilities": []string{"turn-end-commands-v1"}, "telemetry": map[string]any{"running_attempts": 0}, "providers": []any{map[string]any{"instanceId": "claudeAgent", "available": true, "models": []string{"claude-haiku-5-5"}}}}}}}}
				raw, _ := json.Marshal(workers)
				if err := os.WriteFile(filepath.Join(dir, "workers.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte(`{"version":"backlog.admin/v1","kind":"coordinator-identity","coordinatorId":"homelab","health":"healthy","epoch":280}`), 0600); err != nil {
					t.Fatal(err)
				}
				script := "#!/bin/sh\ncase \"$*\" in\n'worker list --json') cat workers.json; "
				if fail == "workers" {
					script += "exit 42; "
				}
				script += ";;\n'coordinator identity --json') cat identity.json; "
				if fail == "identity" {
					script += "exit 42; "
				}
				script += ";;\n*) exit 99;;\nesac\n"
				if err := os.WriteFile(filepath.Join(dir, "t3-steward"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				cfg := map[string]any{
					"Version": "0.11.0-rc.122", "PreviousRelease": strings.Repeat("a", 40), "Checkout": dir, "StatePath": filepath.Join(dir, "state", "rollout.json"), "LocalHost": "last", "CoordinatorHost": "coord", "Canary": "canary", "LastHost": "last", "LaptopHost": "excluded", "Remotes": []string{"origin", "mirror"}, "FleetOrder": []string{"coord", "last"}, "TimerHosts": []string{"coord", "last", "excluded"}, "ActiveWorkers": []string{"canary", "last"}, "DrainedHosts": []string{"coord"}, "Targets": map[string]string{"last": "local", "coord": "coord", "canary": "canary", "excluded": "excluded"}, "QuietCommand": []string{"quiet"}, "BackupMaxAgeSeconds": 3600, "ReadinessSeconds": 180, "SoakSeconds": 1800,
					"ActivityCommand": []string{"upkeeper", "rollout", "activity", "--config", "config.json", "--json"}, "HaikuCheckCommand": []string{"upkeeper", "rollout", "haiku", "--config", "config.json", "--json"}, "DropinTestCommand": []string{"test-dropin"},
				}
				raw, _ = json.Marshal(cfg)
				path := filepath.Join(dir, "config.json")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				code, out, errOut := runSpooled([]string{"rollout", kind, "--config", path, "--json", "coord", "280"})
				if fail == "" {
					if code == 0 {
						t.Fatalf("UNSAFE actual CLI accepted mismatched worker epoch: %d %s %s", code, out, errOut)
					}
				} else if code == 0 || strings.Contains(out, "coordinator_epoch") {
					t.Fatalf("status42 accepted: %d %s %s", code, out, errOut)
				}
				if len(spoolFiles(t, spool)) != 0 {
					t.Fatal("diagnostic wrote feedback")
				}
				if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
					t.Fatal("diagnostic mutated rollout state", err)
				}
			})
		}
	}
}
