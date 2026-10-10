package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRolloutDiagnosticActualCLI(t *testing.T) {
	for _, kind := range []string{"activity", "haiku"} {
		for _, fail := range []string{"", "identity", "workers", "wrong-worker-epoch", "missing-required-epoch", "missing-snapshot-epoch", "missing-worker-id", "missing-snapshot-id", "missing-inventory-id", "missing-coordinator-epoch", "wrong-coordinator-epoch", "duplicate-worker", "unsupported-capability", "expired-final-read", "stale-final-age", "stale-assignment", "future-assignment", "final-producer-failure"} {
			t.Run(kind+"/"+fail, func(t *testing.T) {
				dir := t.TempDir()
				spool := useSpool(t)
				now := time.Now().UTC()
				workers := map[string]any{"version": "backlog.admin/v1", "kind": "workers", "workers": []any{map[string]any{"requirement": map[string]any{"workerId": "canary", "workerEpoch": "process"}, "health": "ready", "state": "observed", "stale": false, "snapshot": map[string]any{"workerId": "canary", "workerEpoch": "process", "coordinatorEpoch": 280, "connected": true, "observedAt": now.Add(-time.Second), "validUntil": now.Add(time.Minute), "inventory": map[string]any{"id": "canary", "health": "ready", "capabilities": []string{"turn-end-commands-v1"}, "telemetry": map[string]any{"running_attempts": 0}, "providers": []any{map[string]any{"instanceId": "claudeAgent", "available": true, "models": []string{"claude-haiku-5-5"}}}}}}}}
				w := workers["workers"].([]any)[0].(map[string]any)
				w["requirement"].(map[string]any)["workerId"] = "coord"
				w["state"] = "draining"
				s := w["snapshot"].(map[string]any)
				s["workerId"] = "coord"
				s["inventory"].(map[string]any)["id"] = "coord"
				switch fail {
				case "wrong-worker-epoch":
					w["requirement"].(map[string]any)["workerEpoch"] = "new"
					s["workerEpoch"] = "old"
				case "missing-required-epoch":
					delete(w["requirement"].(map[string]any), "workerEpoch")
				case "missing-snapshot-epoch":
					delete(s, "workerEpoch")
				case "missing-worker-id":
					delete(w["requirement"].(map[string]any), "workerId")
				case "missing-snapshot-id":
					delete(s, "workerId")
				case "missing-inventory-id":
					delete(s["inventory"].(map[string]any), "id")
				case "missing-coordinator-epoch":
					delete(s, "coordinatorEpoch")
				case "wrong-coordinator-epoch":
					s["coordinatorEpoch"] = 279
				case "duplicate-worker":
					workers["workers"] = []any{w, w}
				case "unsupported-capability":
					s["inventory"].(map[string]any)["capabilities"] = []string{"older-peer"}
				case "expired-final-read":
					s["observedAt"] = now.Add(-59 * time.Second)
					s["validUntil"] = now.Add(time.Second)
				case "stale-final-age":
					s["observedAt"] = now.Add(-59 * time.Second)
				case "stale-assignment", "future-assignment":
					stamp := now.Add(-59 * time.Second)
					if fail == "future-assignment" {
						stamp = now.Add(time.Minute)
					}
					s["assignments"] = []any{map[string]any{"assignmentId": "done", "state": "completed", "observedAt": stamp, "journal": map[string]any{"phase": "completed", "updatedAt": now.Add(-time.Hour)}}}
				}
				raw, _ := json.Marshal(workers)
				if err := os.WriteFile(filepath.Join(dir, "workers.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte(`{"version":"backlog.admin/v1","kind":"coordinator-identity","coordinatorId":"homelab","health":"healthy","epoch":280}`), 0600); err != nil {
					t.Fatal(err)
				}
				script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> producer-calls\ncase \"$*\" in\n'worker list --json') cat workers.json; "
				if fail == "workers" {
					script += "exit 42; "
				}
				script += ";;\n'coordinator identity --json') if test -f identity-read; then :; "
				if fail == "expired-final-read" || fail == "stale-final-age" || fail == "stale-assignment" {
					script += "sleep 2; "
				}
				if fail == "final-producer-failure" {
					script += "cat identity.json; exit 42; "
				}
				script += "fi; touch identity-read; cat identity.json; "
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
					if code != 0 || !strings.Contains(out, `"coordinator_epoch":"280"`) {
						t.Fatalf("%d %s %s", code, out, errOut)
					}
				} else if code == 0 || strings.Contains(out, "coordinator_epoch") {
					t.Fatalf("status42 accepted: %d %s %s", code, out, errOut)
				}
				calls, err := os.ReadFile(filepath.Join(dir, "producer-calls"))
				if err != nil {
					t.Fatal(err)
				}
				for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
					if call != "worker list --json" && call != "coordinator identity --json" {
						t.Fatalf("diagnostic launched unexpected command: %s", call)
					}
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
func TestRolloutDiagnosticArgvRefusals(t *testing.T) {
	for _, args := range [][]string{
		{"rollout", "activity", "--config", "c", "--json", "canary", "0"},
		{"rollout", "haiku", "--config", "c", "--json", "canary", "280", "extra"},
		{"rollout", "activity", "--config", "c", "--yes", "--json", "canary", "280"},
		{"rollout", "haiku", "--config", "c", "--json", "--json", "canary", "280"},
		{"rollout", "activity", "--config", "c", "canary", "280"},
	} {
		if _, err := parse(args, nil); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
