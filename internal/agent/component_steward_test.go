package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

const (
	stewardTestVersion = "0.11.0-rc.1"
	stewardTestBinary  = "#!/bin/sh\necho pinned steward\n"
	stewardTestHook    = "export const Hook = async () => ({});"
	stewardTestReport  = "t3-steward " + stewardTestVersion +
		" (tested with T3 0.0.38..0.0.38)"
	stewardTestCheck = "T3 server 0.0.38 serving on 127.0.0.1:7391\n" +
		"shell snapshot: 4 threads, 0 running"
)

func stewardTestDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// stewardTestArchive builds the release archive shape the installer expects.
func stewardTestArchive(t *testing.T, binary, hook []byte, hookIsSymlink bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressor)
	add := func(name string, body []byte, mode int64, symlink bool) {
		header := &tar.Header{Name: name, Mode: mode, Size: int64(len(body))}
		if symlink {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = "/tmp/elsewhere"
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if !symlink {
			if _, err := writer.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(stewardBinaryMember, binary, 0o755, false)
	if hook != nil {
		add(stewardHookMember, hook, 0o644, hookIsSymlink)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func stewardTestArchiveName() string {
	return fmt.Sprintf("t3-steward_%s_%s_%s.tar.gz",
		stewardTestVersion, runtime.GOOS, runtime.GOARCH)
}

// stewardTestServe replaces the release download with a fixed archive.
func stewardTestServe(t *testing.T, archive []byte, digest string) {
	t.Helper()
	saved := stewardFetch
	stewardFetch = func(url string, _ time.Duration, _ int64) ([]byte, error) {
		if strings.HasSuffix(url, "checksums.txt") {
			return []byte(digest + "  " + stewardTestArchiveName() + "\n"), nil
		}
		return archive, nil
	}
	t.Cleanup(func() { stewardFetch = saved })
}

func stewardTestSpec(withHook bool) map[string]any {
	spec := map[string]any{
		"kind":       "github-release",
		"repository": "iryzhkov/t3-steward",
		"version":    stewardTestVersion,
		"path":       ".local/bin/t3-steward",
		"service":    "t3-steward.service",
		"installer":  "t3-update",
	}
	if withHook {
		spec["session_hook_sha256"] = stewardTestDigest([]byte(stewardTestHook))
	}
	return spec
}

// stewardTestHost is the host behaviour one install has to cope with.
type stewardTestHost struct {
	// restartFailures is how many service restarts fail, from the first.
	restartFailures int
	active          bool
	workerLoaded    bool
	dispatched      string
	versionReport   string
	// workerPID is what systemd reports as the worker unit's main process.
	workerPID string
}

func stewardTestPrepare(t *testing.T, host stewardTestHost) (*Context, *adapterTestRunner) {
	t.Helper()
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	restarts := 0
	report := host.versionReport
	if report == "" {
		report = stewardTestReport
	}
	runner.handle = func(command procx.Command) procx.Result {
		last := command.Argv[len(command.Argv)-1]
		switch {
		case last == "version":
			return procx.Result{Stdout: report}
		case last == "check":
			return procx.Result{Stdout: stewardTestCheck}
		case last == "inspect-journal":
			if host.dispatched == "" {
				return procx.Result{Code: 1, Stderr: "no journal"}
			}
			return procx.Result{Stdout: host.dispatched}
		case adapterTestMatches(command.Argv, "npm", "ls"):
			return procx.Result{Code: 1, Stderr: "not installed"}
		case adapterTestMatches(command.Argv, "is-active"):
			if host.active {
				return procx.Result{Stdout: "active"}
			}
			return procx.Result{Stdout: "inactive"}
		case adapterTestMatches(command.Argv, "show", "MainPID"):
			return procx.Result{Stdout: host.workerPID}
		case adapterTestMatches(command.Argv, "show", "LoadState"):
			if host.workerLoaded {
				return procx.Result{Stdout: "loaded"}
			}
			return procx.Result{Stdout: "not-found"}
		case adapterTestMatches(command.Argv, "restart"):
			restarts++
			if restarts <= host.restartFailures {
				return procx.Result{Code: 1, Stderr: "job failed"}
			}
			return procx.Result{}
		default:
			return procx.Result{}
		}
	}
	return context, runner
}

func TestStewardConvergedWhenThePinnedVersionIsAlreadyInstalled(t *testing.T) {
	context, runner := stewardTestPrepare(t, stewardTestHost{active: true})
	spec := stewardTestSpec(false)
	context.Components["t3-steward"] = spec
	adapterTestWrite(t, context.Path(".local/bin/t3-steward"), stewardTestBinary)
	stewardTestServe(t, nil, "")
	before, err := stewardObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if before["version"] != stewardTestVersion {
		t.Fatalf("observed version = %v", before["version"])
	}
	actions := []string{}
	state, err := stewardApply(context, spec, before, &actions)
	if err != nil || state != "converged" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	if runner.ran("restart") {
		t.Fatalf("a converged host was restarted: %v", runner.calls)
	}
}

func TestStewardDryRunDescribesTheInstall(t *testing.T) {
	context, runner := stewardTestPrepare(t, stewardTestHost{active: true})
	context.DryRun = true
	spec := stewardTestSpec(true)
	context.Components["t3-steward"] = spec
	actions := []string{}
	state, err := stewardApply(context, spec,
		map[string]any{"version": stewardTestVersion, "service_active": true}, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	if !adapterTestMatches(actions, "would install and verify pinned steward "+stewardTestVersion) {
		t.Fatalf("actions %v do not describe the preview", actions)
	}
	if _, err := os.Stat(stewardSessionHook(context)); err == nil {
		t.Fatal("a preview installed the session hook")
	}
	if len(runner.calls) == 0 {
		return
	}
	if runner.ran("restart") {
		t.Fatalf("a preview restarted the service: %v", runner.calls)
	}
}

func TestStewardInstallsAndVerifiesThePinnedArtifact(t *testing.T) {
	context, runner := stewardTestPrepare(t, stewardTestHost{active: true})
	spec := stewardTestSpec(true)
	context.Components["t3-steward"] = spec
	archive := stewardTestArchive(t, []byte(stewardTestBinary), []byte(stewardTestHook), false)
	stewardTestServe(t, archive, stewardTestDigest(archive))
	actions := []string{}
	state, err := stewardApplyPinned(context, spec,
		map[string]any{"version": nil, "service_active": false}, &actions)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v (actions %v)", state, err, actions)
	}
	installed, err := os.ReadFile(context.Path(".local/bin/t3-steward"))
	if err != nil || string(installed) != stewardTestBinary {
		t.Fatalf("installed binary = %q (%v)", installed, err)
	}
	hook, err := os.ReadFile(stewardSessionHook(context))
	if err != nil || string(hook) != stewardTestHook {
		t.Fatalf("installed hook = %q (%v)", hook, err)
	}
	if !adapterTestMatches(actions, "installed verified pinned steward "+stewardTestVersion) ||
		!adapterTestMatches(actions, "installed verified OpenCode caller hook") {
		t.Fatalf("actions %v do not report the install", actions)
	}
	if !runner.ran("restart", "t3-steward.service") {
		t.Fatalf("the service was never restarted: %v", runner.calls)
	}
}

func TestStewardRefusesArtifactsThatDoNotVerify(t *testing.T) {
	cases := []struct {
		name      string
		archive   func(t *testing.T) []byte
		digest    func(archive []byte) string
		errorText string
	}{
		{
			name: "an archive whose checksum does not match is refused",
			archive: func(t *testing.T) []byte {
				return stewardTestArchive(t, []byte(stewardTestBinary),
					[]byte(stewardTestHook), false)
			},
			digest:    func([]byte) string { return strings.Repeat("a", 64) },
			errorText: "steward release archive checksum mismatch",
		},
		{
			name: "a session hook whose checksum does not match is refused",
			archive: func(t *testing.T) []byte {
				return stewardTestArchive(t, []byte(stewardTestBinary),
					[]byte("drifted hook"), false)
			},
			digest:    stewardTestDigest,
			errorText: "steward session hook checksum mismatch",
		},
		{
			name: "a session hook member that is not a regular file is refused",
			archive: func(t *testing.T) []byte {
				return stewardTestArchive(t, []byte(stewardTestBinary),
					[]byte(stewardTestHook), true)
			},
			digest:    stewardTestDigest,
			errorText: "invalid steward session hook member",
		},
		{
			name: "an archive without the pinned session hook is refused",
			archive: func(t *testing.T) []byte {
				return stewardTestArchive(t, []byte(stewardTestBinary), nil, false)
			},
			digest:    stewardTestDigest,
			errorText: "steward artifact fetch failed: KeyError",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context, runner := stewardTestPrepare(t, stewardTestHost{active: true})
			spec := stewardTestSpec(true)
			context.Components["t3-steward"] = spec
			archive := test.archive(t)
			stewardTestServe(t, archive, test.digest(archive))
			actions := []string{}
			_, err := stewardApplyPinned(context, spec,
				map[string]any{"version": nil, "service_active": false}, &actions)
			if err == nil || !strings.Contains(err.Error(), test.errorText) {
				t.Fatalf("want error containing %q, got %v", test.errorText, err)
			}
			if _, statError := os.Stat(context.Path(".local/bin/t3-steward")); statError == nil {
				t.Fatal("a refused artifact was installed anyway")
			}
			if runner.ran("restart") {
				t.Fatalf("a refused artifact restarted the service: %v", runner.calls)
			}
		})
	}
}

func TestStewardRestoresThePreviousBinaryAndHookWhenTheRestartFails(t *testing.T) {
	context, runner := stewardTestPrepare(t, stewardTestHost{active: true, restartFailures: 1})
	spec := stewardTestSpec(true)
	context.Components["t3-steward"] = spec
	previous := "#!/bin/sh\necho previous steward\n"
	previousHook := "export const Hook = async () => ({previous: true});"
	adapterTestWrite(t, context.Path(".local/bin/t3-steward"), previous)
	adapterTestWrite(t, stewardSessionHook(context), previousHook)
	archive := stewardTestArchive(t, []byte(stewardTestBinary), []byte(stewardTestHook), false)
	stewardTestServe(t, archive, stewardTestDigest(archive))
	actions := []string{}
	_, err := stewardApplyPinned(context, spec, map[string]any{
		"version":        "0.10.0",
		"service_active": true,
		"binary_sha256":  stewardTestDigest([]byte(previous)),
	}, &actions)
	state, message := classify(err)
	if state != "rolled-back" ||
		message != "restored previous steward binary, service and session hook" {
		t.Fatalf("state = %q, message = %q", state, message)
	}
	restored, readError := os.ReadFile(context.Path(".local/bin/t3-steward"))
	if readError != nil || string(restored) != previous {
		t.Fatalf("binary = %q (%v), want the previous one", restored, readError)
	}
	hook, readError := os.ReadFile(stewardSessionHook(context))
	if readError != nil || string(hook) != previousHook {
		t.Fatalf("hook = %q (%v), want the previous one", hook, readError)
	}
	if !runner.ran("restart", "t3-steward.service") {
		t.Fatalf("the restore never restarted the service: %v", runner.calls)
	}
}

func TestStewardRemovesAnUnverifiedBinaryItInstalledItself(t *testing.T) {
	context, _ := stewardTestPrepare(t, stewardTestHost{active: true, restartFailures: 1})
	spec := stewardTestSpec(false)
	context.Components["t3-steward"] = spec
	archive := stewardTestArchive(t, []byte(stewardTestBinary), nil, false)
	stewardTestServe(t, archive, stewardTestDigest(archive))
	actions := []string{}
	_, err := stewardApplyPinned(context, spec,
		map[string]any{"version": nil, "service_active": false}, &actions)
	if err == nil || !strings.Contains(err.Error(), "new steward failed; removed unverified binary") {
		t.Fatalf("want the unverified-binary refusal, got %v", err)
	}
	if _, statError := os.Stat(context.Path(".local/bin/t3-steward")); statError == nil {
		t.Fatal("the unverified binary was left in place")
	}
}

func TestStewardPersistentWorkerRestart(t *testing.T) {
	cases := []struct {
		name        string
		host        stewardTestHost
		dryRun      bool
		errorText   string
		action      string
		wantRestart bool
	}{
		{
			name: "a host without the worker unit is untouched",
			host: stewardTestHost{active: true},
		},
		{
			name: "an idle worker is restarted onto the installed binary",
			host: stewardTestHost{
				active: true, workerLoaded: true,
				dispatched: `{"workerId":"testhost","dispatched":0,"settlePending":0}`,
			},
			action:      "restarted t3-steward-worker.service onto the pinned steward",
			wantRestart: true,
		},
		{
			name: "a worker whose journal owns dispatched execution defers",
			host: stewardTestHost{
				active: true, workerLoaded: true,
				dispatched: `{"workerId":"testhost","dispatched":2,"settlePending":0}`,
			},
			action: "worker restart deferred: 2 dispatched attempt(s) in the journal",
		},
		{
			name: "a worker whose journal has attempts waiting to settle defers",
			host: stewardTestHost{
				active: true, workerLoaded: true,
				dispatched: `{"workerId":"testhost","dispatched":0,"settlePending":1}`,
			},
			action: "worker restart deferred: 1 settle-pending attempt(s) in the journal",
		},
		{
			name: "a journal summary that does not count settle-pending attempts defers",
			host: stewardTestHost{
				active: true, workerLoaded: true,
				dispatched: `{"workerId":"testhost","dispatched":0}`,
			},
			action: "worker restart deferred: the journal summary does not count settle-pending attempts",
		},
		{
			name:      "an unreadable journal fails instead of restarting",
			host:      stewardTestHost{active: true, workerLoaded: true},
			errorText: "worker journal inspection failed",
		},
		{
			name: "an unparsable journal summary fails instead of restarting",
			host: stewardTestHost{
				active: true, workerLoaded: true, dispatched: "not json",
			},
			errorText: "worker journal inspection returned no usable summary",
		},
		{
			name: "a worker that stays down after a restart is reported",
			host: stewardTestHost{
				workerLoaded: true,
				dispatched:   `{"dispatched":0,"settlePending":0}`,
			},
			errorText:   "persistent worker is inactive after restart",
			wantRestart: true,
		},
		{
			name: "a dry run reports the restart without performing it",
			host: stewardTestHost{
				active: true, workerLoaded: true, dispatched: `{"dispatched":0,"settlePending":0}`,
			},
			dryRun: true,
			action: "would restart t3-steward-worker.service onto the pinned steward",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context, runner := stewardTestPrepare(t, test.host)
			context.DryRun = test.dryRun
			actions := []string{}
			_, err := stewardRestartPersistentWorker(
				context, context.Path(".local/bin/t3-steward"), &actions)
			if test.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), test.errorText) {
					t.Fatalf("want error containing %q, got %v", test.errorText, err)
				}
			} else if err != nil {
				t.Fatalf("restart: %v", err)
			}
			if runner.ran("restart", stewardWorkerUnit) != test.wantRestart {
				t.Fatalf("restart performed = %v, want %v",
					!test.wantRestart, test.wantRestart)
			}
			if test.action != "" && !adapterTestMatches(actions, test.action) {
				t.Fatalf("actions %v do not report %q", actions, test.action)
			}
		})
	}
}

