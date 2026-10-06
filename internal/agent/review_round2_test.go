package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// stewardPairNPMPrefix answers npm's prefix query with prefix and every other
// npm listing with the empty answer npm gives for a package it did not list.
func stewardPairNPMPrefix(r *adapterTestRunner, prefix string) {
	inner := r.handle
	r.handle = func(command procx.Command) procx.Result {
		switch {
		case adapterTestMatches(command.Argv, "npm", "ls"):
			return procx.Result{Code: 1, Stdout: `{"name": "lib"}`}
		case adapterTestMatches(command.Argv, "npm", "prefix"):
			return procx.Result{Stdout: prefix + "\n"}
		}
		return inner(command)
	}
}

// npm silently leaves out a package it cannot read: with an unreadable global
// node_modules it answers {"name":"lib"} and nothing on stderr. That answer is
// no proof the package is absent, so the pair keeps its proof requirement.
func TestReviewRound2UnreadableGlobalPackagesAreUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	c, r := stewardPairPrepare(t, stewardPairHost{})
	prefix := filepath.Join(t.TempDir(), "prefix")
	modules := filepath.Join(prefix, "lib/node_modules")
	adapterTestWrite(t, filepath.Join(modules, "t3/package.json"), `{"name":"t3","version":"0.0.38"}`)
	if err := os.Chmod(modules, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(modules, 0o700) })
	stewardPairNPMPrefix(r, prefix)
	state, actions, err := stewardPairApply(t, c)
	if err == nil {
		t.Fatalf("installed but unreadable T3 accepted as fresh: state=%s actions=%v", state, actions)
	}
}

// A package directory npm left out of its answer for any reason is still a T3
// on the host.
func TestReviewRound2APackageOnDiskThatNPMOmitsIsUnknown(t *testing.T) {
	c, r := stewardPairPrepare(t, stewardPairHost{})
	prefix := filepath.Join(t.TempDir(), "prefix")
	adapterTestWrite(t, filepath.Join(prefix, "lib/node_modules/t3/package.json"), `{"name":"t3","version":"0.0.38"}`)
	stewardPairNPMPrefix(r, prefix)
	state, actions, err := stewardPairApply(t, c)
	if err == nil {
		t.Fatalf("T3 on disk accepted as absent: state=%s actions=%v", state, actions)
	}
}

// Genuinely empty global trees still converge: a node_modules without t3, a
// lib without node_modules, and a prefix without lib.
func TestReviewRound2EmptyGlobalTreesStillConverge(t *testing.T) {
	for name, existing := range map[string]string{
		"node_modules without t3":  "lib/node_modules/npm",
		"lib without node_modules": "lib",
		"prefix without lib":       ".",
	} {
		t.Run(name, func(t *testing.T) {
			c, r := stewardPairPrepare(t, stewardPairHost{})
			prefix := filepath.Join(t.TempDir(), "prefix")
			if err := os.MkdirAll(filepath.Join(prefix, existing), 0o700); err != nil {
				t.Fatal(err)
			}
			stewardPairNPMPrefix(r, prefix)
			state, actions, err := stewardPairApply(t, c)
			if err != nil || state != "updated" {
				t.Fatalf("fresh host refused: state=%s actions=%v err=%v", state, actions, err)
			}
		})
	}
}

// The same unreadable tree, answered by the real npm rather than a stand-in.
func TestReviewRound2RealNPMUnreadableInstalledT3IsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is not installed")
	}
	c, r := stewardPairPrepare(t, stewardPairHost{})
	prefix := filepath.Join(t.TempDir(), "prefix")
	modules := filepath.Join(prefix, "lib/node_modules")
	adapterTestWrite(t, filepath.Join(modules, "t3/package.json"), `{"name":"t3","version":"0.0.38"}`)
	env := append(os.Environ(), "npm_config_prefix="+prefix)
	listed := procx.Run(procx.Command{Argv: []string{"npm", "ls", "-g", "--depth=0", "--json", "t3"}, Env: env})
	if !listed.OK() || !strings.Contains(listed.Stdout, `"t3"`) {
		t.Fatalf("readable npm fixture: %s", listed.Output())
	}
	if err := os.Chmod(modules, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(modules, 0o700) })
	inner := r.handle
	r.handle = func(command procx.Command) procx.Result {
		if len(command.Argv) > 0 && command.Argv[0] == "npm" {
			command.Env = env
			return procx.Run(command)
		}
		return inner(command)
	}
	state, actions, err := stewardPairApply(t, c)
	if err == nil {
		t.Fatalf("installed but unreadable T3 accepted as fresh: state=%s actions=%v", state, actions)
	}
}

// An interrupted bootstrap is finished only for a checkout nobody has changed
// since: local changes outrank the release, as they do on every other path.
func TestReviewRound2DirtyBootstrapCheckoutIsRefused(t *testing.T) {
	r := &adapterTestRunner{}
	c := adapterTestContext(t, r)
	spec := huyangTestSpec()
	c.Components["huyang"] = spec
	adapterTestWrite(t, c.Path(huyangBootstrapPending), ".local/share/huyang\n")
	adapterTestWrite(t, c.Path(".local/share/huyang/local-work"), "operator changed the checkout\n")
	adapterTestWrite(t, c.Path(".config/systemd/user/huyang.service"), fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
	huyangTestPatch(t, huyangTestProbe(true, 19))
	started := false
	r.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "enable", "--now") {
			started = true
			adapterTestWrite(t, huyangSocket(c), "")
		}
		if adapterTestMatches(command.Argv, "is-active") {
			return procx.Result{Stdout: "active"}
		}
		return procx.Result{}
	}
	before := map[string]any{"commit": huyangPinned, "dirty": true, "service_active": false, "socket": false}
	state, err := huyangApplyCheckout(c, spec, before, &[]string{})
	if err == nil || started {
		t.Fatalf("dirty interrupted checkout started: state=%s err=%v started=%v", state, err, started)
	}
}

// A checkout that was clean when observed but changed, moved or became
// unreadable before the service is taken over is not started either.
func TestReviewRound2BootstrapRecoveryRechecksTheCheckout(t *testing.T) {
	for name, answer := range map[string]func(argv []string) (procx.Result, bool){
		"changed since observed": func(argv []string) (procx.Result, bool) {
			return procx.Result{Stdout: " M main.go"}, adapterTestMatches(argv, "status", "--porcelain")
		},
		"status unreadable": func(argv []string) (procx.Result, bool) {
			return procx.Result{Code: 128, Stderr: "fatal: not a git repository"}, adapterTestMatches(argv, "status", "--porcelain")
		},
		"moved off the pin": func(argv []string) (procx.Result, bool) {
			return procx.Result{Stdout: strings.Repeat("f", 40)}, adapterTestMatches(argv, "rev-parse", "HEAD")
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
			inner := r.handle
			r.handle = func(command procx.Command) procx.Result {
				if result, matched := answer(command.Argv); matched {
					return result
				}
				return inner(command)
			}
			spec := c.Spec("huyang")
			before := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
			state, err := huyangApplyCheckout(c, spec, before, &[]string{})
			if err == nil || r.ran("enable", "--now") {
				t.Fatalf("changed interrupted checkout started: state=%s err=%v calls=%v", state, err, r.calls)
			}
			if !pathExists(c.Path(huyangBootstrapPending)) {
				t.Fatal("the pending marker was removed for an unfinished bootstrap")
			}
		})
	}
}
