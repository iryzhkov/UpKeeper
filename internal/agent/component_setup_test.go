package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// adapterTestRunner is the injected process runner every adapter test uses. No
// test in this file starts a real program, touches a real home directory or
// talks to a real systemd.
type adapterTestRunner struct {
	handle func(command procx.Command) procx.Result
	calls  [][]string
}

func (r *adapterTestRunner) run(command procx.Command) procx.Result {
	r.calls = append(r.calls, command.Argv)
	if r.handle == nil {
		return procx.Result{}
	}
	return r.handle(command)
}

// ran reports whether a command containing every fragment was executed.
func (r *adapterTestRunner) ran(fragments ...string) bool {
	for _, argv := range r.calls {
		if adapterTestMatches(argv, fragments...) {
			return true
		}
	}
	return false
}

func adapterTestMatches(argv []string, fragments ...string) bool {
	joined := strings.Join(argv, " ")
	for _, fragment := range fragments {
		if !strings.Contains(joined, fragment) {
			return false
		}
	}
	return true
}

// adapterTestContext builds a context rooted in a throwaway home directory.
func adapterTestContext(t *testing.T, runner *adapterTestRunner) *Context {
	t.Helper()
	home := t.TempDir()
	return &Context{
		Home:       home,
		Host:       HostRef{Name: "testhost", OSID: "omarchy"},
		RunID:      "20260914T000000Z-test",
		Components: map[string]any{},
		Stored:     map[string]string{},
		// The search path points at a directory that does not exist, so no
		// test ever finds a real tool by accident.
		Env:     []string{"PATH=" + filepath.Join(home, "no-such-bin")},
		Runtime: filepath.Join(home, "run"),
		Runner:  runner.run,
	}
}

// adapterTestNumber builds a manifest number the way a decoded payload carries
// one, so that a test exercises the same type production code sees.
func adapterTestNumber(literal string) json.Number {
	return json.Number(literal)
}

func adapterTestWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	setupOldCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	setupNewCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	setupAhead     = "cccccccccccccccccccccccccccccccccccccccc"
)

// setupTestWorld is one host state the omarchy-setup adapter has to handle.
type setupTestWorld struct {
	head       string
	headAfter  string
	dirty      bool
	ancestorOK bool
	modules    []string
	// scriptFixes says whether running the modules repairs the managed file.
	scriptFixes bool
	// staleManaged puts a managed script in the checkout that the home
	// directory does not have.
	staleManaged bool
}

func setupTestPrepare(t *testing.T, world setupTestWorld) (*Context, *adapterTestRunner, map[string]any) {
	t.Helper()
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	checkout := context.Path(".local/share/omarchy-setup")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(checkout, "config/bin/helper")
	target := context.Path(".local/bin/helper")
	if world.staleManaged {
		adapterTestWrite(t, source, "#!/bin/sh\necho new\n")
		adapterTestWrite(t, target, "#!/bin/sh\necho old\n")
	}
	head := world.head
	runner.handle = func(command procx.Command) procx.Result {
		switch {
		case adapterTestMatches(command.Argv, "rev-parse", "HEAD"):
			return procx.Result{Stdout: head}
		case adapterTestMatches(command.Argv, "status", "--porcelain"):
			if world.dirty {
				return procx.Result{Stdout: " M lib/setup.sh"}
			}
			return procx.Result{}
		case adapterTestMatches(command.Argv, "pull", "--ff-only"):
			if world.headAfter != "" {
				head = world.headAfter
			}
			return procx.Result{}
		case adapterTestMatches(command.Argv, "merge-base", "--is-ancestor"):
			if world.ancestorOK {
				return procx.Result{}
			}
			return procx.Result{Code: 1}
		case adapterTestMatches(command.Argv, "bash"):
			if world.scriptFixes {
				adapterTestWrite(t, target, "#!/bin/sh\necho new\n")
			}
			return procx.Result{}
		default:
			return procx.Result{}
		}
	}
	modules := world.modules
	if modules == nil {
		modules = []string{"28-scripts"}
	}
	listed := make([]any, 0, len(modules))
	for _, module := range modules {
		listed = append(listed, module)
	}
	spec := map[string]any{
		"kind":    "git-checkout",
		"path":    ".local/share/omarchy-setup",
		"commit":  setupNewCommit,
		"ref":     "main",
		"modules": listed,
	}
	context.Components["omarchy-setup"] = spec
	return context, runner, spec
}

