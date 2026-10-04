package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/rpcprobe"
)

func retirementContext(t *testing.T) (*Context, string, string, string) {
	t.Helper()
	home := t.TempDir()
	base := filepath.Join(home, ".local/share/upkeeper-tools")
	old := filepath.Join(base, "releases/historical")
	source := filepath.Join(old, toolingBacklogSource)
	if err := agentenv.AtomicWrite(source, []byte("historical wrapper\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".local/bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	c := &Context{Home: home, RunID: "retirement", Runner: func(procx.Command) procx.Result { return procx.Result{} }}
	priorProbe, priorModels := toolingProbe, toolingModels
	toolingProbe = func(*Context, string, *release.Tooling) error { return nil }
	toolingModels = func([]string, []string, string) ([]string, error) { return []string{"fixture"}, nil }
	t.Cleanup(func() { toolingProbe, toolingModels = priorProbe, priorModels })
	return c, base, old, filepath.Join(bin, "t3-backlog")
}

func TestToolingRetirementApplyAndRollback(t *testing.T) {
	for _, kind := range []string{"absent", "owned", "dangling", "foreign", "regular"} {
		t.Run(kind, func(t *testing.T) {
			c, base, old, path := retirementContext(t)
			managed := filepath.Join(base, "current", toolingBacklogSource)
			switch kind {
			case "owned", "dangling":
				if err := os.Symlink(managed, path); err != nil {
					t.Fatal(err)
				}
				if kind == "dangling" {
					// The old immutable source still exists, but current has already moved.
					if err := swapLink(filepath.Join(base, "current"), filepath.Join(base, "missing")); err != nil {
						t.Fatal(err)
					}
				}
			case "foreign":
				if err := os.Symlink("/outside/operator-wrapper", path); err != nil {
					t.Fatal(err)
				}
			case "regular":
				if err := os.WriteFile(path, []byte("operator wrapper"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			original, err := snapshotLink(path)
			if err != nil {
				t.Fatal(err)
			}
			spec := validToolingSpec(t)
			before, err := toolingObserve(c, spec)
			if err != nil {
				t.Fatal(err)
			}
			actions := []string{}
			if _, err := toolingApply(c, spec, before, &actions); err != nil {
				t.Fatal(err)
			}
			after, err := toolingObserve(c, spec)
			if err != nil || len(after["drift"].([]string)) != 0 {
				t.Fatalf("observe: %v %v", after, err)
			}
			actual, err := snapshotLink(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "foreign" || kind == "regular" {
				if !reflect.DeepEqual(actual, original) || after["t3_backlog_binding"] != "preserved foreign binding" {
					t.Fatal("foreign binding not honestly preserved")
				}
			} else if actual.Exists {
				t.Fatal("retired binding remains")
			}
			state, err := toolingApply(c, spec, after, &actions)
			if err != nil || state != "converged" {
				t.Fatalf("repeat: %s %v", state, err)
			}
			source := filepath.Join(old, toolingBacklogSource)
			if data, err := os.ReadFile(source); err != nil || string(data) != "historical wrapper\n" {
				t.Fatal("historical bundle changed")
			}
			if kind == "owned" || kind == "dangling" {
				// Historical release selection uses the ordinary desired-link activation.
				historical := &release.Tooling{Files: []release.ToolFile{{Path: toolingBacklogSource, Mode: 0755}}}
				if err := activateTooling(c, base, old, historical); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "historical wrapper\n" {
					t.Fatalf("rollback wrapper: %q %v", data, err)
				}
			}
		})
	}
}

func TestToolingRetirementObserveDryRunStaleCurrent(t *testing.T) {
	c, base, _, path := retirementContext(t)
	spec := validToolingSpec(t)
	actions := []string{}
	if _, err := toolingApply(c, spec, map[string]any{}, &actions); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "current", toolingBacklogSource), path); err != nil {
		t.Fatal(err)
	}
	before, err := toolingObserve(c, spec)
	if err != nil || !strings.Contains(strings.Join(before["drift"].([]string), ";"), "retired command binding remains") {
		t.Fatalf("stale observe: %v %v", before, err)
	}
	original, _ := snapshotLink(path)
	c.DryRun = true
	c.Runner = func(procx.Command) procx.Result { t.Fatal("dry run executed command"); return procx.Result{} }
	state, err := toolingApply(c, spec, before, &actions)
	now, _ := snapshotLink(path)
	if err != nil || state != "updated" || !reflect.DeepEqual(now, original) {
		t.Fatalf("dry run: %s %v", state, err)
	}
	c.DryRun = false
	c.Runner = func(procx.Command) procx.Result { return procx.Result{} }
	if _, err := toolingApply(c, spec, before, &actions); err != nil {
		t.Fatal(err)
	}
}

func TestToolingRetirementCompensation(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "probe-failure", true: "concurrent-recreation"}[concurrent], func(t *testing.T) {
			c, base, old, path := retirementContext(t)
			managed := filepath.Join(base, "current", toolingBacklogSource)
			if err := os.Symlink(managed, path); err != nil {
				t.Fatal(err)
			}
			toolingProbe = func(*Context, string, *release.Tooling) error {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("probe ran before retirement")
				}
				if concurrent {
					if err := os.Symlink("operator-choice", path); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("postactivation health failure")
			}
			err := activateTooling(c, base, filepath.Join(base, "new"), &release.Tooling{})
			var rb *RolledBack
			if concurrent {
				if err == nil || !strings.Contains(err.Error(), "rollback failed: concurrent change") {
					t.Fatalf("expected actionable refusal: %v", err)
				}
				if target, _ := os.Readlink(path); target != "operator-choice" {
					t.Fatal("actor overwritten")
				}
			} else {
				if !errors.As(err, &rb) {
					t.Fatalf("expected compensation: %v", err)
				}
				if target, _ := os.Readlink(path); target != managed {
					t.Fatal("retirement not restored")
				}
				if target, _ := os.Readlink(filepath.Join(base, "current")); target != old {
					t.Fatal("current not restored")
				}
			}
			backup := c.Path(".local/state/dev-fleet/tooling", c.RunID, "bindings.json")
			data, err := os.ReadFile(backup)
			if err != nil {
				t.Fatal(err)
			}
			var snapshots []linkSnapshot
			if err := json.Unmarshal(data, &snapshots); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, s := range snapshots {
				if s.Path == path && s.Target == managed {
					found = true
				}
			}
			if !found {
				t.Fatal("retirement missing from compensation snapshot")
			}
			if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("backup not private")
			}
		})
	}
}

