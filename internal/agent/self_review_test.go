package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/prereq"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// Findings of the self-review of the fresh-host change, each reproduced by
// the test below before it was fixed.

// failStages runs attempts failing tooling stages, one minute apart.
func failStages(t *testing.T, home string, attempts int) {
	t.Helper()
	spec := validToolingSpec(t)
	tool, err := release.DecodeTooling(spec)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".local/share/upkeeper-tools/releases", toolingIdentity(spec))
	failVenv := true
	clock := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	c := &Context{Home: home, Runner: toolingStageRunner(&failVenv), Now: func() time.Time { return clock }}
	for i := 0; i < attempts; i++ {
		c.RunID = fmt.Sprintf("run-%d", i)
		clock = clock.Add(time.Minute)
		_ = stageTooling(c, dir, tool)
	}
}

func TestFailedStagesAreNeverMovedThroughASymlink(t *testing.T) {
	home := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	for _, name := range []string{"a", "b", "c", "d"} {
		if err := os.MkdirAll(filepath.Join(victim, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	base := filepath.Join(home, ".local/share/upkeeper-tools")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(base, "failed-stages")); err != nil {
		t.Fatal(err)
	}
	failStages(t, home, 3)
	entries, err := os.ReadDir(victim)
	if err != nil || len(entries) != 4 {
		t.Fatalf("the directory behind the symlink was changed: %v (%v)", entries, err)
	}
}

func TestPruningLeavesDirectoriesUpKeeperDidNotPutThere(t *testing.T) {
	home := t.TempDir()
	foreign := filepath.Join(home, ".local/share/upkeeper-tools/failed-stages/0-operator-copy")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	failStages(t, home, toolingFailedStageLimit+2)
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("a foreign directory was pruned: %v", err)
	}
}

// A crash inside the claim leaves at most a private directory beside the
// stage, never a stage without its marker.
func TestAToolingStageIsNeverWithoutItsMarker(t *testing.T) {
	home := t.TempDir()
	spec := validToolingSpec(t)
	tool, err := release.DecodeTooling(spec)
	if err != nil {
		t.Fatal(err)
	}
	releases := filepath.Join(home, ".local/share/upkeeper-tools/releases")
	dir := filepath.Join(releases, toolingIdentity(spec))
	// What a crash after the private directory was made leaves behind.
	if err := os.MkdirAll(filepath.Join(releases, ".stage-crashed"), 0o700); err != nil {
		t.Fatal(err)
	}
	failVenv := true
	c := &Context{Home: home, RunID: "run", Runner: toolingStageRunner(&failVenv)}
	_ = stageTooling(c, dir, tool)
	if _, err := os.Lstat(filepath.Join(dir, toolingStageMarker)); err != nil {
		t.Fatalf("the stage has no marker: %v", err)
	}
	failVenv = false
	if err := stageTooling(c, dir, tool); err != nil {
		t.Fatalf("the retry was refused: %v", err)
	}
	if err := os.Chmod(releases, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(releases, 0o700) })
	other := filepath.Join(releases, strings.Repeat("0", 64))
	if err := claimToolingStage(c, other); err == nil {
		t.Fatal("a claim in an unwritable directory succeeded")
	}
	if _, err := os.Lstat(other); err == nil {
		t.Fatal("a failed claim left a stage without a marker")
	}
}

func credentialContext(t *testing.T) *Context {
	t.Helper()
	runner := &adapterTestRunner{handle: func(procx.Command) procx.Result { return procx.Result{Code: 1} }}
	c := adapterTestContext(t, runner)
	c.Env = []string{"PATH=" + freshTestTools(t, c.Home, "secret-tool")}
	return c
}

