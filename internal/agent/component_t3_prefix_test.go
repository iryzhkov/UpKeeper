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
	// codePrefix is where T3's entry script and its native module live, when
	// that is not the running Node's own prefix.
	codePrefix string
	// npmPrefixVia, when set, is a symlink to npmPrefix that npm reports.
	npmPrefixVia string
}

func t3PrefixPrepare(t *testing.T, host t3PrefixHost) (*Context, *adapterTestRunner) {
	t.Helper()
	context, runner := t3TestPrepare(t, host.t3TestHost)
	running := context.Path(host.runningPrefix + "/bin/node")
	adapterTestWrite(t, running, "#!/bin/sh\n")
	t3TestProc(t, "4242", running)
	code := host.codePrefix
	if code == "" {
		code = host.runningPrefix
	}
	script := context.Path(code + "/lib/node_modules/t3/dist/bin.mjs")
	native := context.Path(code + "/lib/node_modules/t3/node_modules/node-pty/build/Release/pty.node")
	adapterTestWrite(t, script, "\n")
	adapterTestWrite(t, native, "\n")
	process := filepath.Join(t3ProcRoot, "4242")
	adapterTestWrite(t, filepath.Join(process, "cmdline"),
		running+"\x00"+script+"\x00serve\x00--port\x007391\x00")
	adapterTestWrite(t, filepath.Join(process, "maps"),
		"55d0-55d1 r--p 00000000 103:02 1 "+running+"\n"+
			"7f00-7f01 r-xp 00000000 103:02 2 "+native+"\n"+
			"7f02-7f03 rw-p 00000000 00:00 0 [heap]\n")
	if err := os.MkdirAll(context.Path(host.npmPrefix), 0o755); err != nil {
		t.Fatal(err)
	}
	reported := host.npmPrefix
	if host.npmPrefixVia != "" {
		if err := os.Symlink(context.Path(host.npmPrefix), context.Path(host.npmPrefixVia)); err != nil {
			t.Fatal(err)
		}
		reported = host.npmPrefixVia
	}
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
			return procx.Result{Stdout: context.Path(reported) + "\n"}
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

// A Node outside npm's prefix does not make the install safe when T3's own
// code lives inside it: npm's global prefix can be configured away from Node
// (an ~/.npmrc prefix, say), and then the entry script and node-pty are the
// files npm would replace.
func TestT3BusyServerWithCodeInNpmPrefixIsNotTouched(t *testing.T) {
	context, runner := t3PrefixPrepare(t, t3PrefixHost{
		t3TestHost: t3TestHost{
			served: t3TestPinned, running: 1, checkOK: true, active: true,
		},
		runningPrefix: "usr", codePrefix: ".npm-global", npmPrefix: ".npm-global",
	})
	spec := context.Spec("t3")
	before, _ := t3Observe(context, spec)
	actions := []string{}
	state, err := t3Apply(context, spec, before, &actions)
	if err != nil || state != "deferred" || runner.ran("npm", "install") {
		t.Fatalf("state = %q, error = %v, calls %v", state, err, runner.calls)
	}
}

// A prefix npm reports through a symlink is the directory it points at: here
// the running server's own, so nothing is installed.
func TestT3SymlinkedNpmPrefixIsResolved(t *testing.T) {
	context, runner := t3PrefixPrepare(t, t3PrefixHost{
		t3TestHost: t3TestHost{
			served: t3TestPinned, running: 1, checkOK: true, active: true,
		},
		runningPrefix: "node/26.7.0", npmPrefix: "node/26.7.0", npmPrefixVia: "node/26",
	})
	spec := context.Spec("t3")
	before, _ := t3Observe(context, spec)
	actions := []string{}
	state, err := t3Apply(context, spec, before, &actions)
	if err != nil || state != "deferred" || runner.ran("npm", "install") {
		t.Fatalf("state = %q, error = %v, calls %v", state, err, runner.calls)
	}
}

// A child of the server loads files of its own, and a relative entry script is
// resolved against the working directory; both count, and a path with spaces
// in maps is read whole.
func TestT3RunningFilesReadsTheProcessTree(t *testing.T) {
	context, _ := t3PrefixPrepare(t, t3PrefixHost{
		t3TestHost:    t3TestHost{served: t3TestPinned, running: 1, checkOK: true, active: true},
		runningPrefix: "node/26.7.0", npmPrefix: "node/26.10.0",
	})
	main := filepath.Join(t3ProcRoot, "4242")
	child := filepath.Join(t3ProcRoot, "4300")
	workdir := context.Path("work dir")
	script := filepath.Join(workdir, "bin/t3")
	spaced := context.Path("node/26.10.0/lib/node_modules/t3/a b/helper.node")
	adapterTestWrite(t, script, "\n")
	adapterTestWrite(t, spaced, "\n")
	adapterTestWrite(t, filepath.Join(main, "task/4242/children"), "4300 ")
	adapterTestWrite(t, filepath.Join(child, "cmdline"), "node\x00bin/t3\x00worker\x00")
	if err := os.Symlink(context.Path("node/26.7.0/bin/node"), filepath.Join(child, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workdir, filepath.Join(child, "cwd")); err != nil {
		t.Fatal(err)
	}
	adapterTestWrite(t, filepath.Join(child, "maps"), "7f00-7f01 r-xp 00000000 103:02 9                          "+spaced+"\n")
	files := t3RunningFiles(context, "t3code.service")
	want := map[string]bool{script: false, spaced: false}
	for _, file := range files {
		if _, ok := want[file.(string)]; ok {
			want[file.(string)] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Fatalf("%s missing from %v", path, files)
		}
	}
	if t3InstallSparesRunning(map[string]any{"npm_prefix": context.Path("node/26.10.0"), "running_files": files}) {
		t.Fatal("a child's file inside npm's prefix was spared")
	}
}

func TestT3InstallSparesRunningNeedsEveryFileOutside(t *testing.T) {
	for _, observed := range []map[string]any{
		{"npm_prefix": "/n/26.10.0"},
		{"running_files": []any{"/n/26.7.0/bin/node"}},
		{"npm_prefix": "/n/26.10.0", "running_files": []any{}},
		{"npm_prefix": "/n/26.10.0", "running_files": []any{"/n/26.10.0/bin/node"}},
		{"npm_prefix": "/n/26", "running_files": []any{"/n/26/bin/node"}},
		{"npm_prefix": "/g", "running_files": []any{"/usr/bin/node", "/g/lib/node_modules/t3/pty.node"}},
	} {
		if t3InstallSparesRunning(observed) {
			t.Fatalf("spared on %v", observed)
		}
	}
	if !t3InstallSparesRunning(map[string]any{"npm_prefix": "/n/26.10.0",
		"running_files": []any{"/n/26.7.0/bin/node", "/n/26.7.0/lib/node_modules/t3/dist/bin.mjs", "/usr/lib/libc.so.6"}}) {
		t.Fatal("a different prefix was not spared")
	}
}