func TestToolingRetirementRejectsOutsideBin(t *testing.T) {
	c, base, _, path := retirementContext(t)
	if err := os.Remove(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(outside, "t3-backlog")
	if err := os.Symlink(filepath.Join(base, "current", toolingBacklogSource), foreign); err != nil {
		t.Fatal(err)
	}
	if err := activateTooling(c, base, filepath.Join(base, "new"), &release.Tooling{}); err == nil {
		t.Fatal("outside bin accepted")
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatal("outside path touched")
	}
}

func TestToolingBacklogConditionalProbe(t *testing.T) {
	for _, included := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "included"}[included], func(t *testing.T) {
			c, _, _, _ := retirementContext(t)
			priorExchange := toolingExchange
			t.Cleanup(func() { toolingExchange = priorExchange })
			tool := &release.Tooling{Python: []string{"mcp==1.0.0"}}
			if included {
				tool.Files = append(tool.Files, release.ToolFile{Path: toolingBacklogSource, Mode: 0755, Content: base64.StdEncoding.EncodeToString([]byte("broken"))})
			}
			var commands []string
			c.Runner = func(cmd procx.Command) procx.Result {
				commands = append(commands, strings.Join(cmd.Argv, " "))
				if strings.HasSuffix(cmd.Argv[0], "/t3-backlog") {
					return procx.Result{Code: 1}
				}
				output := ""
				if cmd.Argv[len(cmd.Argv)-1] == "freeze" {
					output = "mcp==1.0.0\n"
				}
				return procx.Result{Stdout: output}
			}
			mcpCalls := 0
			toolingExchange = func(argv, env []string, dir string, codex bool, calls []rpcprobe.Call, timeout time.Duration) ([]map[string]any, error) {
				mcpCalls++
				if len(calls) != 2 || calls[0].Method != "tools/list" || calls[1].Method != "tools/call" {
					t.Fatal("MCP health checks changed")
				}
				return []map[string]any{{"tools": []any{"fixture"}}, {}}, nil
			}
			modelCalls := 0
			toolingModels = func([]string, []string, string) ([]string, error) { modelCalls++; return []string{"fixture"}, nil }
			err := probeTooling(c, "/bundle", tool)
			if included {
				if err == nil || !strings.Contains(err.Error(), "t3-backlog") {
					t.Fatalf("included broken wrapper accepted: %v", err)
				}
			} else if err != nil || mcpCalls != 2 || modelCalls != 1 {
				t.Fatalf("mandatory checks: %v mcp=%d model=%d", err, mcpCalls, modelCalls)
			}
			for _, name := range []string{"ha", "toolfeedback", "feed", "pve", "unifi"} {
				if !strings.Contains(strings.Join(commands, "\n"), "/homelab-cli/bin/"+name+" --help") {
					t.Fatalf("missing mandatory %s", name)
				}
			}
			if !included && strings.Contains(strings.Join(commands, "\n"), "/t3-backlog") {
				t.Fatal("omitted wrapper probed")
			}
			// Source omission must never hide a broken mandatory CLI.
			c.Runner = func(cmd procx.Command) procx.Result { return procx.Result{Code: 1} }
			if err := probeTooling(c, "/bundle", tool); err == nil || !strings.Contains(err.Error(), "ha help") {
				t.Fatalf("mandatory failure hidden: %v", err)
			}
		})
	}
}
