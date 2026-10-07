package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// A global prefix without a lib directory makes npm answer ENOENT, but a
// linked t3 command under its bin directory is still a trace of T3. The
// error path must look for it as the ordinary empty answer does.
func TestSelfReviewRound3ACommandLinkBesideAMissingLibIsATrace(t *testing.T) {
	c, r := stewardPairPrepare(t, stewardPairHost{})
	prefix := filepath.Join(t.TempDir(), "prefix")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/other/lib/node_modules/t3/bin/t3.js", filepath.Join(prefix, "bin/t3")); err != nil {
		t.Fatal(err)
	}
	inner := r.handle
	r.handle = func(command procx.Command) procx.Result {
		switch {
		case adapterTestMatches(command.Argv, "npm", "ls"):
			return procx.Result{Code: 254, Stdout: `{"error":{"code":"ENOENT","summary":"ENOENT: no such file or directory, lstat '` + prefix + `/lib'"}}`}
		case adapterTestMatches(command.Argv, "npm", "prefix"):
			return procx.Result{Stdout: prefix + "\n"}
		}
		return inner(command)
	}
	state, actions, err := stewardPairApply(t, c)
	if err == nil {
		t.Fatalf("prefix without lib but with a t3 command accepted as having no T3: state=%s actions=%v", state, actions)
	}
}

// A bootstrap interrupted before its service started, then a release that
// moves the pin: the operator follows the refusal and puts the checkout on the
// new pin. Recovery must build that commit before it starts the service, never
// serve the new sources with the binary built for the old pin.
func TestSelfReviewRound3RecoveryAfterAPinChangeBuildsBeforeStarting(t *testing.T) {
	c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
	spec := c.Spec("huyang")
	spec["commit"] = huyangLocal
	inner := r.handle
	r.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "rev-parse", "HEAD") {
			return procx.Result{Stdout: huyangLocal}
		}
		return inner(command)
	}
	before := map[string]any{"commit": huyangLocal, "dirty": false, "service_active": false, "socket": false}
	state, err := huyangApplyCheckout(c, spec, before, &[]string{})
	if err != nil || state != "updated" || !huyangRanBefore(r, []string{"make", "build"}, []string{"enable", "--now"}) {
		t.Fatalf("recovery on the new pin = %q, %v; calls %v", state, err, r.calls)
	}
}

// The build output is ignored by git, so a clean status at the pin says
// nothing about bin/huyang. Recovery rebuilds it before starting the service,
// as the ordinary path does for a stopped service, and a failed build starts
// nothing and keeps the pending marker.
func TestSelfReviewRound3RecoveryNeverStartsABinaryItDidNotBuild(t *testing.T) {
	t.Run("build first", func(t *testing.T) {
		c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
		adapterTestWrite(t, c.Path(".local/share/huyang/bin/huyang"), "#!/bin/sh\n# replaced by hand\n")
		before := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
		state, err := huyangApplyCheckout(c, c.Spec("huyang"), before, &[]string{})
		if err != nil || state != "updated" || !huyangRanBefore(r, []string{"make", "build"}, []string{"enable", "--now"}) {
			t.Fatalf("recovery = %q, %v; calls %v", state, err, r.calls)
		}
	})
	t.Run("failed build", func(t *testing.T) {
		c, r := huyangCrashWorld(t, "")
		inner := r.handle
		r.handle = func(command procx.Command) procx.Result {
			if adapterTestMatches(command.Argv, "make", "build") {
				return procx.Result{Code: 2, Stderr: "build failed"}
			}
			return inner(command)
		}
		before := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
		state, err := huyangApplyCheckout(c, c.Spec("huyang"), before, &[]string{})
		if err == nil || r.ran("enable", "--now") || !pathExists(c.Path(huyangBootstrapPending)) {
			t.Fatalf("failed build = %q, %v; marker kept=%v; calls %v",
				state, err, pathExists(c.Path(huyangBootstrapPending)), r.calls)
		}
		if pathExists(c.Path(".config/systemd/user/huyang.service")) {
			t.Fatal("failed build still seeded the unit")
		}
	})
}

