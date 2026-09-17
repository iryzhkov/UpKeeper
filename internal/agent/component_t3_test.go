package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

const (
	t3TestPinned   = "0.0.38"
	t3TestPrevious = "0.0.37"
)

// t3TestHost is the running T3 one convergence has to reason about.
type t3TestHost struct {
	installed string
	served    string
	running   int
	checkOK   bool
	active    bool
	// runningAfterInstall is how many threads the steward reports once the
	// package has been replaced, which is a second, independent observation.
	runningAfterInstall int
}

func t3TestSpec() map[string]any {
	return map[string]any{
		"kind":          "npm-global",
		"version":       t3TestPinned,
		"service":       "t3code.service",
		"compatibility": map[string]any{"min": t3TestPinned, "max": t3TestPinned},
	}
}

func t3TestPrepare(t *testing.T, host t3TestHost) (*Context, *adapterTestRunner) {
	t.Helper()
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	context.Components["t3-steward"] = stewardTestSpec(false)
	context.Components["t3"] = t3TestSpec()
	installed := host.installed
	served := host.served
	running := host.running
	runner.handle = func(command procx.Command) procx.Result {
		last := command.Argv[len(command.Argv)-1]
		switch {
		case last == "version":
			return procx.Result{Stdout: stewardTestReport}
		case last == "check":
			if !host.checkOK {
				return procx.Result{Code: 1, Stderr: "steward is not answering"}
			}
			return procx.Result{Stdout: fmt.Sprintf(
				"T3 server %s serving on 127.0.0.1:7391\n"+
					"shell snapshot: 4 threads, %d running", served, running)}
		case adapterTestMatches(command.Argv, "npm", "ls"):
			if installed == "" {
				return procx.Result{Code: 1, Stderr: "not installed"}
			}
			return procx.Result{Stdout: fmt.Sprintf(
				`{"dependencies":{"t3":{"version":%q}}}`, installed)}
		case adapterTestMatches(command.Argv, "npm", "install"):
			installed = t3TestPinned
			running = host.runningAfterInstall
			return procx.Result{}
		case adapterTestMatches(command.Argv, "restart"):
			served = installed
			return procx.Result{}
		case adapterTestMatches(command.Argv, "is-active"):
			if host.active {
				return procx.Result{Stdout: "active"}
			}
			return procx.Result{Stdout: "inactive"}
		default:
			return procx.Result{}
		}
	}
	return context, runner
}

func TestT3Apply(t *testing.T) {
	cases := []struct {
		name        string
		host        t3TestHost
		dryRun      bool
		state       string
		errorText   string
		wantInstall bool
		wantRestart bool
		action      string
	}{
		{
			name: "a host already serving the pinned version is converged",
			host: t3TestHost{
				installed: t3TestPinned, served: t3TestPinned,
				checkOK: true, active: true,
			},
			state: "converged",
		},
		{
			name: "a busy host defers before anything is installed",
			host: t3TestHost{
				installed: t3TestPrevious, served: t3TestPrevious,
				running: 1, checkOK: true, active: true,
			},
			state:  "deferred",
			action: "T3 change deferred: steward cannot prove zero running threads",
		},
		{
			name: "a steward that cannot answer defers before anything is installed",
			host: t3TestHost{
				installed: t3TestPrevious, served: t3TestPrevious,
				checkOK: false, active: true,
			},
			state:  "deferred",
			action: "T3 change deferred: steward cannot prove zero running threads",
		},
		{
			name: "an idle host is installed, restarted and verified",
			host: t3TestHost{
				installed: t3TestPrevious, served: t3TestPrevious,
				checkOK: true, active: true,
			},
			state:       "updated",
			wantInstall: true,
			wantRestart: true,
			action:      "installed and serving t3@" + t3TestPinned,
		},
		{
			name: "idle evidence that changes during the install defers the restart",
			host: t3TestHost{
				installed: t3TestPrevious, served: t3TestPrevious,
				checkOK: true, active: true, runningAfterInstall: 2,
			},
			state:       "deferred",
			wantInstall: true,
			action:      "T3 installed; restart deferred because idle evidence changed",
		},
		{
			name: "a dry run never installs and never restarts",
			host: t3TestHost{
				installed: t3TestPrevious, served: t3TestPrevious,
				checkOK: true, active: true,
			},
			dryRun: true,
			state:  "updated",
			action: "would install t3@" + t3TestPinned +
				"; restart requires a fresh zero-running-thread observation",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context, runner := t3TestPrepare(t, test.host)
			context.DryRun = test.dryRun
			spec := context.Spec("t3")
			before, err := t3Observe(context, spec)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			actions := []string{}
			state, err := t3Apply(context, spec, before, &actions)
			if test.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), test.errorText) {
					t.Fatalf("want error containing %q, got %q and %v",
						test.errorText, state, err)
				}
			} else if err != nil || state != test.state {
				t.Fatalf("state = %q, error = %v, want %q (actions %v)",
					state, err, test.state, actions)
			}
			if runner.ran("npm", "install") != test.wantInstall {
				t.Fatalf("install performed = %v, want %v",
					!test.wantInstall, test.wantInstall)
			}
			if runner.ran("restart", "t3code.service") != test.wantRestart {
				t.Fatalf("restart performed = %v, want %v",
					!test.wantRestart, test.wantRestart)
			}
			if test.action != "" && !adapterTestMatches(actions, test.action) {
				t.Fatalf("actions %v do not report %q", actions, test.action)
			}
		})
	}
}

