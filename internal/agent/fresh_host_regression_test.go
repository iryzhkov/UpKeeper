package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// These tests replay the first converge of agent-a, a fresh Debian 13 VM, on
// 2026-10-06. Each one stopped that converge and needed a manual step.

// freshTestTools puts an executable stand-in for every named program in one
// directory and returns it. The programs are never run by these tests: the
// injected runner answers for them. They exist so that a lookup on the
// agent's PATH finds them, as it would on a provisioned host.
func freshTestTools(t *testing.T, home string, names ...string) string {
	t.Helper()
	bin := filepath.Join(home, "tools")
	for _, name := range names {
		path := filepath.Join(bin, name)
		adapterTestWrite(t, path, "#!/bin/sh\nexit 0\n")
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// freshTestCommands is every program a fresh host is provisioned with before
// its first converge.
var freshTestCommands = []string{
	"bash", "curl", "jq", "python3", "ssh", "readlink", "secret-tool", "mise",
	"git", "go", "make",
}

// freshTestContext is an Execute-ready context for a host named agent-a. Its
// Go reports a version new enough to build Huyang.
func freshTestContext(
	t *testing.T, home, bin string, inner procx.Runner, selected []string,
	components map[string]any,
) *Context {
	t.Helper()
	runner := func(command procx.Command) procx.Result {
		if strings.Join(command.Argv, " ") == "go env GOVERSION" {
			return procx.Result{Stdout: "go1.24.4\n"}
		}
		return inner(command)
	}
	return &Context{
		Payload: Payload{
			RunID: "20261006T000000Z-fresh",
			Host:  HostRef{Name: "agent-a", OSID: "debian"},
			Lock:  map[string]any{"run_id": "20261006T000000Z-fresh"},
		},
		Home:          home,
		Host:          HostRef{Name: "agent-a", OSID: "debian"},
		RunID:         "20261006T000000Z-fresh",
		Components:    components,
		Selected:      selected,
		Stored:        map[string]string{},
		Env:           []string{"PATH=" + bin, "HOME=" + home},
		Runtime:       filepath.Join(home, "run"),
		Runner:        runner,
		lockDirectory: filepath.Join(home, ".local/state/dev-fleet/locks/agent-a.lock"),
	}
}

// replaceHandler swaps one registered adapter for the duration of a test.
func replaceHandler(t *testing.T, name string, handler Handler) {
	t.Helper()
	prior, had := handlers[name]
	handlers[name] = handler
	t.Cleanup(func() {
		if had {
			handlers[name] = prior
		} else {
			delete(handlers, name)
		}
	})
}

func resultEntry(t *testing.T, result map[string]any, name string) map[string]any {
	t.Helper()
	entries, _ := result["components"].([]any)
	for _, item := range entries {
		entry, _ := item.(map[string]any)
		if entry["name"] == name {
			return entry
		}
	}
	t.Fatalf("no result entry for %s in %v", name, result)
	return nil
}

// Evidence 1: the OV memory virtualenv failed on a host without Debian's
// python3-venv package, and nothing before the failure named the package. A
// dry run names it, with the command that installs it.
func TestDryRunNamesAMissingOSPrerequisiteWithItsInstallCommand(t *testing.T) {
	home := t.TempDir()
	bin := freshTestTools(t, home, freshTestCommands...)
	adapterTestWrite(t, filepath.Join(home, ".config/ov-mcp/key"), "redacted\n")
	runner := func(command procx.Command) procx.Result {
		if len(command.Argv) > 1 && command.Argv[0] == "python3" && command.Argv[1] == "-c" {
			return procx.Result{Code: 1, Stderr: "ModuleNotFoundError: No module named 'ensurepip'"}
		}
		return procx.Result{Code: 1, Stderr: "not available in this test"}
	}
	c := freshTestContext(t, home, bin, runner, []string{release.ToolingComponent},
		map[string]any{release.ToolingComponent: validToolingSpec(t)})
	c.DryRun = true
	c.Payload.DryRun = true
	result := Execute(c)
	entry := resultEntry(t, result, release.ToolingComponent)
	message, _ := entry["error"].(string)
	if entry["state"] != "blocked" || !strings.Contains(message, "python3-venv") ||
		!strings.Contains(message, "sudo apt-get install -y python3-venv") {
		t.Fatalf("entry = %v", entry)
	}
}

// Evidence 2: npm was not on PATH, because a non-interactive SSH session on
// Debian never reads the part of ~/.bashrc that adds the mise shims. The agent
// finds tools through its own explicit PATH, whatever the session had.
func TestAgentCommandsFindMiseShimsWithoutShellStartupFiles(t *testing.T) {
	home := t.TempDir()
	shim := filepath.Join(home, ".local/share/mise/shims/npm")
	adapterTestWrite(t, shim, "#!/bin/sh\necho npm from mise\n")
	if err := os.Chmod(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	c := &Context{Home: home, Env: agentEnvironment(home), Runner: procx.Run}
	result := c.Run([]string{"npm", "--version"}, 10*time.Second, home)
	if !result.OK || result.Output != "npm from mise" {
		t.Fatalf("npm through the agent PATH = %+v", result)
	}
}

// toolingStageRunner answers the staging commands, failing the virtualenv
// while failVenv says so.
func toolingStageRunner(failVenv *bool) procx.Runner {
	return func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "-m", "venv") && *failVenv {
			return procx.Result{Code: 1, Stderr: "ensurepip is not available"}
		}
		return procx.Result{}
	}
}

// Evidence 3: after each failure the next run refused with "incomplete tooling
// stage preserved", although the stage was this release's own failed attempt.
// UpKeeper's own failed stage is set aside for diagnosis and retried.
func TestToolingRetriesItsOwnFailedStage(t *testing.T) {
	home := t.TempDir()
	spec := validToolingSpec(t)
	tool, err := release.DecodeTooling(spec)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".local/share/upkeeper-tools")
	dir := filepath.Join(base, "releases", toolingIdentity(spec))
	failVenv := true
	c := &Context{Home: home, RunID: "20261006T000000Z-first", Runner: toolingStageRunner(&failVenv)}
	if err := stageTooling(c, dir, tool); err == nil ||
		!strings.Contains(err.Error(), "OV memory virtualenv failed") {
		t.Fatalf("first attempt = %v", err)
	}
	failVenv = false
	c.RunID = "20261006T000100Z-second"
	if err := stageTooling(c, dir, tool); err != nil {
		t.Fatalf("the retry of UpKeeper's own failed stage was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err != nil {
		t.Fatalf("the retried stage is not complete: %v", err)
	}
	kept, err := os.ReadDir(filepath.Join(base, "failed-stages"))
	if err != nil || len(kept) != 1 {
		t.Fatalf("failed stages kept for diagnosis = %v (%v)", kept, err)
	}
	lock := filepath.Join(base, "failed-stages", kept[0].Name(), "ov-memory/requirements.lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the failed attempt was not kept intact: %v", err)
	}
}

