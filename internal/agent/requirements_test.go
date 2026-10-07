package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// venvRunner answers the python3-venv check from *venv, sudo from sudo, and
// records whether an OS package was installed.
func venvRunner(venv *bool, sudo bool, installed *[]string) procx.Runner {
	return func(command procx.Command) procx.Result {
		argv := command.Argv
		switch {
		case len(argv) > 1 && argv[0] == "python3" && argv[1] == "-c":
			if *venv {
				return procx.Result{}
			}
			return procx.Result{Code: 1, Stderr: "No module named 'ensurepip'"}
		case adapterTestMatches(argv, "sudo", "-n", "true"):
			if sudo {
				return procx.Result{}
			}
			return procx.Result{Code: 1, Stderr: "sudo: a password is required"}
		case adapterTestMatches(argv, "sudo", "apt-get", "install"):
			*installed = append(*installed, strings.Join(argv, " "))
			*venv = true
			return procx.Result{}
		}
		return procx.Result{}
	}
}

func stubTooling(t *testing.T) *bool {
	t.Helper()
	ran := false
	replaceHandler(t, release.ToolingComponent, Handler{
		Observe: func(*Context, map[string]any) (map[string]any, error) { return map[string]any{}, nil },
		Apply: func(*Context, map[string]any, map[string]any, *[]string) (string, error) {
			ran = true
			return "updated", nil
		},
	})
	return &ran
}

func TestAnOptedInHostInstallsMissingOSPackagesWithPasswordlessSudo(t *testing.T) {
	home := t.TempDir()
	bin := freshTestTools(t, home, freshTestCommands...)
	adapterTestWrite(t, filepath.Join(home, ".config/ov-mcp/key"), "redacted\n")
	ran := stubTooling(t)
	venv, installed := false, []string{}
	c := freshTestContext(t, home, bin, venvRunner(&venv, true, &installed),
		[]string{release.ToolingComponent},
		map[string]any{release.ToolingComponent: validToolingSpec(t)})
	c.Host.InstallOSPackages = true
	result := Execute(c)
	entry := resultEntry(t, result, release.ToolingComponent)
	if !*ran || entry["state"] != "updated" {
		t.Fatalf("entry = %v", entry)
	}
	if len(installed) != 1 || !strings.Contains(installed[0], "apt-get install -y python3-venv") {
		t.Fatalf("installed = %v", installed)
	}
	if actions, _ := entry["actions"].([]string); !adapterTestMatches(actions, "installed OS packages: python3-venv") {
		t.Fatalf("actions %v do not report the installation", entry["actions"])
	}
}

func TestAnOptedInHostWithoutPasswordlessSudoIsBlockedNotPrompted(t *testing.T) {
	home := t.TempDir()
	bin := freshTestTools(t, home, freshTestCommands...)
	adapterTestWrite(t, filepath.Join(home, ".config/ov-mcp/key"), "redacted\n")
	ran := stubTooling(t)
	venv, installed := false, []string{}
	c := freshTestContext(t, home, bin, venvRunner(&venv, false, &installed),
		[]string{release.ToolingComponent},
		map[string]any{release.ToolingComponent: validToolingSpec(t)})
	c.Host.InstallOSPackages = true
	entry := resultEntry(t, Execute(c), release.ToolingComponent)
	message, _ := entry["error"].(string)
	if *ran || entry["state"] != "blocked" || len(installed) != 0 ||
		!strings.Contains(message, "passwordless sudo") ||
		!strings.Contains(message, "sudo apt-get install -y python3-venv") {
		t.Fatalf("entry = %v, installed = %v", entry, installed)
	}
}

func TestAHostThatDidNotOptInIsNeverChangedBySudo(t *testing.T) {
	home := t.TempDir()
	bin := freshTestTools(t, home, freshTestCommands...)
	adapterTestWrite(t, filepath.Join(home, ".config/ov-mcp/key"), "redacted\n")
	stubTooling(t)
	venv, installed := false, []string{}
	runner := &adapterTestRunner{handle: venvRunner(&venv, true, &installed)}
	c := freshTestContext(t, home, bin, runner.run, []string{release.ToolingComponent},
		map[string]any{release.ToolingComponent: validToolingSpec(t)})
	entry := resultEntry(t, Execute(c), release.ToolingComponent)
	if entry["state"] != "blocked" || runner.ran("sudo") {
		t.Fatalf("entry = %v, calls = %v", entry, runner.calls)
	}
}