// stewardTestRunningWorker makes the worker process execute running, as if it
// had been started from the installed binary's path.
func stewardTestRunningWorker(t *testing.T, context *Context, running string) {
	t.Helper()
	saved := stewardWorkerExecutable
	stewardWorkerExecutable = func(pid int) (string, os.FileInfo, error) {
		if pid != 4242 {
			t.Errorf("asked for the executable of process %d", pid)
		}
		info, err := os.Stat(running)
		if err != nil {
			return "", nil, err
		}
		path, err := filepath.EvalSymlinks(context.Path(".local/bin/t3-steward"))
		return path, info, err
	}
	t.Cleanup(func() { stewardWorkerExecutable = saved })
}

// A restart deferred because the worker was busy must not be forgotten: the
// next pass sees the binary already installed, so only the running worker's
// own executable says that it still serves the previous release.
func TestStewardObserveReportsAPendingWorkerRestart(t *testing.T) {
	for _, test := range []struct {
		name        string
		stale       bool
		workerPID   string
		wantPending bool
	}{
		{name: "a worker running the replaced binary", stale: true, workerPID: "4242", wantPending: true},
		{name: "a worker running the installed binary", workerPID: "4242"},
		{name: "a worker with no main process", stale: true, workerPID: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			context, _ := stewardTestPrepare(t, stewardTestHost{
				active: true, workerLoaded: true, workerPID: test.workerPID,
			})
			spec := stewardTestSpec(false)
			context.Components["t3-steward"] = spec
			binary := context.Path(".local/bin/t3-steward")
			adapterTestWrite(t, binary, stewardTestBinary)
			running := binary
			if test.stale {
				running = context.Path("previous-t3-steward")
				adapterTestWrite(t, running, "#!/bin/sh\necho previous steward\n")
			}
			stewardTestRunningWorker(t, context, running)
			observed, err := stewardObserve(context, spec)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			if observed["worker_restart_pending"] != test.wantPending {
				t.Fatalf("worker_restart_pending = %v, want %v", observed["worker_restart_pending"], test.wantPending)
			}
			drift, _ := observed["drift"].([]string)
			if test.wantPending != adapterTestMatches(drift, "worker restart pending") {
				t.Fatalf("drift = %v", drift)
			}
			if matches := observationMatches(context, "t3-steward", spec, observed); matches == test.wantPending {
				t.Fatalf("status reports converged = %v for a pending restart = %v", matches, test.wantPending)
			}
		})
	}
}

