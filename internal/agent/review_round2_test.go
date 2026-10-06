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

// The prefix npm prints is looked at exactly as the system resolves it: a
// space belongs to the name, and ".." after a symlink leads into the link's
// target, not back to where the link lives.
func TestSelfReviewRound2PrefixIsProbedAsTheSystemResolvesIt(t *testing.T) {
	cases := map[string]func(t *testing.T, base string) (printed, installed string){
		"trailing space in the name": func(t *testing.T, base string) (string, string) {
			return filepath.Join(base, "pre "), filepath.Join(base, "pre ")
		},
		"dot-dot through a symlink": func(t *testing.T, base string) (string, string) {
			real := filepath.Join(base, "real")
			if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(base, "home"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(real, "sub"), filepath.Join(base, "home", "link")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(base, "home", "link") + "/..", real
		},
	}
	for name, layout := range cases {
		t.Run(name, func(t *testing.T) {
			c, r := stewardPairPrepare(t, stewardPairHost{})
			printed, installed := layout(t, t.TempDir())
			adapterTestWrite(t, filepath.Join(installed, "lib/node_modules/t3/package.json"), `{"name":"t3","version":"0.0.38"}`)
			stewardPairNPMPrefix(r, printed)
			state, actions, err := stewardPairApply(t, c)
			if err == nil {
				t.Fatalf("T3 on disk accepted as absent: state=%s actions=%v", state, actions)
			}
		})
	}
}

// A refused recovery leaves the host as it found it: no unit is seeded for a
// checkout that changed since the observation.
func TestSelfReviewRound2RefusedRecoverySeedsNoUnit(t *testing.T) {
	c, r := huyangCrashWorld(t, "")
	inner := r.handle
	r.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "status", "--porcelain") {
			return procx.Result{Stdout: " M main.go"}
		}
		return inner(command)
	}
	before := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
	_, err := huyangApplyCheckout(c, c.Spec("huyang"), before, &[]string{})
	if err == nil {
		t.Fatal("changed interrupted checkout accepted")
	}
	if unit := c.Path(".config/systemd/user/huyang.service"); pathExists(unit) {
		t.Fatalf("refused recovery (%v) still seeded %s", err, unit)
	}
}

// A refused recovery tells the operator to keep the pending marker, because
// only the bootstrap enables Huyang at boot: following the advice and cleaning
// the checkout lets the next run finish it, enable included.
func TestSelfReviewRound2RefusedRecoveryKeepsTheWayToEnable(t *testing.T) {
	for name, dirtyObserved := range map[string]bool{"dirty when observed": true, "dirty after observing": false} {
		t.Run(name, func(t *testing.T) {
			c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
			spec := c.Spec("huyang")
			dirty := dirtyObserved
			inner := r.handle
			r.handle = func(command procx.Command) procx.Result {
				if adapterTestMatches(command.Argv, "status", "--porcelain") && dirty {
					return procx.Result{Stdout: "?? scratch"}
				}
				return inner(command)
			}
			before, err := huyangObserve(c, spec)
			if err != nil {
				t.Fatal(err)
			}
			dirty = true
			_, err = huyangApply(c, spec, before, &[]string{})
			marker := c.Path(huyangBootstrapPending)
			if err == nil || !strings.Contains(err.Error(), "Keep "+marker) || strings.Contains(err.Error(), "then remove") {
				t.Fatalf("refusal does not keep the marker: %v", err)
			}
			if r.ran("enable", "--now") {
				t.Fatal("refused recovery started the service")
			}
			dirty = false
			before, err = huyangObserve(c, spec)
			if err != nil {
				t.Fatal(err)
			}
			state, err := huyangApply(c, spec, before, &[]string{})
			if err != nil || state != "updated" || !r.ran("enable", "--now", "huyang.service") || pathExists(marker) {
				t.Fatalf("clean rerun = %q, %v; calls %v", state, err, r.calls)
			}
		})
	}
}

// A unit the operator masked or emptied reads as nothing, which is no proof of
// a seeded unit cut short; recovery neither replaces nor starts it.
func TestSelfReviewRound2RecoveryLeavesAMaskedOrEmptyUnitAlone(t *testing.T) {
	for name, place := range map[string]func(t *testing.T, unit string){
		"masked": func(t *testing.T, unit string) {
			if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/dev/null", unit); err != nil {
				t.Fatal(err)
			}
		},
		"empty": func(t *testing.T, unit string) { adapterTestWrite(t, unit, "") },
	} {
		t.Run(name, func(t *testing.T) {
			c, r := huyangCrashWorld(t, "")
			unit := c.Path(".config/systemd/user/huyang.service")
			place(t, unit)
			before := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
			state, err := huyangApplyCheckout(c, c.Spec("huyang"), before, &[]string{})
			info, statErr := os.Lstat(unit)
			if err == nil || r.ran("enable", "--now") || statErr != nil || info.Size() != 0 && info.Mode().IsRegular() {
				t.Fatalf("operator's unit replaced or started: state=%s err=%v calls=%v", state, err, r.calls)
			}
			if name == "masked" {
				if link, _ := os.Readlink(unit); link != "/dev/null" {
					t.Fatalf("mask removed: link=%q", link)
				}
			}
		})
	}
}

// The command npm links for T3 is a trace of T3, even when the package it
// points at lives outside the current prefix.
func TestSelfReviewRound2AT3CommandLinkIsATrace(t *testing.T) {
	c, r := stewardPairPrepare(t, stewardPairHost{})
	prefix := filepath.Join(t.TempDir(), "prefix")
	elsewhere := filepath.Join(t.TempDir(), "other/lib/node_modules/t3/bin/t3.js")
	adapterTestWrite(t, elsewhere, "#!/usr/bin/env node\n")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(prefix, "bin/t3")); err != nil {
		t.Fatal(err)
	}
	stewardPairNPMPrefix(r, prefix)
	state, actions, err := stewardPairApply(t, c)
	if err == nil {
		t.Fatalf("host with a t3 command accepted as having no T3: state=%s actions=%v", state, actions)
	}
}

// A dry run of a recovery the real run would refuse does not report success.
func TestSelfReviewRound2RecoveryDryRunMatchesTheRealRun(t *testing.T) {
	run := func(dry bool) (string, error) {
		c, r := huyangCrashWorld(t, "")
		c.DryRun = dry
		inner := r.handle
		r.handle = func(command procx.Command) procx.Result {
			if adapterTestMatches(command.Argv, "status", "--porcelain") {
				return procx.Result{Code: 128, Stderr: "fatal: unsafe repository"}
			}
			return inner(command)
		}
		before := map[string]any{"commit": huyangPinned, "dirty": nil, "service_active": false, "socket": false}
		return huyangApplyCheckout(c, c.Spec("huyang"), before, &[]string{})
	}
	dryState, dryErr := run(true)
	realState, realErr := run(false)
	if dryErr == nil || realErr == nil {
		t.Fatalf("dry run state=%q err=%v; real run state=%q err=%v", dryState, dryErr, realState, realErr)
	}
}
