package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// These tests come from the second review of the round-4 chain: finishing an
// interrupted Huyang bootstrap builds the checkout again although its HEAD is
// already the pin, so it needs Go and make like any other build.

// huyangPrereqWorld is the Huyang crash world run through Execute on a host
// whose PATH has only the named tools, with an unrelated component after
// Huyang. A make that is not on that PATH fails as it would on the host.
func huyangPrereqWorld(t *testing.T, tools ...string) (*Context, *adapterTestRunner, string) {
	t.Helper()
	old, runner := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
	bin := freshTestTools(t, old.Home, tools...)
	inner := runner.handle
	runner.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "make", "build") && !pathExists(filepath.Join(bin, "make")) {
			return procx.Result{Code: -1, Stderr: "make: executable file not found"}
		}
		return inner(command)
	}
	c := freshTestContext(t, old.Home, bin, runner.run, []string{"huyang", "independent-next"},
		map[string]any{"huyang": old.Spec("huyang"), "independent-next": map[string]any{}})
	replaceHandler(t, "independent-next", Handler{
		Observe: func(*Context, map[string]any) (map[string]any, error) { return map[string]any{}, nil },
		Apply: func(*Context, map[string]any, map[string]any, *[]string) (string, error) {
			return "updated", nil
		},
	})
	return c, runner, bin
}

// A pending bootstrap on a host without make is blocked with the remedy, in a
// dry run and a real one, and does not stop the unrelated component after it.
func TestRound4ReviewRecoveryNeedsTheBuildTools(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry run %v", dry), func(t *testing.T) {
			c, runner, _ := huyangPrereqWorld(t, "git", "go")
			c.DryRun = dry
			c.Payload.DryRun = dry
			result := Execute(c)
			entry := resultEntry(t, result, "huyang")
			next := resultEntry(t, result, "independent-next")
			message, _ := entry["error"].(string)
			if entry["state"] != "blocked" || !strings.Contains(message, "sudo apt-get install -y make") ||
				next["state"] != "updated" {
				t.Fatalf("huyang = %v, next = %v", entry, next)
			}
			if runner.ran("make", "build") || !pathExists(c.Path(huyangBootstrapPending)) {
				t.Fatalf("a blocked recovery changed the host: calls %v", runner.calls)
			}
		})
	}
}

// A Go too old to build Huyang blocks the recovery in the same way.
func TestRound4ReviewRecoveryNeedsANewEnoughGo(t *testing.T) {
	c, runner, _ := huyangPrereqWorld(t, "git", "go", "make")
	inner := c.Runner
	c.Runner = func(command procx.Command) procx.Result {
		if strings.Join(command.Argv, " ") == "go env GOVERSION" {
			return procx.Result{Stdout: "go1.19.8\n"}
		}
		return inner(command)
	}
	entry := resultEntry(t, Execute(c), "huyang")
	message, _ := entry["error"].(string)
	if entry["state"] != "blocked" || !strings.Contains(message, "Go 1.21 or newer") || runner.ran("make", "build") {
		t.Fatalf("huyang = %v, calls %v", entry, runner.calls)
	}
}

// On a host that opted in, the missing make is installed before the recovery
// builds, and the recovery then finishes.
func TestRound4ReviewOptedInRecoveryInstallsTheBuildTools(t *testing.T) {
	c, runner, bin := huyangPrereqWorld(t, "git", "go")
	c.Host.InstallOSPackages = true
	installed := []string{}
	inner := runner.handle
	runner.handle = func(command procx.Command) procx.Result {
		switch {
		case adapterTestMatches(command.Argv, "sudo", "-n", "true"):
			return procx.Result{}
		case adapterTestMatches(command.Argv, "sudo", "apt-get", "install"):
			installed = append(installed, strings.Join(command.Argv, " "))
			freshTestTools(t, filepath.Dir(bin), "make")
			return procx.Result{}
		}
		return inner(command)
	}
	entry := resultEntry(t, Execute(c), "huyang")
	if entry["state"] != "updated" || len(installed) != 1 || !strings.Contains(installed[0], "install -y make") {
		t.Fatalf("huyang = %v, installed = %v", entry, installed)
	}
	if !runner.ran("make", "build") || pathExists(c.Path(huyangBootstrapPending)) {
		t.Fatalf("the recovery did not finish: calls %v", runner.calls)
	}
}

// A pinned checkout whose service is not running is rebuilt, so it needs the
// build tools too.
func TestRound4ReviewAStoppedServiceOnThePinNeedsTheBuildTools(t *testing.T) {
	c, runner, _ := huyangPrereqWorld(t, "git", "go")
	if err := os.Remove(c.Path(huyangBootstrapPending)); err != nil {
		t.Fatal(err)
	}
	entry := resultEntry(t, Execute(c), "huyang")
	message, _ := entry["error"].(string)
	if entry["state"] != "blocked" || !strings.Contains(message, "sudo apt-get install -y make") ||
		runner.ran("make", "build") {
		t.Fatalf("huyang = %v, calls %v", entry, runner.calls)
	}
}

// A healthy installation on the pin still needs neither Go nor make.
func TestRound4ReviewAHealthyInstallationNeedsNoBuildTools(t *testing.T) {
	c, runner, _ := huyangPrereqWorld(t, "git")
	if err := os.Remove(c.Path(huyangBootstrapPending)); err != nil {
		t.Fatal(err)
	}
	adapterTestWrite(t, huyangSocket(c), "")
	inner := runner.handle
	runner.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "is-active") {
			return procx.Result{Stdout: "active"}
		}
		return inner(command)
	}
	entry := resultEntry(t, Execute(c), "huyang")
	if entry["state"] != "converged" || runner.ran("make", "build") {
		t.Fatalf("huyang = %v, calls %v", entry, runner.calls)
	}
}