// The pass after a deferral restarts the worker once the journal owns nothing,
// and never while it owns dispatched or settle-pending attempts.
func TestStewardRetriesADeferredWorkerRestart(t *testing.T) {
	for _, test := range []struct {
		name        string
		journal     string
		dryRun      bool
		wantState   string
		wantRestart bool
		action      string
	}{
		{
			name:        "an idle journal restarts the worker",
			journal:     `{"dispatched":0,"settlePending":0}`,
			wantState:   "updated",
			wantRestart: true,
			action:      "restarted t3-steward-worker.service onto the pinned steward",
		},
		{
			name:      "dispatched attempts defer again",
			journal:   `{"dispatched":1,"settlePending":0}`,
			wantState: "converged",
			action:    "worker restart deferred: 1 dispatched attempt(s) in the journal",
		},
		{
			name:      "settle-pending attempts defer again",
			journal:   `{"dispatched":0,"settlePending":2}`,
			wantState: "converged",
			action:    "worker restart deferred: 2 settle-pending attempt(s) in the journal",
		},
		{
			name:      "a dry run describes the restart",
			journal:   `{"dispatched":0,"settlePending":0}`,
			dryRun:    true,
			wantState: "updated",
			action:    "would restart t3-steward-worker.service onto the pinned steward",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			context, runner := stewardTestPrepare(t, stewardTestHost{
				active: true, workerLoaded: true, workerPID: "4242", dispatched: test.journal,
			})
			spec := stewardTestSpec(false)
			context.Components["t3-steward"] = spec
			binary := context.Path(".local/bin/t3-steward")
			adapterTestWrite(t, binary, stewardTestBinary)
			previous := context.Path("previous-t3-steward")
			adapterTestWrite(t, previous, "#!/bin/sh\necho previous steward\n")
			stewardTestRunningWorker(t, context, previous)
			stewardTestServe(t, nil, "")
			before, err := stewardObserve(context, spec)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			context.DryRun = test.dryRun
			actions := []string{}
			state, err := stewardApply(context, spec, before, &actions)
			if err != nil || state != test.wantState {
				t.Fatalf("state = %q, error = %v, want %q (actions %v)", state, err, test.wantState, actions)
			}
			if runner.ran("restart", stewardWorkerUnit) != test.wantRestart {
				t.Fatalf("worker restart performed = %v, want %v: %v",
					!test.wantRestart, test.wantRestart, runner.calls)
			}
			if runner.ran("restart", "t3-steward.service") {
				t.Fatalf("a converged steward service was restarted: %v", runner.calls)
			}
			if !adapterTestMatches(actions, test.action) {
				t.Fatalf("actions %v do not report %q", actions, test.action)
			}
		})
	}
}