// A converged tooling bundle no longer needs the virtualenv module, so a host
// that already has it staged is not asked for it again.
func TestAStagedToolingBundleDoesNotNeedTheVirtualenvModule(t *testing.T) {
	home := t.TempDir()
	bin := freshTestTools(t, home, freshTestCommands...)
	adapterTestWrite(t, filepath.Join(home, ".config/ov-mcp/key"), "redacted\n")
	ran := stubTooling(t)
	spec := validToolingSpec(t)
	adapterTestWrite(t, filepath.Join(home, ".local/share/upkeeper-tools/releases",
		toolingIdentity(spec), ".complete"), "staged\n")
	venv, installed := false, []string{}
	c := freshTestContext(t, home, bin, venvRunner(&venv, false, &installed),
		[]string{release.ToolingComponent}, map[string]any{release.ToolingComponent: spec})
	if entry := resultEntry(t, Execute(c), release.ToolingComponent); !*ran || entry["state"] != "updated" {
		t.Fatalf("entry = %v", entry)
	}
}

// Only the components that really depend on a blocked one are skipped.
func TestABlockedComponentSkipsOnlyItsDependents(t *testing.T) {
	home := t.TempDir()
	bin := freshTestTools(t, home, freshTestCommands...)
	prior := componentCredentials["t3-steward"]
	componentCredentials["t3-steward"] = []credential{{
		Name:    "test credential",
		Present: func(*Context) bool { return false },
		Create:  "create it by hand",
	}}
	t.Cleanup(func() {
		if prior == nil {
			delete(componentCredentials, "t3-steward")
		} else {
			componentCredentials["t3-steward"] = prior
		}
	})
	applied := map[string]bool{}
	for _, name := range []string{"huyang", "t3-steward", "t3", "worker-configuration", "agent-config"} {
		name := name
		replaceHandler(t, name, Handler{
			Observe: func(*Context, map[string]any) (map[string]any, error) { return map[string]any{}, nil },
			Apply: func(*Context, map[string]any, map[string]any, *[]string) (string, error) {
				applied[name] = true
				return "converged", nil
			},
		})
	}
	selected := []string{"agent-config", "huyang", "t3-steward", "t3", "worker-configuration"}
	components := map[string]any{}
	for _, name := range selected {
		components[name] = map[string]any{}
	}
	c := freshTestContext(t, home, bin, func(procx.Command) procx.Result { return procx.Result{} },
		selected, components)
	result := Execute(c)
	want := map[string]string{
		"agent-config": "converged", "huyang": "converged", "t3-steward": "blocked",
		"t3": "skipped", "worker-configuration": "skipped",
	}
	for name, state := range want {
		if got := resultEntry(t, result, name)["state"]; got != state {
			t.Errorf("%s = %v, want %s", name, got, state)
		}
	}
	if message, _ := resultEntry(t, result, "t3")["error"].(string); !strings.Contains(message, "depends on blocked t3-steward") {
		t.Errorf("t3 skip reason = %q", message)
	}
	if applied["t3-steward"] || applied["t3"] || applied["worker-configuration"] || !applied["huyang"] {
		t.Errorf("applied = %v", applied)
	}
}

