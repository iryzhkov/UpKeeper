package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func validToolingSpec(t *testing.T) map[string]any {
	t.Helper()
	bundle := release.Tooling{Kind: release.ToolingComponent, ModelPolicy: "discover", Python: []string{"mcp==1.0.0"}}
	for _, path := range []string{"homelab-cli/bin/ha", "homelab-cli/bin/toolfeedback", "homelab-cli/lib/common.sh", "ov-memory/server.py", "ov-memory/friction.py", "local-file-llm/server.mjs", "local-file-llm/package.json", "local-file-llm/package-lock.json"} {
		data := []byte("fixture\n")
		bundle.Files = append(bundle.Files, release.ToolFile{Path: path, Content: base64.StdEncoding.EncodeToString(data), SHA256: agentenv.Digest(data), Mode: 0644})
	}
	data, _ := json.Marshal(bundle)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestToolingDryRunDoesNotUpgradeOrStage(t *testing.T) {
	home := t.TempDir()
	calls := 0
	c := &Context{Home: home, DryRun: true, Runner: func(procx.Command) procx.Result { calls++; return procx.Result{} }}
	actions := []string{}
	state, err := toolingApply(c, validToolingSpec(t), map[string]any{"drift": []string{"missing"}}, &actions)
	if err != nil || state != "updated" || calls != 0 {
		t.Fatalf("dry run: %s %v calls=%d", state, err, calls)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatal("dry run wrote files")
	}
}

func TestToolingFailedMiseUpgradeStopsBeforeStaging(t *testing.T) {
	home := t.TempDir()
	var commands [][]string
	c := &Context{Home: home, Runner: func(cmd procx.Command) procx.Result {
		commands = append(commands, cmd.Argv)
		return procx.Result{Code: 1}
	}}
	actions := []string{}
	_, err := toolingApply(c, validToolingSpec(t), map[string]any{}, &actions)
	if err == nil {
		t.Fatal("failed mise upgrade accepted")
	}
	if !reflect.DeepEqual(commands, [][]string{{"mise", "upgrade", "--yes", "--no-prune"}}) {
		t.Fatalf("commands %v", commands)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatal("failed upgrade staged files")
	}
}

func TestObserveReleaseNeverCallsApply(t *testing.T) {
	name := "status-test"
	prior, had := handlers[name]
	t.Cleanup(func() {
		if had {
			handlers[name] = prior
		} else {
			delete(handlers, name)
		}
	})
	calls := 0
	Register(name, Handler{
		Observe: func(*Context, map[string]any) (map[string]any, error) { return map[string]any{}, nil },
		Apply: func(*Context, map[string]any, map[string]any, *[]string) (string, error) {
			calls++
			return "", errors.New("mutation")
		},
	})
	c := &Context{Payload: Payload{ObserveOnly: true}, Home: t.TempDir(), Selected: []string{name}, Components: map[string]any{name: map[string]any{}}}
	result := Execute(c)
	if calls != 0 || result["observe_only"] != true {
		t.Fatal("status called a mutation handler")
	}
	entries, _ := os.ReadDir(c.Home)
	if len(entries) != 0 {
		t.Fatal("status wrote state")
	}
}

func TestToolingActivationRestoresBindingsWhenProbeFails(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, ".local/share/upkeeper-tools")
	bin := filepath.Join(home, ".local/bin")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(base, "old")
	if err := os.Symlink(old, filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	original := []byte("#!/bin/sh\necho old\n")
	if err := os.WriteFile(filepath.Join(bin, "ha"), original, 0755); err != nil {
		t.Fatal(err)
	}
	priorProbe := toolingProbe
	toolingProbe = func(*Context, string, *release.Tooling) error { return errors.New("health failure") }
	t.Cleanup(func() { toolingProbe = priorProbe })
	c := &Context{Home: home, RunID: "test-rollback"}
	tool := &release.Tooling{Files: []release.ToolFile{{Path: "homelab-cli/bin/ha", Mode: 0755}}}
	err := activateTooling(c, base, filepath.Join(base, "new"), tool)
	var rb *RolledBack
	if !errors.As(err, &rb) {
		t.Fatalf("expected compensation: %v", err)
	}
	current, _ := os.Readlink(filepath.Join(base, "current"))
	if current != old {
		t.Fatal("did not restore active bundle")
	}
	data, _ := os.ReadFile(filepath.Join(bin, "ha"))
	if string(data) != string(original) {
		t.Fatal("did not restore command")
	}
	if _, err := os.Lstat(filepath.Join(bin, "ov-memory-mcp")); !os.IsNotExist(err) {
		t.Fatal("new binding survived rollback")
	}
}

func TestToolingRollbackPreservesConcurrentChange(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, ".local/share/upkeeper-tools")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	priorProbe := toolingProbe
	toolingProbe = func(*Context, string, *release.Tooling) error {
		path := filepath.Join(base, "current")
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := os.Symlink("operator-choice", path); err != nil {
			return err
		}
		return errors.New("concurrent change")
	}
	t.Cleanup(func() { toolingProbe = priorProbe })
	err := activateTooling(&Context{Home: home, RunID: "concurrent"}, base, filepath.Join(base, "new"), &release.Tooling{})
	if err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("expected refusal: %v", err)
	}
	current, _ := os.Readlink(filepath.Join(base, "current"))
	if current != "operator-choice" {
		t.Fatal("clobbered concurrent work")
	}
}

// Opt-in integration validation stages real source/dependency bytes and speaks
// only tools/list, memory_status and model/list. It never activates the bundle,
// writes memory, performs inference or restarts a service.
func TestLiveToolingStageAndProbe(t *testing.T) {
	if os.Getenv("UPKEEPER_TEST_LIVE_TOOLING") != "1" {
		t.Skip("opt-in live integration")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := release.CaptureTooling(home, procx.Run)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := release.DecodeTooling(raw)
	if err != nil {
		t.Fatal(err)
	}
	c := &Context{Home: home, Env: agentEnvironment(home), Runner: procx.Run}
	dir := filepath.Join(t.TempDir(), "bundle")
	if err := stageTooling(c, dir, tool); err != nil {
		t.Fatal(err)
	}
	if err := probeTooling(c, dir, tool); err != nil {
		t.Fatal(err)
	}
	if err := verifyToolFiles(c, dir, tool); err != nil {
		t.Fatal(err)
	}
}