func TestSetupApply(t *testing.T) {
	cases := []struct {
		name        string
		world       setupTestWorld
		state       string
		errorText   string
		wantPull    bool
		wantModules bool
		action      string
	}{
		{
			name:  "a clean checkout on the pinned commit is converged",
			world: setupTestWorld{head: setupNewCommit},
			state: "converged",
		},
		{
			name:        "a drifting checkout is pulled forward and the modules run",
			world:       setupTestWorld{head: setupOldCommit, headAfter: setupNewCommit},
			state:       "updated",
			wantPull:    true,
			wantModules: true,
			action:      "git pull --ff-only",
		},
		{
			name:      "a dirty checkout is refused before anything is pulled",
			world:     setupTestWorld{head: setupOldCommit, dirty: true},
			errorText: "has local changes; refusing to pull over them",
		},
		{
			name: "a checkout that lands past the release is reported as ahead",
			world: setupTestWorld{
				head: setupOldCommit, headAfter: setupAhead, ancestorOK: true,
			},
			state:       "ahead",
			wantPull:    true,
			wantModules: true,
			action:      "checkout is ahead of the release at cccccccccccc",
		},
		{
			name: "a pull that lands somewhere unrelated is a failure",
			world: setupTestWorld{
				head: setupOldCommit, headAfter: setupAhead, ancestorOK: false,
			},
			errorText: "after pull HEAD is " + setupAhead + ", wanted " + setupNewCommit,
			wantPull:  true,
		},
		{
			name: "stale managed files re-run the modules on a current checkout",
			world: setupTestWorld{
				head: setupNewCommit, staleManaged: true, scriptFixes: true,
			},
			state:       "updated",
			wantModules: true,
			action:      "ran modules 28-scripts",
		},
		{
			name: "modules that still leave a managed file behind are a failure",
			world: setupTestWorld{
				head: setupNewCommit, staleManaged: true, scriptFixes: false,
			},
			errorText:   "managed files still differ after modules: .local/bin/helper",
			wantModules: true,
		},
		{
			name: "a historical release naming retired fleet modules is refused",
			world: setupTestWorld{
				head: setupOldCommit, headAfter: setupNewCommit,
				modules: []string{"28-scripts", "45-agents"},
			},
			errorText: "historical setup release includes retired fleet modules",
			wantPull:  true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context, runner, spec := setupTestPrepare(t, test.world)
			before, err := setupObserve(context, spec)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			actions := []string{}
			state, err := setupApply(context, spec, before, &actions)
			if test.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), test.errorText) {
					t.Fatalf("want error containing %q, got state %q and error %v",
						test.errorText, state, err)
				}
			} else {
				if err != nil {
					t.Fatalf("apply: %v", err)
				}
				if state != test.state {
					t.Fatalf("state = %q, want %q (actions %v)", state, test.state, actions)
				}
			}
			if runner.ran("pull", "--ff-only") != test.wantPull {
				t.Fatalf("pull performed = %v, want %v", !test.wantPull, test.wantPull)
			}
			if runner.ran("bash") != test.wantModules {
				t.Fatalf("modules run = %v, want %v", !test.wantModules, test.wantModules)
			}
			if test.action != "" && !adapterTestMatches(actions, test.action) {
				t.Fatalf("actions %v do not report %q", actions, test.action)
			}
		})
	}
}

func TestSetupApplyRefusesACheckoutThatIsNotAGitRepository(t *testing.T) {
	context, _, spec := setupTestPrepare(t, setupTestWorld{head: ""})
	before, err := setupObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if before["commit"] != nil {
		t.Fatalf("commit = %v, want nil", before["commit"])
	}
	actions := []string{}
	if _, err := setupApply(context, spec, before, &actions); err == nil ||
		!strings.Contains(err.Error(), "bootstrap omarchy-setup by hand first") {
		t.Fatalf("want a bootstrap refusal, got %v", err)
	}
}

func TestSetupDryRunChangesNothing(t *testing.T) {
	context, runner, spec := setupTestPrepare(t, setupTestWorld{
		head: setupOldCommit, headAfter: setupNewCommit,
	})
	context.DryRun = true
	before, err := setupObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	actions := []string{}
	state, err := setupApply(context, spec, before, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	if runner.ran("pull") || runner.ran("bash") {
		t.Fatalf("a dry run executed %v", runner.calls)
	}
	if !adapterTestMatches(actions, "would git pull --ff-only to aaaaaaaaaaaa") ||
		!adapterTestMatches(actions, "would run modules 28-scripts (stale: none)") {
		t.Fatalf("actions %v do not describe the preview", actions)
	}
}

// The agent-environment component owns the harness instruction file and the
// skills when the release carries it, and setup must not claim them as well.
func TestSetupManagedFilePairsYieldToAgentEnvironment(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	checkout := filepath.Join(context.Home, "setup")
	for _, relative := range []string{
		"config/claude/CLAUDE.md",
		"config/claude/skills/example/SKILL.md",
		"config/bin/helper",
	} {
		adapterTestWrite(t, filepath.Join(checkout, relative), "source")
	}
	context.Components["agent-environment"] = map[string]any{}
	pairs := setupManagedFilePairs(context, checkout)
	if len(pairs) != 1 ||
		!strings.HasSuffix(pairs[0][0], "config/bin/helper") {
		t.Fatalf("pairs = %v, want only the script", pairs)
	}
	delete(context.Components, "agent-environment")
	if pairs := setupManagedFilePairs(context, checkout); len(pairs) != 3 {
		t.Fatalf("pairs = %v, want all three managed files", pairs)
	}
}