// A recovery whose service does not answer is stopped and disabled again, as a
// failed bootstrap is, rather than left enabled at boot unproven.
func TestSelfReviewRound3AFailedRecoveryDisablesTheService(t *testing.T) {
	c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
	huyangTestPatch(t, huyangTestProbe(false, 0))
	before := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
	state, err := huyangApplyCheckout(c, c.Spec("huyang"), before, &[]string{})
	if err == nil || !huyangRanBefore(r, []string{"enable", "--now"}, []string{"disable", "--now", "huyang.service"}) {
		t.Fatalf("failed recovery = %q, %v; calls %v", state, err, r.calls)
	}
	if !pathExists(c.Path(huyangBootstrapPending)) {
		t.Fatal("failed recovery dropped the pending marker")
	}
}

// huyangRanBefore reports whether a call matching first ran, and a call
// matching then ran after it.
func huyangRanBefore(r *adapterTestRunner, first, then []string) bool {
	for index, argv := range r.calls {
		if adapterTestMatches(argv, first...) {
			for _, later := range r.calls[index+1:] {
				if adapterTestMatches(later, then...) {
					return true
				}
			}
			return false
		}
	}
	return false
}

// Recovery builds for up to ten minutes before it seeds and starts the unit.
// Whatever the operator does to the checkout or the unit meanwhile is theirs:
// recovery checks both again after the build, starts nothing and keeps the
// pending marker if either changed or can no longer be read.
func TestRound3ReviewRecoveryRevalidatesAfterBuild(t *testing.T) {
	seeded := fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang")
	operator := "[Service]\nExecStart=/operator/huyang\n"
	for _, change := range []string{
		"dirty", "off-pin", "head-unreadable", "operator-unit", "masked-unit",
		"unit-appears", "partial-unit-rewritten",
	} {
		t.Run(change, func(t *testing.T) {
			before := seeded
			switch change {
			case "unit-appears":
				before = ""
			case "partial-unit-rewritten":
				before = seeded[:40]
			}
			c, r := huyangCrashWorld(t, before)
			inner := r.handle
			built := false
			unit := c.Path(".config/systemd/user/huyang.service")
			r.handle = func(cmd procx.Command) procx.Result {
				if adapterTestMatches(cmd.Argv, "make", "build") {
					built = true
					switch change {
					case "operator-unit", "unit-appears":
						adapterTestWrite(t, unit, operator)
					case "partial-unit-rewritten":
						adapterTestWrite(t, unit, seeded[:20])
					case "masked-unit":
						if err := os.Remove(unit); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink("/dev/null", unit); err != nil {
							t.Fatal(err)
						}
					}
				}
				if built && change == "dirty" && adapterTestMatches(cmd.Argv, "status", "--porcelain") {
					return procx.Result{Stdout: " M main.go\n"}
				}
				if built && change == "off-pin" && adapterTestMatches(cmd.Argv, "rev-parse", "HEAD") {
					return procx.Result{Stdout: huyangLocal}
				}
				if built && change == "head-unreadable" && adapterTestMatches(cmd.Argv, "rev-parse", "HEAD") {
					return procx.Result{Code: 128, Stderr: "fatal: not a git repository"}
				}
				return inner(cmd)
			}
			observed := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
			state, err := huyangApplyCheckout(c, c.Spec("huyang"), observed, &[]string{})
			if !built {
				t.Fatalf("recovery did not build: state=%s err=%v calls=%v", state, err, r.calls)
			}
			if err == nil || r.ran("enable", "--now") || !pathExists(c.Path(huyangBootstrapPending)) {
				t.Fatalf("%s during build accepted: state=%s err=%v marker=%v calls=%v",
					change, state, err, pathExists(c.Path(huyangBootstrapPending)), r.calls)
			}
			switch change {
			case "operator-unit", "unit-appears":
				if raw, _ := os.ReadFile(unit); string(raw) != operator {
					t.Fatalf("operator unit overwritten: %q", raw)
				}
			case "partial-unit-rewritten":
				if raw, _ := os.ReadFile(unit); string(raw) != seeded[:20] {
					t.Fatalf("unit overwritten: %q", raw)
				}
			case "masked-unit":
				if link, _ := os.Readlink(unit); link != "/dev/null" {
					t.Fatalf("mask removed: %q", link)
				}
			}
		})
	}
}

