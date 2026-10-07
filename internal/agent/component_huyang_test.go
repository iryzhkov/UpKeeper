package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

const (
	huyangPinned = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	huyangLocal  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// huyangTestPatch replaces the probe and shortens the waits, so that no test
// starts a Huyang process or sleeps for the production bounds.
func huyangTestPatch(t *testing.T, probe func(*Context, string, int) huyangProbeResult) {
	t.Helper()
	savedProbe := huyangProbeFunc
	savedSettle, savedPoll, savedTimeout :=
		huyangSettleDelay, huyangSocketPoll, huyangSocketTimeout
	huyangProbeFunc = probe
	huyangSettleDelay = 0
	huyangSocketPoll = time.Millisecond
	huyangSocketTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		huyangProbeFunc = savedProbe
		huyangSettleDelay, huyangSocketPoll, huyangSocketTimeout =
			savedSettle, savedPoll, savedTimeout
	})
}

func huyangTestProbe(ok bool, tools int) func(*Context, string, int) huyangProbeResult {
	return func(*Context, string, int) huyangProbeResult {
		count := tools
		version := "1.4.0"
		return huyangProbeResult{OK: ok, Tools: &count, ServerVersion: &version}
	}
}

func huyangTestSpec() map[string]any {
	return map[string]any{
		"kind":    "git-checkout",
		"source":  "https://github.com/iryzhkov/huyang.git",
		"ref":     "main",
		"commit":  huyangPinned,
		"path":    ".local/share/huyang",
		"build":   []any{"make", "build"},
		"service": "huyang.service",
		"probe":   map[string]any{"kind": "huyang-mcp", "tools": adapterTestNumber("19")},
	}
}

// huyangTestWorld is one host state the huyang adapter has to handle.
type huyangTestWorld struct {
	head string
	// aheadOfPin makes the pinned commit an ancestor of the checkout.
	aheadOfPin bool
	// containsPin makes the checkout an ancestor of the pinned commit.
	containsPin bool
	dirty       bool
	active      bool
	socket      bool
	// buildFailures is how many of the builds fail, counted from the first.
	buildFailures int
}