func TestStewardVerifyPairBinary(t *testing.T) {
	cases := []struct {
		name       string
		report     string
		t3         map[string]any
		errorText  string
		wantBounds [2]string
	}{
		{
			name:       "a binary that reports the pinned version and a range passes",
			report:     stewardTestReport,
			wantBounds: [2]string{"0.0.38", "0.0.38"},
		},
		{
			name:      "a binary reporting another version is refused",
			report:    "t3-steward 0.11.0-rc.2 (tested with T3 0.0.38..0.0.38)",
			errorText: "does not prove the pinned version and T3 compatibility",
		},
		{
			name:      "a binary without a tested range is refused",
			report:    "t3-steward " + stewardTestVersion,
			errorText: "does not prove the pinned version and T3 compatibility",
		},
		{
			name:   "a range that does not match the frozen compatibility is refused",
			report: stewardTestReport,
			t3: map[string]any{
				"version":       "0.0.38",
				"compatibility": map[string]any{"min": "0.0.37", "max": "0.0.39"},
			},
			errorText: "does not prove the frozen T3 compatibility range",
		},
		{
			name:   "a frozen range that contains the release T3 passes",
			report: stewardTestReport,
			t3: map[string]any{
				"version":       "0.0.38",
				"compatibility": map[string]any{"min": "0.0.38", "max": "0.0.38"},
			},
			wantBounds: [2]string{"0.0.38", "0.0.38"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context, _ := stewardTestPrepare(t, stewardTestHost{
				active: true, versionReport: test.report,
			})
			context.Components["t3-steward"] = stewardTestSpec(false)
			if test.t3 != nil {
				context.Components["t3"] = test.t3
			}
			low, high, err := stewardVerifyPairBinary(
				context, context.Path(".local/bin/t3-steward"))
			if test.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), test.errorText) {
					t.Fatalf("want error containing %q, got %v", test.errorText, err)
				}
				return
			}
			if err != nil || low != test.wantBounds[0] || high != test.wantBounds[1] {
				t.Fatalf("bounds = (%q, %q), error = %v", low, high, err)
			}
		})
	}
}

