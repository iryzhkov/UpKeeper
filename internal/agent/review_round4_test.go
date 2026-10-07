package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// A bootstrap that crashed after enable --now left the service running, and
// its socket is still on disk when systemctl restart returns, before the new
// process listens. Recovery must settle before probing, as the ordinary
// update path does, rather than disable a healthy service.
func TestRound4SelfReviewRecoverySettlesAfterTheRestart(t *testing.T) {
	c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
	huyangSettleDelay = 200 * time.Millisecond
	adapterTestWrite(t, huyangSocket(c), "")
	var guard sync.Mutex
	listening := true
	setListening := func(value bool) { guard.Lock(); listening = value; guard.Unlock() }
	huyangProbeFunc = func(*Context, string, int) huyangProbeResult {
		guard.Lock()
		defer guard.Unlock()
		if !listening {
			return huyangProbeResult{Error: "connect: connection refused"}
		}
		tools := 19
		return huyangProbeResult{OK: true, Tools: &tools}
	}
	var rebound sync.WaitGroup
	t.Cleanup(rebound.Wait)
	inner := r.handle
	r.handle = func(command procx.Command) procx.Result {
		switch {
		case adapterTestMatches(command.Argv, "is-active"):
			return procx.Result{Stdout: "active"}
		case adapterTestMatches(command.Argv, "enable", "--now"):
			return procx.Result{}
		case adapterTestMatches(command.Argv, "systemctl", "restart"):
			setListening(false)
			rebound.Add(1)
			go func() {
				defer rebound.Done()
				time.Sleep(50 * time.Millisecond)
				setListening(true)
			}()
			return procx.Result{}
		}
		return inner(command)
	}
	before := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": true, "socket": true}
	state, err := huyangApplyCheckout(c, c.Spec("huyang"), before, &[]string{})
	if err != nil || state != "updated" || r.ran("disable", "--now") || pathExists(c.Path(huyangBootstrapPending)) {
		t.Fatalf("recovery probed before the restarted service listened: state=%q err=%v marker=%v calls=%v",
			state, err, pathExists(c.Path(huyangBootstrapPending)), r.calls)
	}
}

// A checkout deleted and cloned again during the build can be given the
// freed inode number, which os.SameFile cannot tell apart. Recovery keeps the
// built directory open so the number cannot be reused, and refuses the clone.
// Before the fix this failed on filesystems that reuse inodes at once (ext4).
func TestRound4SelfReviewRecoveryRefusesACheckoutRecreatedDuringTheBuild(t *testing.T) {
	c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
	checkout := c.Path(".local/share/huyang")
	inner := r.handle
	r.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "make", "build") {
			if err := os.RemoveAll(checkout); err != nil {
				t.Fatal(err)
			}
			adapterTestWrite(t, filepath.Join(checkout, ".git/HEAD"), "x\n")
			adapterTestWrite(t, filepath.Join(checkout, "bin/huyang"), "another clone's binary\n")
		}
		return inner(command)
	}
	before := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
	state, err := huyangApplyCheckout(c, c.Spec("huyang"), before, &[]string{})
	if err == nil || r.ran("enable", "--now") || !pathExists(c.Path(huyangBootstrapPending)) {
		t.Fatalf("recreated checkout accepted: state=%s err=%v marker=%v calls=%v",
			state, err, pathExists(c.Path(huyangBootstrapPending)), r.calls)
	}
	if !strings.Contains(err.Error(), "replaced while it built") {
		t.Fatalf("refusal does not say the checkout was replaced: %v", err)
	}
}

// The pending marker is UpKeeper's regular file. Anything else at its path
// cannot be removed once the bootstrap is finished, which left the service
// running and every later run building and restarting it again; recovery
// refuses before it builds instead.
func TestRound4SelfReviewRecoveryRefusesAMarkerThatIsNotAFile(t *testing.T) {
	c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
	marker := c.Path(huyangBootstrapPending)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	adapterTestWrite(t, filepath.Join(marker, "keep"), "x\n")
	before := map[string]any{"commit": huyangPinned, "dirty": false, "service_active": false, "socket": false}
	state, err := huyangApplyCheckout(c, c.Spec("huyang"), before, &[]string{})
	if err == nil || r.ran("make", "build") || r.ran("enable", "--now") || !pathExists(marker) {
		t.Fatalf("marker directory not refused before the build: state=%s err=%v calls=%v", state, err, r.calls)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("refusal does not explain the marker: %v", err)
	}
}

// When a fresh bootstrap's service fails the probe and stopping it fails too,
// the error still says why the start failed, as recovery's does.
func TestRound4SelfReviewABootstrapStopFailureKeepsTheStartFailure(t *testing.T) {
	runner := &adapterTestRunner{}
	c := adapterTestContext(t, runner)
	c.Components["huyang"] = huyangTestSpec()
	c.Env = []string{"PATH=" + freshTestTools(t, c.Home, "git", "go", "make")}
	huyangTestPatch(t, huyangTestProbe(false, 0))
	runner.handle = func(command procx.Command) procx.Result {
		switch {
		case adapterTestMatches(command.Argv, "git", "clone"):
			candidate := command.Argv[len(command.Argv)-1]
			adapterTestWrite(t, filepath.Join(candidate, ".git/HEAD"), "ref: refs/heads/main\n")
		case adapterTestMatches(command.Argv, "enable", "--now"):
			adapterTestWrite(t, huyangSocket(c), "")
		case adapterTestMatches(command.Argv, "disable", "--now"):
			return procx.Result{Code: 1, Stderr: "Failed to connect to bus"}
		}
		return procx.Result{}
	}
	_, err := huyangBootstrap(c, c.Spec("huyang"), &[]string{})
	if err == nil || !strings.Contains(err.Error(), "probe failed") || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("bootstrap error lost the start or the stop failure: %v", err)
	}
}

// A checkout an interrupted bootstrap left whose HEAD cannot be read is
// refused as that bootstrap's, naming the marker to keep, rather than as a
// directory that is not a Git checkout.
func TestRound4SelfReviewAnUnreadableInterruptedCheckoutNamesTheMarker(t *testing.T) {
	c, r := huyangCrashWorld(t, fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang"))
	before := map[string]any{"commit": nil, "dirty": nil, "service_active": false, "socket": false}
	_, err := huyangApplyCheckout(c, c.Spec("huyang"), before, &[]string{})
	if err == nil || r.ran("enable", "--now") || !pathExists(c.Path(huyangBootstrapPending)) {
		t.Fatalf("unreadable interrupted checkout accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "interrupted Huyang bootstrap") || !strings.Contains(err.Error(), huyangBootstrapPending) {
		t.Fatalf("refusal does not name the interrupted bootstrap and its marker: %v", err)
	}
}