// A stage UpKeeper cannot prove it made is still somebody else's to inspect.
func TestToolingStillRefusesAStageOfUnknownOrigin(t *testing.T) {
	home := t.TempDir()
	spec := validToolingSpec(t)
	tool, err := release.DecodeTooling(spec)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".local/share/upkeeper-tools/releases", toolingIdentity(spec))
	adapterTestWrite(t, filepath.Join(dir, "operator-notes"), "looking at this\n")
	failVenv := false
	c := &Context{Home: home, RunID: "20261006T000000Z-unknown", Runner: toolingStageRunner(&failVenv)}
	err = stageTooling(c, dir, tool)
	if err == nil || !strings.Contains(err.Error(), "incomplete tooling stage") ||
		!strings.Contains(err.Error(), dir) {
		t.Fatalf("stage of unknown origin = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "operator-notes")); err != nil ||
		string(data) != "looking at this\n" {
		t.Fatal("the stage of unknown origin was touched")
	}
}

// Evidence 4: the ov-memory health check failed on a host without an OV
// credential, which UpKeeper must never copy, and the failure skipped every
// component after it. The credential blocks its own component only.
func TestAMissingCredentialBlocksOnlyItsComponent(t *testing.T) {
	home := t.TempDir()
	bin := freshTestTools(t, home, freshTestCommands...)
	replaceHandler(t, release.ToolingComponent, Handler{
		Observe: func(*Context, map[string]any) (map[string]any, error) {
			return map[string]any{"drift": []string{"tooling bundle not installed"}}, nil
		},
		Apply: func(*Context, map[string]any, map[string]any, *[]string) (string, error) {
			return "", Failf("ov-memory health check: no OpenViking API key")
		},
	})
	huyangRan := false
	replaceHandler(t, "huyang", Handler{
		Observe: func(*Context, map[string]any) (map[string]any, error) { return map[string]any{}, nil },
		Apply: func(*Context, map[string]any, map[string]any, *[]string) (string, error) {
			huyangRan = true
			return "converged", nil
		},
	})
	runner := func(command procx.Command) procx.Result {
		if len(command.Argv) > 1 && command.Argv[1] == "lookup" {
			return procx.Result{Code: 1, Stderr: "Cannot autolaunch D-Bus without X11 $DISPLAY"}
		}
		return procx.Result{}
	}
	c := freshTestContext(t, home, bin, runner,
		[]string{release.ToolingComponent, "huyang"},
		map[string]any{release.ToolingComponent: validToolingSpec(t), "huyang": huyangTestSpec()})
	result := Execute(c)
	tooling := resultEntry(t, result, release.ToolingComponent)
	message, _ := tooling["error"].(string)
	if tooling["state"] != "blocked" || !strings.Contains(message, "credential missing") ||
		!strings.Contains(message, ".config/ov-mcp/key") {
		t.Fatalf("tooling entry = %v", tooling)
	}
	if !huyangRan || resultEntry(t, result, "huyang")["state"] != "converged" {
		t.Fatalf("a missing credential stopped an unrelated component: %v", result)
	}
	if result["result"] != "blocked" {
		t.Fatalf("host result = %v, want blocked", result["result"])
	}
}

// Evidence 5: Huyang bootstrap claimed the checkout with os.Mkdir and then
// renamed the built candidate onto it, which Go's os.Rename refuses for an
// existing directory, so a host without Huyang could never bootstrap. This
// runs the bootstrap on a real filesystem.
func TestHuyangBootstrapsAFreshHostOnARealFilesystem(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	context.Components["huyang"] = huyangTestSpec()
	context.Env = []string{"PATH=" + freshTestTools(t, context.Home, "git", "go", "make")}
	huyangTestPatch(t, huyangTestProbe(true, 19))
	runner.handle = func(command procx.Command) procx.Result {
		switch {
		case adapterTestMatches(command.Argv, "git", "clone"):
			candidate := command.Argv[len(command.Argv)-1]
			adapterTestWrite(t, filepath.Join(candidate, ".git/HEAD"), "ref: refs/heads/main\n")
			adapterTestWrite(t, filepath.Join(candidate, "go.mod"), "module huyang\n")
		case adapterTestMatches(command.Argv, "make", "build"):
			adapterTestWrite(t, filepath.Join(command.Dir, "bin/huyang"), "#!/bin/sh\n")
		case adapterTestMatches(command.Argv, "enable", "--now"):
			adapterTestWrite(t, huyangSocket(context), "")
		}
		return procx.Result{}
	}
	actions := []string{}
	state, err := huyangBootstrap(context, context.Spec("huyang"), &actions)
	if err != nil || state != "updated" {
		t.Fatalf("bootstrap = %q, %v (actions %v)", state, err, actions)
	}
	checkout := context.Path(".local/share/huyang")
	for _, path := range []string{".git/HEAD", "go.mod", "bin/huyang"} {
		if _, err := os.Stat(filepath.Join(checkout, path)); err != nil {
			t.Fatalf("the built checkout is missing %s: %v", path, err)
		}
	}
	unit, err := os.ReadFile(context.Path(".config/systemd/user/huyang.service"))
	if err != nil || !strings.Contains(string(unit), "ExecStart=%h/.local/share/huyang/bin/huyang serve") {
		t.Fatalf("seeded unit = %q (%v)", unit, err)
	}
	left, err := filepath.Glob(filepath.Join(filepath.Dir(checkout), ".huyang-bootstrap-*"))
	if err != nil || len(left) != 0 {
		t.Fatalf("bootstrap staging left behind: %v (%v)", left, err)
	}
}

// stewardPairHost is one of the four T3/steward installations a pair
// transition can find.
type stewardPairHost struct {
	t3Installed      bool
	t3Serving        string
	stewardInstalled bool
}

const stewardPairOldBinary = "#!/bin/sh\necho old steward\n"

// stewardPairPrepare builds a host whose programs answer the way the real ones
// do: a binary that is not on disk cannot be run, and a steward can only report
// a T3 that is serving.
func stewardPairPrepare(t *testing.T, host stewardPairHost) (*Context, *adapterTestRunner) {
	t.Helper()
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	context.Components["t3-steward"] = stewardTestSpec(false)
	context.Components["t3"] = map[string]any{
		"kind": "npm-package", "package": "t3", "service": "t3code.service", "version": "0.0.38",
		"compatibility": map[string]any{"min": "0.0.38", "max": "0.0.38"},
	}
	installed := context.Path(".local/bin/t3-steward")
	if host.stewardInstalled {
		adapterTestWrite(t, installed, stewardPairOldBinary)
	}
	loaded := func(unit string) bool {
		switch unit {
		case "t3code.service":
			return host.t3Installed
		case "t3-steward.service":
			return host.stewardInstalled
		}
		return false
	}
	runner.handle = func(command procx.Command) procx.Result {
		argv := command.Argv
		last := argv[len(argv)-1]
		if filepath.IsAbs(argv[0]) {
			body, err := os.ReadFile(argv[0])
			if err != nil {
				return procx.Result{Code: -1, Err: errors.New("fork/exec " + argv[0] + ": no such file or directory")}
			}
			switch last {
			case "version":
				if string(body) == stewardPairOldBinary {
					return procx.Result{Stdout: "t3-steward 0.10.0 (tested with T3 0.0.38..0.0.38)"}
				}
				return procx.Result{Stdout: stewardTestReport}
			case "check":
				if host.t3Serving == "" {
					return procx.Result{Code: 1, Stderr: "dial tcp 127.0.0.1:7391: connection refused"}
				}
				return procx.Result{Stdout: "T3 server " + host.t3Serving + " serving on 127.0.0.1:7391\n" +
					"shell snapshot: 4 threads, 0 running"}
			case "inspect-journal":
				return procx.Result{Code: 1, Stderr: "no journal"}
			}
			return procx.Result{}
		}
		switch {
		case adapterTestMatches(argv, "npm", "ls"):
			if host.t3Installed {
				return procx.Result{Stdout: `{"dependencies": {"t3": {"version": "0.0.38"}}}`}
			}
			return procx.Result{Code: 1, Stdout: `{}`}
		case adapterTestMatches(argv, "systemctl", "is-active"):
			if loaded(last) && (last != "t3code.service" || host.t3Serving != "") {
				return procx.Result{Stdout: "active"}
			}
			return procx.Result{Code: 3, Stdout: "inactive"}
		case adapterTestMatches(argv, "systemctl", "show") && adapterTestMatches(argv, "LoadState"):
			unit := argv[3]
			state := "not-found"
			if loaded(unit) {
				state = "loaded"
			}
			if adapterTestMatches(argv, "--value") {
				return procx.Result{Stdout: state}
			}
			return procx.Result{Stdout: "LoadState=" + state + "\nActiveState=inactive\nUnitFileState=\nFragmentPath="}
		case adapterTestMatches(argv, "systemctl", "cat"):
			if !loaded(last) {
				return procx.Result{Code: 1, Stderr: "No files found for " + last + "."}
			}
			return procx.Result{Stdout: "[Service]\n"}
		case adapterTestMatches(argv, "systemctl", "restart"):
			if !loaded(last) {
				return procx.Result{Code: 5, Stderr: "Failed to restart " + last + ": Unit " + last + " not found."}
			}
			return procx.Result{}
		}
		return procx.Result{}
	}
	return context, runner
}

func stewardPairApply(t *testing.T, context *Context) (string, []string, error) {
	t.Helper()
	archive := stewardTestArchive(t, []byte(stewardTestBinary), nil, false)
	stewardTestServe(t, archive, stewardTestDigest(archive))
	spec := context.Spec("t3-steward")
	before, err := stewardObserve(context, spec)
	if err != nil {
		t.Fatal(err)
	}
	actions := []string{}
	state, err := stewardApply(context, spec, before, &actions)
	return state, actions, err
}

// Evidence 6: the steward refused a pair transition without an observed
// running T3, and T3 is converged after the steward, so a host with neither
// could never converge. Nothing installed is a safe transition; every other
// combination keeps its proof.
func TestStewardPairTransitionOnEveryInstallation(t *testing.T) {
	cases := []struct {
		name      string
		host      stewardPairHost
		state     string
		errorText string
		restarted bool
	}{
		{name: "fresh host with neither installed", host: stewardPairHost{}, state: "updated"},
		{
			name:  "T3 only, serving inside the range",
			host:  stewardPairHost{t3Installed: true, t3Serving: "0.0.38"},
			state: "updated",
		},
		{
			name:  "steward only, no T3 at all",
			host:  stewardPairHost{stewardInstalled: true},
			state: "updated", restarted: true,
		},
		{
			name:  "both installed, T3 inside the range",
			host:  stewardPairHost{t3Installed: true, t3Serving: "0.0.38", stewardInstalled: true},
			state: "updated", restarted: true,
		},
		{
			name:      "both installed, T3 outside the range",
			host:      stewardPairHost{t3Installed: true, t3Serving: "0.0.40", stewardInstalled: true},
			errorText: "running T3 0.0.40 is outside the candidate steward's T3 range",
		},
		{
			name:      "T3 installed but not answering",
			host:      stewardPairHost{t3Installed: true, stewardInstalled: true},
			errorText: "running T3 version could not be observed",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context, runner := stewardPairPrepare(t, test.host)
			state, actions, err := stewardPairApply(t, context)
			if test.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), test.errorText) {
					t.Fatalf("want a refusal containing %q, got %q, %v", test.errorText, state, err)
				}
				body, _ := os.ReadFile(context.Path(".local/bin/t3-steward"))
				if string(body) == stewardTestBinary {
					t.Fatal("a refused transition installed the candidate")
				}
				return
			}
			if err != nil || state != test.state {
				t.Fatalf("state = %q, error = %v (actions %v)", state, err, actions)
			}
			body, err := os.ReadFile(context.Path(".local/bin/t3-steward"))
			if err != nil || string(body) != stewardTestBinary {
				t.Fatalf("installed steward = %q (%v)", body, err)
			}
			if runner.ran("restart", "t3-steward.service") != test.restarted {
				t.Fatalf("steward restart = %v, want %v (calls %v)",
					!test.restarted, test.restarted, runner.calls)
			}
		})
	}
}
