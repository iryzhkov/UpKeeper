package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// t3PrefixHost is omarchy-pc after a mise Node upgrade: the server runs from
// the Node it was started under, npm installs into the Node mise selects now,
// and the unit's shim resolves t3 only when that Node has it.
type t3PrefixHost struct {
	t3TestHost
	runningPrefix string
	npmPrefix     string
}

func t3PrefixPrepare(t *testing.T, host t3PrefixHost) (*Context, *adapterTestRunner) {
	t.Helper()
	context, runner := t3TestPrepare(t, host.t3TestHost)
	running := context.Path(host.runningPrefix + "/bin/node")
	adapterTestWrite(t, running, "#!/bin/sh\n")
	t3TestProc(t, "4242", running)
	shim := context.Path(".local/share/mise/shims/t3")
	mise := context.Path("usr/bin/mise")
	adapterTestWrite(t, mise, "#!/bin/sh\n")
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mise, shim); err != nil {
		t.Fatal(err)
	}
	inner := runner.handle
	installedNow := host.installed != ""
	unit := t3TestUnit{execStart: shim, mainPID: "4242"}
	runner.handle = func(command procx.Command) procx.Result {
		switch {
		case adapterTestMatches(command.Argv, "npm", "prefix", "-g"):
			return procx.Result{Stdout: context.Path(host.npmPrefix) + "\n"}
		case adapterTestMatches(command.Argv, "npm", "install"):
			installedNow = true
		case adapterTestMatches(command.Argv, "mise", "which", "t3"):
			if !installedNow {
				return procx.Result{Code: 1, Stderr: "mise ERROR t3 is a mise bin however it is not currently active"}
			}
			return procx.Result{Stdout: context.Path(host.npmPrefix+"/bin/t3") + "\n"}
		}
		return unit.handle(inner)(command)
	}
	return context, runner
}

// A busy server whose Node is not npm's global prefix is not touched by an
// install, so the install goes ahead without idle proof. When the server
// already serves the pin, that is the whole convergence: the launcher can
// start T3 again and nothing restarts.
func TestT3InstallsBesideABusyServerRunningFromAnotherNode(t *testing.T) {
	context, runner := t3PrefixPrepare(t, t3PrefixHost{
		t3TestHost: t3TestHost{
			served: t3TestPinned, running: 1, runningAfterInstall: 1,
			checkOK: true, active: true,
		},
		runningPrefix: "node/26.7.0", npmPrefix: "node/26.10.0",
	})
	spec := context.Spec("t3")
	before, err := t3Observe(context, spec)
	if err != nil {
		t.Fatal(err)
	}
	if warning, _ := before["warning"].(string); !strings.Contains(warning, "does not resolve t3") ||
		!strings.Contains(warning, "a restart of t3code.service will fail") {
		t.Fatalf("the unstartable launcher is not reported: %v", before)
	}
	actions := []string{}
	state, err := t3Apply(context, spec, before, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v, actions %v", state, err, actions)
	}
	if !runner.ran("npm", "install") || runner.ran("restart", "t3code.service") {
		t.Fatalf("want install and no restart: %v", runner.calls)
	}
	if !adapterTestMatches(actions, "installed t3@"+t3TestPinned+" into "+context.Path("node/26.10.0")+
		" beside the running server; no restart needed") {
		t.Fatalf("actions = %v", actions)
	}
	after, _ := t3Observe(context, spec)
	if _, warned := after["launcher_problem"]; warned {
		t.Fatalf("the launcher still does not resolve t3 after the install: %v", after)
	}
}

// When the running server is older than the pin, the install still goes
// ahead beside it, and the restart waits for idle proof.
func TestT3BusyOlderServerGetsTheInstallButNotTheRestart(t *testing.T) {
	context, runner := t3PrefixPrepare(t, t3PrefixHost{
		t3TestHost: t3TestHost{
			installed: t3TestPrevious, served: t3TestPrevious, running: 1, runningAfterInstall: 1,
			checkOK: true, active: true,
		},
		runningPrefix: "node/26.7.0", npmPrefix: "node/26.10.0",
	})
	spec := context.Spec("t3")
	before, _ := t3Observe(context, spec)
	actions := []string{}
	state, err := t3Apply(context, spec, before, &actions)
	if err != nil || state != "deferred" {
		t.Fatalf("state = %q, error = %v, actions %v", state, err, actions)
	}
	if !runner.ran("npm", "install") || runner.ran("restart", "t3code.service") {
		t.Fatalf("want install and no restart: %v", runner.calls)
	}
}

// A busy server running from npm's own prefix would have its files replaced,
// so nothing is installed.
func TestT3BusyServerInNpmPrefixIsNotTouched(t *testing.T) {
	context, runner := t3PrefixPrepare(t, t3PrefixHost{
		t3TestHost: t3TestHost{
			installed: t3TestPrevious, served: t3TestPrevious, running: 1,
			checkOK: true, active: true,
		},
		runningPrefix: "node/26.10.0", npmPrefix: "node/26.10.0",
	})
	spec := context.Spec("t3")
	before, _ := t3Observe(context, spec)
	actions := []string{}
	state, err := t3Apply(context, spec, before, &actions)
	if err != nil || state != "deferred" || runner.ran("npm", "install") {
		t.Fatalf("state = %q, error = %v, calls %v", state, err, runner.calls)
	}
}

func TestT3InstallSparesRunningNeedsBothFacts(t *testing.T) {
	for _, observed := range []map[string]any{
		{"npm_prefix": "/n/26.10.0"},
		{"running_path": "/n/26.7.0/bin/node"},
		{"npm_prefix": "/n/26.10.0", "running_path": "/n/26.10.0/bin/node"},
		{"npm_prefix": "/n/26", "running_path": "/n/26/bin/node"},
	} {
		if t3InstallSparesRunning(observed) {
			t.Fatalf("spared on %v", observed)
		}
	}
	if !t3InstallSparesRunning(map[string]any{"npm_prefix": "/n/26.10.0", "running_path": "/n/26.7.0/bin/node"}) {
		t.Fatal("a different prefix was not spared")
	}
}