func huyangTestPrepare(t *testing.T, world huyangTestWorld) (*Context, *adapterTestRunner) {
	t.Helper()
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	// Upgrade and rollback fixtures represent a provisioned build host.
	// Keep lookup isolated from the operator's tools while declaring the
	// prerequisites that Apply now checks at its build decision.
	bin := freshTestTools(t, context.Home, "git", "go", "make")
	context.Env = []string{"PATH=" + bin}
	checkout := context.Path(".local/share/huyang")
	if err := os.MkdirAll(filepath.Join(checkout, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if world.socket {
		adapterTestWrite(t, huyangSocket(context), "")
	}
	head := world.head
	builds := 0
	runner.handle = func(command procx.Command) procx.Result {
		last := command.Argv[len(command.Argv)-1]
		switch {
		case adapterTestMatches(command.Argv, "go", "env", "GOVERSION"):
			return procx.Result{Stdout: "go1.26.8\n"}
		case adapterTestMatches(command.Argv, "rev-parse", "HEAD"):
			return procx.Result{Stdout: head}
		case adapterTestMatches(command.Argv, "status", "--porcelain"):
			if world.dirty {
				return procx.Result{Stdout: " M internal/agent.go"}
			}
			return procx.Result{}
		case adapterTestMatches(command.Argv, "merge-base", "--is-ancestor"):
			ancestor := command.Argv[len(command.Argv)-2]
			if ancestor == huyangPinned && world.aheadOfPin {
				return procx.Result{}
			}
			if ancestor == world.head && world.containsPin {
				return procx.Result{}
			}
			return procx.Result{Code: 1}
		case adapterTestMatches(command.Argv, "checkout", "--detach"):
			head = last
			return procx.Result{}
		case adapterTestMatches(command.Argv, "make", "build"):
			builds++
			if builds <= world.buildFailures {
				return procx.Result{Code: 2, Stderr: "compile error"}
			}
			return procx.Result{}
		case adapterTestMatches(command.Argv, "is-active"):
			if world.active {
				return procx.Result{Stdout: "active"}
			}
			return procx.Result{Stdout: "inactive"}
		default:
			return procx.Result{}
		}
	}
	context.Components["huyang"] = huyangTestSpec()
	return context, runner
}

func TestHuyangApply(t *testing.T) {
	cases := []struct {
		name      string
		world     huyangTestWorld
		probeOK   bool
		state     string
		errorText string
		wantFetch bool
		action    string
	}{
		{
			name: "a pinned checkout whose service answers the probe is converged",
			world: huyangTestWorld{
				head: huyangPinned, active: true, socket: true, containsPin: true,
			},
			probeOK: true,
			state:   "converged",
			action:  "mcp probe tools=19",
		},
		{
			name: "a checkout past the pin is preserved rather than moved back",
			world: huyangTestWorld{
				head: huyangLocal, active: true, socket: true, aheadOfPin: true,
			},
			probeOK: true,
			state:   "ahead",
			action:  "normal pull preserves newer Huyang state",
		},
		{
			name: "a checkout that diverges from the pin is preserved",
			world: huyangTestWorld{
				head: huyangLocal, active: true, socket: true,
			},
			probeOK:   true,
			state:     "ahead",
			wantFetch: true,
			action:    "normal pull preserves local Huyang work",
		},
		{
			name: "a dirty checkout is refused before anything moves",
			world: huyangTestWorld{
				head: huyangLocal, dirty: true, active: true, socket: true,
			},
			probeOK:   true,
			errorText: "has local changes; refusing to move it",
		},
		{
			name: "a good build and restart updates the checkout",
			world: huyangTestWorld{
				head: huyangLocal, active: true, socket: true, containsPin: true,
			},
			probeOK:   true,
			state:     "updated",
			wantFetch: true,
			action:    "built",
		},
		{
			name: "a failed build is rolled back to the commit the run found",
			world: huyangTestWorld{
				head: huyangLocal, active: true, socket: true, containsPin: true,
				buildFailures: 1,
			},
			probeOK:   true,
			errorText: "build failed",
			wantFetch: true,
			action:    "rolling back to bbbbbbbbbbbb",
		},
		{
			name: "a rollback that also fails is reported as a failed rollback",
			world: huyangTestWorld{
				head: huyangLocal, active: true, socket: true, containsPin: true,
				buildFailures: 2,
			},
			probeOK:   true,
			errorText: "rollback failed: build failed",
			wantFetch: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context, runner := huyangTestPrepare(t, test.world)
			huyangTestPatch(t, huyangTestProbe(test.probeOK, 19))
			spec := context.Spec("huyang")
			before, err := huyangObserve(context, spec)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			actions := []string{}
			state, err := huyangApply(context, spec, before, &actions)
			if test.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), test.errorText) {
					t.Fatalf("want error containing %q, got %q and %v (actions %v)",
						test.errorText, state, err, actions)
				}
			} else if err != nil || state != test.state {
				t.Fatalf("state = %q, error = %v, want %q (actions %v)",
					state, err, test.state, actions)
			}
			if runner.ran("fetch", "origin", "main") != test.wantFetch {
				t.Fatalf("fetch performed = %v, want %v", !test.wantFetch, test.wantFetch)
			}
			if test.action != "" && !adapterTestMatches(actions, test.action) {
				t.Fatalf("actions %v do not report %q", actions, test.action)
			}
		})
	}
}

func TestHuyangRollbackClassifiesTheOutcome(t *testing.T) {
	context, _ := huyangTestPrepare(t, huyangTestWorld{
		head: huyangLocal, active: true, socket: true, containsPin: true,
		buildFailures: 1,
	})
	huyangTestPatch(t, huyangTestProbe(true, 19))
	spec := context.Spec("huyang")
	before, err := huyangObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	actions := []string{}
	_, applyError := huyangApply(context, spec, before, &actions)
	state, message := classify(applyError)
	if state != "rolled-back" || !strings.Contains(message, "build failed") {
		t.Fatalf("state = %q, message = %q", state, message)
	}
}

func TestHuyangRollbackFailureClassifiesTheOutcome(t *testing.T) {
	context, _ := huyangTestPrepare(t, huyangTestWorld{
		head: huyangLocal, active: true, socket: true, containsPin: true,
		buildFailures: 2,
	})
	huyangTestPatch(t, huyangTestProbe(true, 19))
	spec := context.Spec("huyang")
	before, err := huyangObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	actions := []string{}
	_, applyError := huyangApply(context, spec, before, &actions)
	if state, _ := classify(applyError); state != "rollback-failed" {
		t.Fatalf("state = %q, want rollback-failed", state)
	}
}

