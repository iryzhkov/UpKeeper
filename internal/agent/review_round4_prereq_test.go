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

// The self-review of this round found the rest of the cases in which the
// preflight and huyangApplyCheckout disagreed about whether Huyang builds.

// huyangRunningWorld is huyangPrereqWorld with the service running, its socket
// present and no interrupted bootstrap, and the probe answering ok.
func huyangRunningWorld(t *testing.T, ok bool, tools ...string) (*Context, *adapterTestRunner) {
	t.Helper()
	c, runner, _ := huyangPrereqWorld(t, tools...)
	if err := os.Remove(c.Path(huyangBootstrapPending)); err != nil {
		t.Fatal(err)
	}
	adapterTestWrite(t, huyangSocket(c), "")
	huyangTestPatch(t, huyangTestProbe(ok, 19))
	inner := runner.handle
	runner.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "is-active") {
			return procx.Result{Stdout: "active"}
		}
		return inner(command)
	}
	return c, runner
}

// A running service on the pin that does not answer the probe is rebuilt, so
// a host without make is blocked with the remedy rather than failing the
// build and its rollback and skipping everything after Huyang.
func TestRound4SelfReviewAServiceFailingTheProbeNeedsTheBuildTools(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry run %v", dry), func(t *testing.T) {
			c, runner := huyangRunningWorld(t, false, "git", "go")
			c.DryRun = dry
			c.Payload.DryRun = dry
			result := Execute(c)
			entry := resultEntry(t, result, "huyang")
			next := resultEntry(t, result, "independent-next")
			message, _ := entry["error"].(string)
			if entry["state"] != "blocked" || !strings.Contains(message, "sudo apt-get install -y make") ||
				next["state"] != "updated" || runner.ran("make", "build") {
				t.Fatalf("huyang = %v, next = %v, calls %v", entry, next, runner.calls)
			}
		})
	}
}

// A checkout ahead of the pin is kept and never built, so it needs neither Go
// nor make.
func TestRound4SelfReviewACheckoutAheadOfThePinNeedsNoBuildTools(t *testing.T) {
	c, runner := huyangRunningWorld(t, true, "git")
	inner := runner.handle
	runner.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "rev-parse", "HEAD") {
			return procx.Result{Stdout: huyangLocal}
		}
		return inner(command)
	}
	if entry := resultEntry(t, Execute(c), "huyang"); entry["state"] != "ahead" {
		t.Fatalf("huyang = %v", entry)
	}
}

// An interrupted bootstrap whose checkout has local changes is refused, not
// built: the operator is told about the changes and the marker, and an
// opted-in host installs nothing first.
func TestRound4SelfReviewARefusedRecoveryInstallsNothing(t *testing.T) {
	c, runner, _ := huyangPrereqWorld(t, "git", "go")
	c.Host.InstallOSPackages = true
	inner := runner.handle
	runner.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "status", "--porcelain") {
			return procx.Result{Stdout: " M main.go\n"}
		}
		return inner(command)
	}
	entry := resultEntry(t, Execute(c), "huyang")
	message, _ := entry["error"].(string)
	if !strings.Contains(message, "has local changes") || !strings.Contains(message, huyangBootstrapPending) ||
		runner.ran("sudo") || runner.ran("make", "build") {
		t.Fatalf("huyang = %v, calls %v", entry, runner.calls)
	}
}
