package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// The service mock retains activity across commands: deleting its executable
// cannot stop it. All commands and files belong to the temporary fixture.
func TestFreshStewardServiceRestoration(t *testing.T) {
	for _, tc := range []struct {
		name                                                                                                              string
		noUnit, priorActive, priorUnknown, restartFails, stopFails, staysActive, verifyUnknown, preEffect, installRefusal bool
		want                                                                                                              string
	}{
		{name: "restores inactive", want: "failed"},
		{name: "stop fails", stopFails: true, want: "rollback-failed"},
		{name: "stop leaves active", staysActive: true, want: "rollback-failed"},
		{name: "stop cannot be verified", verifyUnknown: true, want: "rollback-failed"},
		{name: "restart fails after activation", restartFails: true, want: "failed"},
		{name: "previous active cannot be restored without binary", priorActive: true, want: "rollback-failed"},
		{name: "unknown prior state refuses before effects", priorUnknown: true, want: "failed"},
		{name: "no unit cleanup", noUnit: true, want: "failed"},
		{name: "staging refusal", preEffect: true, want: "failed"},
		{name: "install staging path refusal", installRefusal: true, want: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, runner := stewardTestPrepare(t, stewardTestHost{})
			spec := stewardTestSpec(false)
			c.Components["t3-steward"] = spec
			archive := stewardTestArchive(t, []byte(stewardTestBinary), nil, false)
			stewardTestServe(t, archive, stewardTestDigest(archive))
			binary := c.Path(".local/bin/t3-steward")
			active := tc.priorActive
			failInstalledProbe := true
			if tc.installRefusal {
				adapterTestWrite(t, binary+".upkeeper-tmp", "unknown staging")
			}
			restarts, stops, queries := 0, 0, 0
			var events []string
			original := runner.handle
			runner.handle = func(cmd procx.Command) procx.Result {
				switch {
				case adapterTestMatches(cmd.Argv, "cat", "t3-steward.service") && tc.noUnit:
					return procx.Result{Code: 1, Stderr: "No files found for t3-steward.service"}
				case adapterTestMatches(cmd.Argv, "is-active", "t3-steward.service"):
					queries++
					events = append(events, "activity")
					if tc.priorUnknown || (tc.verifyUnknown && stops > 0) {
						return procx.Result{Code: 1, Stderr: "bus unavailable"}
					}
					if active {
						return procx.Result{Stdout: "active"}
					}
					return procx.Result{Code: 3, Stdout: "inactive"}
				case adapterTestMatches(cmd.Argv, "restart", "t3-steward.service"):
					restarts++
					events = append(events, "restart")
					active = true
					if tc.restartFails {
						return procx.Result{Code: 1, Stderr: "restart failed after activation"}
					}
					return procx.Result{}
				case adapterTestMatches(cmd.Argv, "stop", "t3-steward.service"):
					stops++
					events = append(events, "stop")
					if _, err := os.Stat(binary); err != nil {
						t.Errorf("binary removed before stopping: %v", err)
					}
					if tc.stopFails {
						return procx.Result{Code: 1, Stderr: "stop denied"}
					}
					if !tc.staysActive {
						active = false
					}
					return procx.Result{}
				case len(cmd.Argv) > 1 && cmd.Argv[1] == "version" && ((cmd.Argv[0] == binary && failInstalledProbe) || tc.preEffect):
					return procx.Result{Code: 1, Stderr: "injected binary probe failure"}
				}
				return original(cmd)
			}
			_, err := stewardApply(c, spec, map[string]any{"service_active": false}, &[]string{})
			if err == nil {
				t.Fatal("expected verification failure")
			}
			state, _ := classify(err)
			if state != tc.want {
				t.Errorf("state=%s want=%s error=%v", state, tc.want, err)
			}
			if _, e := os.Stat(binary); !os.IsNotExist(e) {
				t.Errorf("binary remains: %v", e)
			}
			if tc.noUnit || tc.preEffect || tc.priorUnknown || tc.installRefusal {
				if restarts != 0 || stops != 0 {
					t.Errorf("pre-activation path touched service: %v", events)
				}
			} else {
				if restarts != 1 || stops != 1 || queries < 2 || events[0] != "activity" {
					t.Errorf("restoration ordering not proven: %v", events)
				}
				if tc.want == "failed" && active {
					t.Error("ordinary failure left service active")
				}
			}
			if tc.stopFails && !strings.Contains(err.Error(), "stop denied") {
				t.Errorf("lost stop command error: %v", err)
			}
			if tc.name == "restores inactive" {
				failInstalledProbe = false
				outcome, retryError := stewardApply(c, spec, map[string]any{"service_active": false}, &[]string{})
				if retryError != nil || outcome != "updated" || !active {
					t.Fatalf("rerun after safe cleanup: outcome=%s active=%v error=%v", outcome, active, retryError)
				}
			}
			run, dispatch := newPartialRun(t, map[string][]string{}, "b", "c")
			run.outcomes["b"] = func() (string, error) { return "", err }
			result := Execute(dispatch)
			wantCalls, wantLater := 3, "updated"
			if tc.want == "rollback-failed" {
				wantCalls, wantLater = 0, "skipped"
			}
			if run.calls["c"] != wantCalls || resultEntry(t, result, "c")["state"] != wantLater {
				t.Errorf("unsafe dispatch: calls=%v result=%v", run.calls, result)
			}
			if result["result"] != tc.want {
				t.Errorf("host result=%v want=%s", result["result"], tc.want)
			}
			t.Logf("state=%s active=%v events=%v error=%v", state, active, events, err)
		})
	}
}

func TestFreshStewardCleanupFailuresAbort(t *testing.T) {
	for _, path := range []string{"binary", "hook"} {
		t.Run(path, func(t *testing.T) {
			c := adapterTestContext(t, &adapterTestRunner{})
			binary, hook := c.Path("binary"), c.Path("hook")
			state := stewardRestoration{binary: binary, hookPath: hook, hookInstalled: path == "hook"}
			target := binary
			if path == "hook" {
				target = hook
			}
			adapterTestWrite(t, target+"/child", "retained")
			err := stewardRestorePrevious(c, map[string]any{}, state)
			abortsTheHost(t, err)
		})
	}
}