func TestTheCredentialCheckNeverOpensAFIFO(t *testing.T) {
	for _, variable := range []string{"OV_CONFIG", "OV_KEY_FILE"} {
		t.Run(variable, func(t *testing.T) {
			c := credentialContext(t)
			fifo := filepath.Join(c.Home, "credential.fifo")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			c.Env = append(c.Env, variable+"="+fifo)
			done := make(chan bool, 1)
			go func() { done <- ovMemoryCredentialPresent(c) }()
			select {
			case present := <-done:
				if present {
					t.Fatal("a FIFO counted as a credential")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the credential check blocked on a FIFO")
			}
		})
	}
}

func TestTheCredentialCheckReadsABoundedAmount(t *testing.T) {
	for _, variable := range []string{"OV_CONFIG", "OV_KEY_FILE"} {
		t.Run(variable, func(t *testing.T) {
			c := credentialContext(t)
			path := filepath.Join(c.Home, "huge")
			handle, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := handle.Truncate(64 << 20); err != nil {
				t.Fatal(err)
			}
			handle.Close()
			c.Env = append(c.Env, variable+"="+path)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			ovMemoryCredentialPresent(c)
			runtime.ReadMemStats(&after)
			if grown := after.TotalAlloc - before.TotalAlloc; grown > 8<<20 {
				t.Fatalf("the check allocated %d MiB for a 64 MiB file", grown>>20)
			}
		})
	}
}

// The server reads its config with tomllib, which finds no key in these.
func TestAnEmptyOrCommentedAPIKeyIsNoCredential(t *testing.T) {
	for _, line := range []string{
		`api_key = "" # fill in later`,
		`api_key = ''`,
		`api_key = ""`,
	} {
		c := credentialContext(t)
		adapterTestWrite(t, c.Path(".config/ov-mcp/config.toml"), line+"\n")
		if ovMemoryCredentialPresent(c) {
			t.Errorf("%q counted as a credential", line)
		}
	}
	c := credentialContext(t)
	adapterTestWrite(t, c.Path(".config/ov-mcp/config.toml"), "\xef\xbb\xbf\"api_key\" = \"x\" # set\n")
	if !ovMemoryCredentialPresent(c) {
		t.Error("a quoted key after a byte order mark was not found")
	}
}

// systemctl failing for any other reason than a missing unit file, such as an
// unreachable user bus, is not "no steward unit": the install restarts and
// fails loudly instead of swapping the binary without a restart.
func TestAnUnreachableUserBusIsNotAMissingStewardUnit(t *testing.T) {
	context, runner := stewardPairPrepare(t, stewardPairHost{
		t3Installed: true, t3Serving: "0.0.38", stewardInstalled: true,
	})
	inner := runner.handle
	runner.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "systemctl", "cat") || adapterTestMatches(command.Argv, "systemctl", "restart") {
			return procx.Result{Code: 1, Stderr: "Failed to connect to bus: No medium found"}
		}
		return inner(command)
	}
	state, actions, err := stewardPairApply(t, context)
	if err == nil || adapterTestMatches(actions, "is not installed on this host") {
		t.Fatalf("bus failure = %q, %v, actions %v", state, err, actions)
	}
}

func requirementsContext(t *testing.T, items []prereq.Prerequisite, runner procx.Runner) *Context {
	t.Helper()
	previous := componentPrerequisites
	componentPrerequisites = func(string) []prereq.Prerequisite { return items }
	t.Cleanup(func() { componentPrerequisites = previous })
	c := adapterTestContext(t, &adapterTestRunner{})
	c.Runner = runner
	c.Host.OSID = "debian"
	c.Host.InstallOSPackages = true
	return c
}

func TestAHostWithoutSudoIsNotToldItNeedsAPassword(t *testing.T) {
	c := requirementsContext(t,
		[]prereq.Prerequisite{{Name: "jq", Command: "jq", Packages: map[string]string{prereq.Debian: "jq"}}},
		func(command procx.Command) procx.Result {
			if command.Argv[0] == "sudo" {
				return procx.Result{Code: -1, Err: &exec.Error{Name: "sudo", Err: exec.ErrNotFound}}
			}
			return procx.Result{}
		})
	blocker := requirementsBlock(c, "review", nil, &[]string{})
	if strings.Contains(blocker, "needs a password") || !strings.Contains(blocker, "sudo is not installed") {
		t.Fatalf("blocker = %s", blocker)
	}
}