func TestStewardVersionOrdering(t *testing.T) {
	numbers, final, prerelease := stewardVersionTuple("0.11.0-rc.1")
	if len(numbers) != 3 || numbers[1] != 11 || final != 0 || prerelease != "rc.1" {
		t.Fatalf("tuple = %v, %d, %q", numbers, final, prerelease)
	}
	if _, final, _ := stewardVersionTuple("0.11.0"); final != 1 {
		t.Fatal("a final release must sort above its prereleases")
	}
	if _, err := stewardNumericVersion("0.11.0-rc.1"); err == nil {
		t.Fatal("a prerelease is not a stable numeric T3 version")
	}
}

// A pair-range refusal has to say what was observed and what to do about it.
// The fixed sentence it replaced hid a deleted T3 binary behind "outside
// candidate steward range" for a whole day.
func TestStewardRunningT3RangeRefusalsSayWhatWasObserved(t *testing.T) {
	cases := []struct {
		name     string
		observed map[string]any
		want     string
	}{
		{
			name:     "a served version outside the range is named with the range",
			observed: map[string]any{"check_ok": true, "served": "0.0.40"},
			want: "running T3 0.0.40 is outside the candidate steward's T3 range " +
				"0.0.38..0.0.39; no safe pair transition",
		},
		{
			name: "a failed check quotes the steward's first line",
			observed: map[string]any{
				"check_ok":    false,
				"check_error": "dial tcp 127.0.0.1:7391: connection refused",
			},
			want: "running T3 version could not be observed (t3-steward check failed: " +
				"dial tcp 127.0.0.1:7391: connection refused); no safe pair transition",
		},
		{
			name:     "a check that answered without a version says so",
			observed: map[string]any{"check_ok": true},
			want: "running T3 version could not be observed (t3-steward check reported " +
				"no T3 server version); no safe pair transition",
		},
		{
			name: "a failed check with the program missing on disk names the program",
			observed: map[string]any{
				"check_ok":              false,
				"check_error":           "exec: no such file or directory",
				"installed_path":        "/home/igor/.local/share/mise/installs/node/26.7.0/bin/node",
				"installed_path_exists": false,
			},
			want: "running T3 binary missing on disk: /home/igor/.local/share/mise/installs/node/26.7.0/bin/node " +
				"(installed 0.0.38 from the manifest); restart T3 after reinstalling, then retry",
		},
		{
			name: "a check without a version and the program missing names the program too",
			observed: map[string]any{
				"check_ok": true, "installed_path": "/x/node", "installed_path_exists": false,
			},
			want: "running T3 binary missing on disk: /x/node (installed 0.0.38 from the manifest)",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := stewardRunningT3InRange(test.observed, "0.0.38", "0.0.39", "0.0.38")
			if err == nil || !strings.HasPrefix(err.Error(), test.want) {
				t.Fatalf("want a refusal starting with %q, got %v", test.want, err)
			}
			if !strings.HasSuffix(err.Error(), "then retry") {
				t.Fatalf("the refusal carries no remediation: %v", err)
			}
		})
	}
	inRange := map[string]any{"check_ok": true, "served": "0.0.38", "installed_path_exists": true}
	if err := stewardRunningT3InRange(inRange, "0.0.38", "0.0.39", "0.0.38"); err != nil {
		t.Fatalf("a running T3 inside the range was refused: %v", err)
	}
	// A server that still answers from a deleted program is inside the range
	// and proceeds; the missing program is a warning, not a new precondition.
	stillServing := map[string]any{
		"check_ok": true, "served": "0.0.38",
		"installed_path": "/x/node", "installed_path_exists": false,
	}
	if err := stewardRunningT3InRange(stillServing, "0.0.38", "0.0.39", "0.0.38"); err != nil {
		t.Fatalf("an answering in-range T3 was refused for its missing program: %v", err)
	}
	outOfRange := map[string]any{
		"check_ok": true, "served": "0.0.40",
		"installed_path": "/x/node", "installed_path_exists": false,
	}
	err := stewardRunningT3InRange(outOfRange, "0.0.38", "0.0.39", "0.0.38")
	if err == nil || !strings.HasPrefix(err.Error(), "running T3 0.0.40 is outside") {
		t.Fatalf("an answering out-of-range T3 got %v", err)
	}
}