func TestT3ObserveReportsWhatTheStewardProved(t *testing.T) {
	context, _ := t3TestPrepare(t, t3TestHost{
		installed: t3TestPinned, served: t3TestPinned, running: 3,
		checkOK: true, active: true,
	})
	observed, err := t3Observe(context, context.Spec("t3"))
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if observed["installed"] != t3TestPinned || observed["served"] != t3TestPinned ||
		observed["running_threads"] != 3 || observed["check_ok"] != true ||
		observed["service_active"] != true {
		t.Fatalf("observation = %v", observed)
	}
	if t3Idle(observed) {
		t.Fatal("three running threads are not idle")
	}
}

// An observation the steward could not make is never treated as idle.
func TestT3IdleNeedsAProvenCount(t *testing.T) {
	if t3Idle(map[string]any{"check_ok": true, "running_threads": nil}) {
		t.Fatal("an unknown thread count is not idle")
	}
	if t3Idle(map[string]any{"check_ok": false, "running_threads": 0}) {
		t.Fatal("a failed check is not idle")
	}
	if !t3Idle(map[string]any{"check_ok": true, "running_threads": 0}) {
		t.Fatal("a proven zero is idle")
	}
}

// t3TestUnit answers systemd's questions about the T3 unit for one test: the
// ExecStart program, the main process, and what mise says t3 is.
type t3TestUnit struct {
	execStart string
	mainPID   string
	miseWhich procx.Result
}

func (u t3TestUnit) handle(inner func(procx.Command) procx.Result) func(procx.Command) procx.Result {
	return func(command procx.Command) procx.Result {
		switch {
		case adapterTestMatches(command.Argv, "show", "t3code.service", "ExecStart"):
			if u.execStart == "" {
				return procx.Result{}
			}
			return procx.Result{Stdout: "{ path=" + u.execStart + " ; argv[]=" + u.execStart +
				" serve --host 127.0.0.1 --port 7391 ; ignore_errors=no ; pid=0 }\n"}
		case adapterTestMatches(command.Argv, "show", "t3code.service", "MainPID"):
			return procx.Result{Stdout: u.mainPID + "\n"}
		case adapterTestMatches(command.Argv, "which", "t3"):
			return u.miseWhich
		default:
			return inner(command)
		}
	}
}

// t3TestProc builds a /proc lookalike with one process whose exe link points
// at target, which may carry the kernel's " (deleted)" suffix.
func t3TestProc(t *testing.T, pid, target string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, pid, "exe")); err != nil {
		t.Fatal(err)
	}
	saved := t3ProcRoot
	t3ProcRoot = root
	t.Cleanup(func() { t3ProcRoot = saved })
}