func TestHuyangDryRunDescribesTheUpgradeWithoutTouchingTheCheckout(t *testing.T) {
	context, runner := huyangTestPrepare(t, huyangTestWorld{
		head: huyangLocal, active: true, socket: true, containsPin: true,
	})
	huyangTestPatch(t, huyangTestProbe(true, 19))
	context.DryRun = true
	spec := context.Spec("huyang")
	before, err := huyangObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	actions := []string{}
	state, err := huyangApply(context, spec, before, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	if runner.ran("fetch") || runner.ran("make") {
		t.Fatalf("a dry run executed %v", runner.calls)
	}
	if !adapterTestMatches(actions,
		"would fetch origin main, check out aaaaaaaaaaaa, build, restart") {
		t.Fatalf("actions %v do not describe the preview", actions)
	}
}

func TestHuyangBootstrapPreservesPathsItDidNotCreate(t *testing.T) {
	cases := []struct {
		name      string
		prepare   func(t *testing.T, context *Context)
		errorText string
	}{
		{
			name: "a directory where the checkout belongs is preserved",
			prepare: func(t *testing.T, context *Context) {
				if err := os.MkdirAll(context.Path(".local/share/huyang"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			errorText: "exists but is not a Git checkout; preserving it",
		},
		{
			name: "a dangling symlink where the checkout belongs is preserved",
			prepare: func(t *testing.T, context *Context) {
				path := context.Path(".local/share/huyang")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(context.Path("absent"), path); err != nil {
					t.Fatal(err)
				}
			},
			errorText: "exists but is not a Git checkout; preserving it",
		},
		{
			name: "a unit without a checkout is inspected by hand",
			prepare: func(t *testing.T, context *Context) {
				adapterTestWrite(t,
					context.Path(".config/systemd/user/huyang.service"), "custom")
			},
			errorText: "a user unit exists; inspect it first",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &adapterTestRunner{}
			context := adapterTestContext(t, runner)
			context.Components["huyang"] = huyangTestSpec()
			test.prepare(t, context)
			actions := []string{}
			_, err := huyangBootstrap(context, context.Spec("huyang"), &actions)
			if err == nil || !strings.Contains(err.Error(), test.errorText) {
				t.Fatalf("want error containing %q, got %v", test.errorText, err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("a refusal executed %v", runner.calls)
			}
		})
	}
}

func TestHuyangBootstrapRefusesAHostWithoutBuildTools(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	context.Components["huyang"] = huyangTestSpec()
	actions := []string{}
	_, err := huyangBootstrap(context, context.Spec("huyang"), &actions)
	if err == nil || !strings.Contains(err.Error(), "Huyang build prerequisite missing: git") {
		t.Fatalf("want a prerequisite refusal, got %v", err)
	}
}

func TestHuyangBootstrapDryRunWritesNothing(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	context.DryRun = true
	context.Components["huyang"] = huyangTestSpec()
	bin := filepath.Join(context.Home, "tools")
	for _, tool := range []string{"git", "go", "make"} {
		adapterTestWrite(t, filepath.Join(bin, tool), "#!/bin/sh\n")
		if err := os.Chmod(filepath.Join(bin, tool), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	context.Env = []string{"PATH=" + bin}
	actions := []string{}
	state, err := huyangBootstrap(context, context.Spec("huyang"), &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	if _, err := os.Stat(context.Path(".local/share/huyang")); err == nil {
		t.Fatal("a preview created the checkout")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a preview executed %v", runner.calls)
	}
}

func TestHuyangWaitForSocketRefusesAServiceThatNeverListens(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	huyangTestPatch(t, huyangTestProbe(true, 19))
	if err := huyangWaitForSocket(context); err == nil ||
		!strings.Contains(err.Error(), "control socket did not appear after restart") {
		t.Fatalf("want a socket refusal, got %v", err)
	}
}

func TestHuyangProbeReprMatchesTheReportedText(t *testing.T) {
	tools := 19
	version := "1.4.0"
	good := huyangProbeRepr(huyangProbeResult{OK: true, Tools: &tools, ServerVersion: &version})
	if good != "{'ok': True, 'tools': 19, 'server_version': '1.4.0'}" {
		t.Fatalf("repr = %s", good)
	}
	bad := huyangProbeRepr(huyangProbeResult{Error: "no such file"})
	if bad != "{'ok': False, 'error': 'no such file'}" {
		t.Fatalf("repr = %s", bad)
	}
}