func TestAnUnpackagedPrerequisiteIsNotBlamedOnThePackageManager(t *testing.T) {
	c := requirementsContext(t,
		[]prereq.Prerequisite{{Name: "mise", Command: "mise", Packages: map[string]string{prereq.Arch: "mise"},
			Manual: "install mise by hand"}},
		func(procx.Command) procx.Result { return procx.Result{} })
	blocker := requirementsBlock(c, "review", nil, &[]string{})
	if strings.Contains(blocker, "only with apt-get or pacman") || !strings.Contains(blocker, "install mise by hand") {
		t.Fatalf("blocker = %s", blocker)
	}
}

// huyangCrashWorld is the host a bootstrap killed after its checkout rename
// leaves: the checkout on the pin and the pending marker, with or without the
// seeded unit. systemd refuses to start a unit that has no file.
func huyangCrashWorld(t *testing.T, unit string) (*Context, *adapterTestRunner) {
	t.Helper()
	runner := &adapterTestRunner{}
	c := adapterTestContext(t, runner)
	c.Components["huyang"] = huyangTestSpec()
	adapterTestWrite(t, c.Path(".local/share/huyang/.git/HEAD"), "x\n")
	adapterTestWrite(t, c.Path(huyangBootstrapPending), ".local/share/huyang\n")
	path := c.Path(".config/systemd/user/huyang.service")
	if unit != "" {
		adapterTestWrite(t, path, unit)
	}
	huyangTestPatch(t, huyangTestProbe(true, 19))
	started := false
	runner.handle = func(command procx.Command) procx.Result {
		argv := command.Argv
		switch {
		case adapterTestMatches(argv, "rev-parse", "HEAD"):
			return procx.Result{Stdout: huyangPinned}
		case adapterTestMatches(argv, "is-active"):
			if started {
				return procx.Result{Stdout: "active"}
			}
			return procx.Result{Code: 3, Stdout: "inactive"}
		case adapterTestMatches(argv, "systemctl", "restart"), adapterTestMatches(argv, "enable", "--now"):
			if !pathExists(path) {
				return procx.Result{Code: 5, Stderr: "Unit huyang.service not found."}
			}
			started = true
			adapterTestWrite(t, huyangSocket(c), "")
		}
		return procx.Result{}
	}
	return c, runner
}

func TestAnInterruptedHuyangBootstrapIsFinishedByTheNextRun(t *testing.T) {
	seeded := fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang")
	for name, unit := range map[string]string{
		"killed before the unit was written": "",
		"killed while the unit was written":  seeded[:40],
		"killed before the unit was enabled": seeded,
	} {
		t.Run(name, func(t *testing.T) {
			c, runner := huyangCrashWorld(t, unit)
			spec := c.Spec("huyang")
			states := []string{}
			for run := 0; run < 2; run++ {
				before, err := huyangObserve(c, spec)
				if err != nil {
					t.Fatal(err)
				}
				actions := []string{}
				state, err := huyangApply(c, spec, before, &actions)
				if err != nil {
					t.Fatalf("run %d = %q, %v (actions %v)", run+1, state, err, actions)
				}
				states = append(states, state)
			}
			if states[0] != "updated" || states[1] != "converged" || !runner.ran("enable", "--now", "huyang.service") {
				t.Fatalf("states = %v, calls %v", states, runner.calls)
			}
			if raw, _ := os.ReadFile(c.Path(".config/systemd/user/huyang.service")); string(raw) != seeded {
				t.Fatalf("unit = %q", raw)
			}
			if pathExists(c.Path(huyangBootstrapPending)) {
				t.Fatal("the pending marker outlived the finished bootstrap")
			}
		})
	}
}

// A unit the operator changed since is theirs; the bootstrap is not finished
// over it.
func TestAnInterruptedBootstrapPreservesAChangedUnit(t *testing.T) {
	c, runner := huyangCrashWorld(t, "[Service]\nExecStart=/opt/huyang serve\n")
	spec := c.Spec("huyang")
	before, err := huyangObserve(c, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := huyangApply(c, spec, before, &[]string{}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed unit = %v", err)
	}
	if runner.ran("enable") || runner.ran("restart") {
		t.Fatalf("a changed unit was started: %v", runner.calls)
	}
}