// The ov-memory server takes its key from the environment, its config file,
// the keyring or a 0600 file, so any of them satisfies the requirement. The
// key itself is never read into a result.
func TestTheOVCredentialIsFoundWhereverTheServerLooks(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, c *Context)
		keyring bool
		present bool
	}{
		{name: "nothing anywhere", present: false},
		{
			name: "an empty key file",
			prepare: func(t *testing.T, c *Context) {
				adapterTestWrite(t, c.Path(".config/ov-mcp/key"), "")
			},
			present: false,
		},
		{
			name: "the key file",
			prepare: func(t *testing.T, c *Context) {
				adapterTestWrite(t, c.Path(".config/ov-mcp/key"), "secret\n")
			},
			present: true,
		},
		{
			name: "OV_KEY_FILE elsewhere",
			prepare: func(t *testing.T, c *Context) {
				adapterTestWrite(t, c.Path("secrets/ov"), "secret\n")
				c.Env = append(c.Env, "OV_KEY_FILE="+c.Path("secrets/ov"))
			},
			present: true,
		},
		{
			name:    "OV_API_KEY in the environment",
			prepare: func(t *testing.T, c *Context) { c.Env = append(c.Env, "OV_API_KEY=secret") },
			present: true,
		},
		{
			name: "api_key in the config file",
			prepare: func(t *testing.T, c *Context) {
				adapterTestWrite(t, c.Path(".config/ov-mcp/config.toml"),
					"base_url = \"http://ov\"\napi_key = \"secret\"\n")
			},
			present: true,
		},
		{
			name: "a config file without a key",
			prepare: func(t *testing.T, c *Context) {
				adapterTestWrite(t, c.Path(".config/ov-mcp/config.toml"),
					"# api_key = \"commented out\"\nbase_url = \"http://ov\"\n")
			},
			present: false,
		},
		{name: "the keyring", keyring: true, present: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner := &adapterTestRunner{handle: func(command procx.Command) procx.Result {
				if adapterTestMatches(command.Argv, "lookup", "openviking") && test.keyring {
					return procx.Result{Stdout: "secret\n"}
				}
				return procx.Result{Code: 1}
			}}
			c := adapterTestContext(t, runner)
			c.Env = []string{"PATH=" + freshTestTools(t, c.Home, "secret-tool")}
			if test.prepare != nil {
				test.prepare(t, c)
			}
			if got := ovMemoryCredentialPresent(c); got != test.present {
				t.Fatalf("present = %v, want %v", got, test.present)
			}
		})
	}
}

// Failed stages are kept for diagnosis, but not without bound.
func TestToolingKeepsABoundedNumberOfFailedStages(t *testing.T) {
	home := t.TempDir()
	spec := validToolingSpec(t)
	tool, err := release.DecodeTooling(spec)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".local/share/upkeeper-tools")
	dir := filepath.Join(base, "releases", toolingIdentity(spec))
	failVenv := true
	clock := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	c := &Context{Home: home, Runner: toolingStageRunner(&failVenv), Now: func() time.Time { return clock }}
	for attempt := 0; attempt < toolingFailedStageLimit+3; attempt++ {
		c.RunID = "run-" + string(rune('a'+attempt))
		clock = clock.Add(time.Minute)
		if err := stageTooling(c, dir, tool); err == nil ||
			!strings.Contains(err.Error(), "OV memory virtualenv failed") {
			t.Fatalf("attempt %d = %v", attempt, err)
		}
	}
	kept, err := os.ReadDir(filepath.Join(base, "failed-stages"))
	if err != nil || len(kept) != toolingFailedStageLimit {
		t.Fatalf("kept %d failed stages (%v), want %d", len(kept), err, toolingFailedStageLimit)
	}
	// The newest attempts are the ones kept, named by when each attempt began;
	// the last attempt is still the live stage.
	if !strings.HasPrefix(kept[0].Name(), "20261006T120300Z") ||
		!strings.HasPrefix(kept[len(kept)-1].Name(), "20261006T120500Z") {
		t.Fatalf("kept stages = %v", kept)
	}
}

// A marker that names another release is not this release's own stage.
func TestToolingRefusesAStageMarkedForAnotherRelease(t *testing.T) {
	home := t.TempDir()
	spec := validToolingSpec(t)
	tool, err := release.DecodeTooling(spec)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".local/share/upkeeper-tools/releases", toolingIdentity(spec))
	adapterTestWrite(t, filepath.Join(dir, toolingStageMarker),
		`{"created_by": "upkeeper", "identity": "0000000000000000000000000000000000000000000000000000000000000000"}`)
	failVenv := false
	c := &Context{Home: home, RunID: "run", Runner: toolingStageRunner(&failVenv)}
	if err := stageTooling(c, dir, tool); err == nil || !strings.Contains(err.Error(), "incomplete tooling stage") {
		t.Fatalf("stage marked for another release = %v", err)
	}
}