// The program that runs T3 is not the unit's ExecStart on this fleet: the
// unit starts the mise shim, a symlink to /usr/bin/mise, and both survive the
// Node upgrade that deletes the Node prefix T3 lives under. The observation
// has to look past the launcher, and must never vouch for mise as T3.
func TestT3ObserveLooksPastTheMiseShimToTheRealProgram(t *testing.T) {
	context, runner := t3TestPrepare(t, t3TestHost{
		installed: t3TestPinned, served: t3TestPinned, checkOK: false, active: true,
	})
	inner := runner.handle
	shim := context.Path(".local/share/mise/shims/t3")
	mise := context.Path("usr/bin/mise")
	adapterTestWrite(t, mise, "#!/bin/sh\n")
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mise, shim); err != nil {
		t.Fatal(err)
	}
	node := context.Path(".local/share/mise/installs/node/26.7.0/bin/node")
	t3 := context.Path(".local/share/mise/installs/node/26.7.0/bin/t3")

	// The reviewer's case: the server is still running from a Node install
	// that a mise upgrade removed. The kernel marks its program deleted.
	t3TestProc(t, "4242", node+" (deleted)")
	runner.handle = t3TestUnit{execStart: shim, mainPID: "4242"}.handle(inner)
	observed, err := t3Observe(context, context.Spec("t3"))
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if observed["installed_path"] != node || observed["installed_path_exists"] != false {
		t.Fatalf("a deleted running program was not reported: %v", observed)
	}
	if warning, _ := observed["warning"].(string); !strings.Contains(warning, "missing on disk") {
		t.Fatalf("the observation carries no warning: %v", observed)
	}
	refusal := stewardRunningT3InRange(observed, t3TestPinned, t3TestPinned, t3TestPinned)
	if refusal == nil || !strings.HasPrefix(refusal.Error(),
		"running T3 binary missing on disk: "+node+" (installed "+t3TestPinned+" from the manifest)") {
		t.Fatalf("the refusal does not name the deleted program: %v", refusal)
	}

	// A running program that is still on disk is reported as present.
	adapterTestWrite(t, node, "#!/bin/sh\n")
	t3TestProc(t, "4243", node)
	runner.handle = t3TestUnit{execStart: shim, mainPID: "4243"}.handle(inner)
	observed, _ = t3Observe(context, context.Spec("t3"))
	if observed["installed_path"] != node || observed["installed_path_exists"] != true {
		t.Fatalf("a present running program was not reported: %v", observed)
	}

	// No process (the unit is down): mise is asked which t3 it would run, and
	// its answer is checked on disk.
	runner.handle = t3TestUnit{execStart: shim, mainPID: "0",
		miseWhich: procx.Result{Stdout: t3 + "\n"}}.handle(inner)
	observed, _ = t3Observe(context, context.Spec("t3"))
	if observed["installed_path"] != t3 || observed["installed_path_exists"] != false {
		t.Fatalf("a missing mise-managed t3 was not reported: %v", observed)
	}
	adapterTestWrite(t, t3, "#!/bin/sh\n")
	observed, _ = t3Observe(context, context.Spec("t3"))
	if observed["installed_path_exists"] != true {
		t.Fatalf("a present mise-managed t3 was not reported: %v", observed)
	}

	// A mise that cannot answer leaves the question open: the shim resolving
	// to an existing mise binary is never evidence that T3 exists.
	runner.handle = t3TestUnit{execStart: shim, mainPID: "0",
		miseWhich: procx.Result{Code: 1, Stderr: "mise ERROR t3 is not a tool"}}.handle(inner)
	observed, _ = t3Observe(context, context.Spec("t3"))
	if observed["installed_path"] != shim || observed["installed_path_exists"] != nil {
		t.Fatalf("mise itself was taken for T3: %v", observed)
	}
}

// The service unit names the T3 program it starts. Whether that program still
// exists on disk is the one fact that separates "T3 is not answering" from "a
// Node upgrade deleted T3", so the observation carries it.
func TestT3ObserveReportsWhetherTheServiceBinaryExists(t *testing.T) {
	context, runner := t3TestPrepare(t, t3TestHost{
		installed: t3TestPinned, served: t3TestPinned, checkOK: true, active: true,
	})
	binary := context.Path(".local/share/mise/shims/t3")
	inner := runner.handle
	runner.handle = t3TestUnit{execStart: binary, mainPID: "0"}.handle(inner)
	observed, err := t3Observe(context, context.Spec("t3"))
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if observed["installed_path"] != binary || observed["installed_path_exists"] != false {
		t.Fatalf("a missing service binary was not reported: %v", observed)
	}
	adapterTestWrite(t, binary, "#!/bin/sh\n")
	observed, err = t3Observe(context, context.Spec("t3"))
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if observed["installed_path_exists"] != true {
		t.Fatalf("a present service binary was not reported: %v", observed)
	}

	// A unit systemd cannot describe leaves the question open rather than
	// answering it either way.
	runner.handle = inner
	observed, err = t3Observe(context, context.Spec("t3"))
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if observed["installed_path"] != nil || observed["installed_path_exists"] != nil {
		t.Fatalf("an unknown service binary was guessed at: %v", observed)
	}
}
