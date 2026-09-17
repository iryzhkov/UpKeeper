package agent

import (
	"fmt"
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

// The service unit names the T3 program it starts. Whether that program still
// exists on disk is the one fact that separates "T3 is not answering" from "a
// Node upgrade deleted T3", so the observation carries it.
func TestT3ObserveReportsWhetherTheServiceBinaryExists(t *testing.T) {
	context, runner := t3TestPrepare(t, t3TestHost{
		installed: t3TestPinned, served: t3TestPinned, checkOK: true, active: true,
	})
	binary := context.Path(".local/share/mise/shims/t3")
	inner := runner.handle
	runner.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "show", "t3code.service", "ExecStart") {
			return procx.Result{Stdout: "{ path=" + binary + " ; argv[]=" + binary +
				" serve --host 127.0.0.1 --port 7391 ; ignore_errors=no ; pid=0 }\n"}
		}
		return inner(command)
	}
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