// A crash after enable --now, or a reboot, leaves the seeded service running
// the binary of the earlier pin while the marker stays. enable --now does not
// restart a running unit, so recovery must restart it to serve what it built.
func TestRound3SelfReviewRecoveryRestartsARunningService(t *testing.T) {
	c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
	spec := c.Spec("huyang")
	spec["commit"] = huyangLocal
	adapterTestWrite(t, huyangSocket(c), "")
	serving, built := "old-pin binary", ""
	inner := r.handle
	r.handle = func(cmd procx.Command) procx.Result {
		switch {
		case adapterTestMatches(cmd.Argv, "rev-parse", "HEAD"):
			return procx.Result{Stdout: huyangLocal}
		case adapterTestMatches(cmd.Argv, "is-active"):
			return procx.Result{Stdout: "active"}
		case adapterTestMatches(cmd.Argv, "make", "build"):
			built = "new-pin binary"
		case adapterTestMatches(cmd.Argv, "systemctl", "restart"), adapterTestMatches(cmd.Argv, "systemctl", "stop"):
			serving = built
		case adapterTestMatches(cmd.Argv, "enable", "--now"):
			return procx.Result{}
		}
		return inner(cmd)
	}
	observed := map[string]any{"commit": huyangLocal, "dirty": false, "service_active": true, "socket": true}
	state, err := huyangApplyCheckout(c, spec, observed, &[]string{})
	if err != nil || serving != built {
		t.Fatalf("recovery reported %s (err=%v) while %s still serves (built %s); calls=%v",
			state, err, serving, built, r.calls)
	}
}

// The checkout is replaced during the build by another clone on the same
// commit: clean and on the pin, but its git-ignored binary is not the one
// recovery built.
func TestRound3SelfReviewRecoveryRefusesACheckoutSwappedDuringTheBuild(t *testing.T) {
	c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
	checkout := c.Path(".local/share/huyang")
	inner := r.handle
	r.handle = func(cmd procx.Command) procx.Result {
		if adapterTestMatches(cmd.Argv, "make", "build") {
			adapterTestWrite(t, checkout+"/bin/huyang", "built by recovery\n")
			if err := os.Rename(checkout, checkout+".old"); err != nil {
				t.Fatal(err)
			}
			adapterTestWrite(t, checkout+"/.git/HEAD", "x\n")
			adapterTestWrite(t, checkout+"/bin/huyang", "stale\n")
		}
		return inner(cmd)
	}
	observed := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
	state, err := huyangApplyCheckout(c, c.Spec("huyang"), observed, &[]string{})
	if err == nil || r.ran("enable", "--now") || !pathExists(c.Path(huyangBootstrapPending)) {
		t.Fatalf("swapped checkout accepted: state=%s err=%v calls=%v", state, err, r.calls)
	}
}

// An unreadable unit is refused like any other unit recovery cannot vouch
// for: with the marker kept and the operator told what to do.
func TestRound3SelfReviewAnUnreadableUnitIsExplained(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	c, r := huyangCrashWorld(t, "[Service]\n")
	unit := c.Path(".config/systemd/user/huyang.service")
	if err := os.Chmod(unit, 0); err != nil {
		t.Fatal(err)
	}
	observed := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
	_, err := huyangApplyCheckout(c, c.Spec("huyang"), observed, &[]string{})
	if err == nil || !strings.Contains(err.Error(), "inspect it") || !strings.Contains(err.Error(), huyangBootstrapPending) {
		t.Fatalf("unreadable unit error lacks guidance: %v", err)
	}
	if r.ran("enable", "--now") || !pathExists(c.Path(huyangBootstrapPending)) {
		t.Fatalf("unreadable unit accepted: calls=%v", r.calls)
	}
}
