package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// huyangCycleUnit is the unit every host had installed on 2026-10-02, byte
// for byte. It is wanted by default.target and also ordered after it, so any
// unit ordered after huyang.service closes an ordering cycle with
// default.target, and systemd deletes that unit's start job at boot:
//
//	Job t3-steward-worker.service/start deleted to break ordering cycle
const huyangCycleUnit = "[Unit]\nDescription=Huyang semantic workspace service\n" +
	"After=default.target\n\n[Service]\nType=simple\n" +
	"WorkingDirectory=%h/.local/share/huyang\n" +
	"ExecStart=%h/.local/share/huyang/bin/huyang serve\n" +
	"Restart=on-failure\nRestartSec=2\n\n[Install]\nWantedBy=default.target\n"

// huyangReconciledUnit is huyangCycleUnit without the ordering line, and is
// what a fresh bootstrap now writes.
const huyangReconciledUnit = "[Unit]\nDescription=Huyang semantic workspace service\n" +
	"\n[Service]\nType=simple\n" +
	"WorkingDirectory=%h/.local/share/huyang\n" +
	"ExecStart=%h/.local/share/huyang/bin/huyang serve\n" +
	"Restart=on-failure\nRestartSec=2\n\n[Install]\nWantedBy=default.target\n"

const huyangTestUnitDir = ".config/systemd/user"

// huyangConvergedWorld is a host whose checkout, service and socket already
// match the release, so the only thing an apply could change is the unit.
var huyangConvergedWorld = huyangTestWorld{
	head: huyangPinned, active: true, socket: true, containsPin: true,
}

func huyangTestApply(t *testing.T, context *Context) (string, []string, error) {
	t.Helper()
	spec := context.Spec("huyang")
	before, err := huyangObserve(context, spec)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	actions := []string{}
	state, err := huyangApply(context, spec, before, &actions)
	return state, actions, err
}

func huyangTestRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestHuyangUnitTemplateHasNoOrderingCycle(t *testing.T) {
	unit := fmt.Sprintf(huyangUnitTemplate, ".local/share/huyang")
	if strings.Contains(unit, "After=default.target") {
		t.Fatalf("the seeded unit is wanted by and ordered after default.target:\n%s", unit)
	}
	if unit != huyangReconciledUnit {
		t.Fatalf("the seeded unit differs from the reconciled unit:\n%s", unit)
	}
}

func TestHuyangApplyRewritesAnInstalledUnitWithAnOrderingCycle(t *testing.T) {
	context, runner := huyangTestPrepare(t, huyangConvergedWorld)
	huyangTestPatch(t, huyangTestProbe(true, 19))
	unit := context.Path(huyangTestUnitDir, "huyang.service")
	dropIn := context.Path(huyangTestUnitDir, "huyang.service.d/20-resource-limits.conf")
	adapterTestWrite(t, unit, huyangCycleUnit)
	adapterTestWrite(t, dropIn, "[Service]\nMemoryMax=12G\n")

	state, actions, err := huyangTestApply(t, context)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v (actions %v)", state, err, actions)
	}
	if got := huyangTestRead(t, unit); got != huyangReconciledUnit {
		t.Fatalf("unit after reconcile:\n%s", got)
	}
	if got := huyangTestRead(t, dropIn); got != "[Service]\nMemoryMax=12G\n" {
		t.Fatalf("the drop-in was changed:\n%s", got)
	}
	if !runner.ran("systemctl", "--user", "daemon-reload") {
		t.Fatalf("the rewritten unit was not reloaded: %v", runner.calls)
	}
	if runner.ran("systemctl", "--user", "restart") {
		t.Fatalf("an ordering-only change restarted Huyang: %v", runner.calls)
	}
	if !adapterTestMatches(actions, "removed After=default.target from huyang.service") {
		t.Fatalf("actions %v do not report the rewrite", actions)
	}

	// A second run finds nothing to do and touches nothing.
	runner.calls = nil
	state, actions, err = huyangTestApply(t, context)
	if err != nil || state != "converged" {
		t.Fatalf("second apply: state = %q, error = %v (actions %v)", state, err, actions)
	}
	if runner.ran("daemon-reload") {
		t.Fatalf("an unchanged unit was reloaded: %v", runner.calls)
	}
}

func TestHuyangUnitReconcilePreservesUnrelatedSettings(t *testing.T) {
	context, _ := huyangTestPrepare(t, huyangConvergedWorld)
	huyangTestPatch(t, huyangTestProbe(true, 19))
	unit := context.Path(huyangTestUnitDir, "huyang.service")
	custom := "# local edit\n[Unit]\nDescription=Huyang\nAfter=network-online.target default.target\n" +
		"Wants=network-online.target\n\n[Service]\nExecStart=/opt/huyang serve\n" +
		"Environment=HUYANG_DEBUG=1\nAfter=default.target\n\n[Install]\nWantedBy=default.target\n"
	adapterTestWrite(t, unit, custom)
	if err := os.Chmod(unit, 0o640); err != nil {
		t.Fatal(err)
	}

	state, actions, err := huyangTestApply(t, context)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v (actions %v)", state, err, actions)
	}
	// Only the [Unit] ordering on default.target goes; the line in [Service]
	// is not an ordering directive and is left for systemd to ignore.
	want := strings.Replace(custom,
		"After=network-online.target default.target\n", "After=network-online.target\n", 1)
	if got := huyangTestRead(t, unit); got != want {
		t.Fatalf("unit after reconcile:\n%s\nwant:\n%s", got, want)
	}
	info, err := os.Stat(unit)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("the unit's mode was not preserved: %v %v", info, err)
	}
}

func TestHuyangUnitReconcileDryRunWritesNothing(t *testing.T) {
	context, runner := huyangTestPrepare(t, huyangConvergedWorld)
	huyangTestPatch(t, huyangTestProbe(true, 19))
	context.DryRun = true
	unit := context.Path(huyangTestUnitDir, "huyang.service")
	adapterTestWrite(t, unit, huyangCycleUnit)

	state, actions, err := huyangTestApply(t, context)
	if err != nil || state != "updated" {
		t.Fatalf("state = %q, error = %v (actions %v)", state, err, actions)
	}
	if got := huyangTestRead(t, unit); got != huyangCycleUnit {
		t.Fatalf("a preview rewrote the unit:\n%s", got)
	}
	if runner.ran("daemon-reload") {
		t.Fatalf("a preview reloaded systemd: %v", runner.calls)
	}
	if !adapterTestMatches(actions, "would remove After=default.target from huyang.service") {
		t.Fatalf("actions %v do not describe the preview", actions)
	}
}

func TestHuyangApplyChecksEveryUserUnitForAnOrderingCycle(t *testing.T) {
	otherCycle := strings.Replace(huyangCycleUnit, "Huyang", "Other", 1)
	otherClean := strings.Replace(huyangReconciledUnit, "Huyang", "Other", 1)
	cases := []struct {
		name  string
		files map[string]string
		// links are symlinks to create, name to target, under the unit dir.
		links map[string]string
		// warned names another unit the run must warn about without failing.
		warned string
		// failing is set when Huyang's own unit keeps a cycle.
		failing bool
	}{
		{
			name:   "another unit wanted by and ordered after default.target",
			files:  map[string]string{"bellerophon.service": otherCycle},
			warned: "bellerophon.service",
		},
		{
			name: "a drop-in that adds the ordering",
			files: map[string]string{
				"other.service":                 otherClean,
				"other.service.d/ordering.conf": "[Unit]\nAfter=default.target\n",
			},
			warned: "other.service",
		},
		{
			// An empty After= does not reset a dependency list (systemd 252).
			name: "a drop-in with an empty After= keeps the ordering",
			files: map[string]string{
				"other.service":              otherCycle,
				"other.service.d/reset.conf": "[Unit]\nAfter=\nAfter=basic.target\n",
			},
			warned: "other.service",
		},
		{
			name: "a drop-in that adds WantedBy=default.target",
			files: map[string]string{
				"other.service":                "[Unit]\nAfter=default.target\n\n[Service]\nExecStart=/bin/true\n",
				"other.service.d/install.conf": "[Install]\nWantedBy=default.target\n",
			},
			warned: "other.service",
		},
		{
			name: "a unit enabled into default.target.wants",
			files: map[string]string{
				"other.service": "[Unit]\nAfter=default.target\n\n[Service]\nExecStart=/bin/true\n",
			},
			links:  map[string]string{"default.target.wants/other.service": "../other.service"},
			warned: "other.service",
		},
		{
			name: "an After= continued onto the next line",
			files: map[string]string{
				"other.service": "[Unit]\nAfter=network.target \\\n  default.target\n\n" +
					"[Install]\nWantedBy=default.target\n",
			},
			warned: "other.service",
		},
		{
			name: "ordering after default.target without being wanted by it",
			files: map[string]string{
				"late.service": "[Unit]\nAfter=default.target\n\n[Install]\nWantedBy=graphical-session.target\n",
			},
		},
		{
			name:  "a backup file is not a unit",
			files: map[string]string{"t3code.service.bak.lan-20260905": huyangCycleUnit},
		},
		{
			name: "a drop-in that orders Huyang itself after default.target",
			files: map[string]string{
				"huyang.service.d/ordering.conf": "[Unit]\nAfter=default.target\n",
			},
			failing: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			context, _ := huyangTestPrepare(t, huyangConvergedWorld)
			huyangTestPatch(t, huyangTestProbe(true, 19))
			adapterTestWrite(t, context.Path(huyangTestUnitDir, "huyang.service"), huyangReconciledUnit)
			for name, content := range test.files {
				adapterTestWrite(t, context.Path(huyangTestUnitDir, name), content)
			}
			for name, target := range test.links {
				link := context.Path(huyangTestUnitDir, name)
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			state, actions, err := huyangTestApply(t, context)
			if test.failing {
				if err == nil {
					t.Fatalf("a cycle in huyang.service passed as %q (actions %v)", state, actions)
				}
				if failed, _ := classify(err); failed != "failed" ||
					!strings.Contains(err.Error(), "huyang.service") ||
					!strings.Contains(err.Error(), "ordering cycle") {
					t.Fatalf("failure does not name huyang.service as an ordering cycle: %v", err)
				}
				return
			}
			if err != nil || state != "converged" {
				t.Fatalf("state = %q, error = %v (actions %v)", state, err, actions)
			}
			warnings := []string{}
			for _, action := range actions {
				if strings.HasPrefix(action, "warning:") {
					warnings = append(warnings, action)
				}
			}
			if test.warned == "" {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings %v", warnings)
				}
				return
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "user unit "+test.warned) ||
				!strings.Contains(warnings[0], "ordering cycle") ||
				!strings.Contains(warnings[0], "systemctl --user daemon-reload") ||
				strings.Contains(warnings[0], "huyang.service") {
				t.Fatalf("warnings %v do not name %s, the cycle and the fix", warnings, test.warned)
			}
		})
	}
}

func TestHuyangUnitReconcileRetriesAFailedReload(t *testing.T) {
	context, runner := huyangTestPrepare(t, huyangConvergedWorld)
	huyangTestPatch(t, huyangTestProbe(true, 19))
	unit := context.Path(huyangTestUnitDir, "huyang.service")
	adapterTestWrite(t, unit, huyangCycleUnit)
	passThrough := runner.handle
	reloads := 0
	runner.handle = func(command procx.Command) procx.Result {
		if adapterTestMatches(command.Argv, "systemctl", "daemon-reload") {
			reloads++
			if reloads == 1 {
				return procx.Result{Code: 1, Stderr: "transport endpoint is not connected"}
			}
		}
		return passThrough(command)
	}

	if _, actions, err := huyangTestApply(t, context); err == nil {
		t.Fatalf("a failed reload passed (actions %v)", actions)
	}
	if got := huyangTestRead(t, unit); got != huyangReconciledUnit {
		t.Fatalf("unit after the first run:\n%s", got)
	}

	// The bytes on disk are already right; the retry must still reload.
	state, actions, err := huyangTestApply(t, context)
	if err != nil || state != "updated" {
		t.Fatalf("retry: state = %q, error = %v (actions %v)", state, err, actions)
	}
	if reloads != 2 {
		t.Fatalf("the retry ran %d reloads in total, want 2", reloads)
	}
	if !adapterTestMatches(actions, "reloaded systemd") {
		t.Fatalf("actions %v do not report the reload", actions)
	}

	// Once the reload has gone through, nothing is pending any more.
	state, _, err = huyangTestApply(t, context)
	if err != nil || state != "converged" || reloads != 2 {
		t.Fatalf("third run: state = %q, error = %v, reloads = %d", state, err, reloads)
	}
}

// huyangContinuedCommentUnit continues Description= across a comment line.
// systemd skips the comment inside the continuation, so the physical line
// After=default.target is part of the description, not an ordering.
const huyangContinuedCommentUnit = "[Unit]\nDescription=keep \\\n# comment\nAfter=default.target\n" +
	"[Install]\nWantedBy=default.target\n"

func TestHuyangUnitReconcileNeverRewritesInsideAContinuedValue(t *testing.T) {
	if out, changed := unitDropOrderingAfter([]byte(huyangContinuedCommentUnit), "default.target"); changed ||
		string(out) != huyangContinuedCommentUnit {
		t.Fatalf("rewrite changed = %v, output:\n%s", changed, out)
	}
	context, runner := huyangTestPrepare(t, huyangConvergedWorld)
	huyangTestPatch(t, huyangTestProbe(true, 19))
	unit := context.Path(huyangTestUnitDir, "huyang.service")
	adapterTestWrite(t, unit, huyangContinuedCommentUnit)
	state, actions, err := huyangTestApply(t, context)
	if err != nil || state != "converged" {
		t.Fatalf("state = %q, error = %v (actions %v)", state, err, actions)
	}
	if got := huyangTestRead(t, unit); got != huyangContinuedCommentUnit {
		t.Fatalf("unit bytes changed:\n%s", got)
	}
	if runner.ran("daemon-reload") {
		t.Fatalf("an unchanged unit was reloaded: %v", runner.calls)
	}
}

func huyangTestStatusMatches(t *testing.T, context *Context) (bool, map[string]any) {
	t.Helper()
	spec := context.Spec("huyang")
	observed, err := huyangObserve(context, spec)
	if err != nil {
		t.Fatal(err)
	}
	return observationMatches(context, "huyang", spec, observed), observed
}

func TestHuyangStatusTreatsAPendingReloadAsDrift(t *testing.T) {
	context, runner := huyangTestPrepare(t, huyangConvergedWorld)
	huyangTestPatch(t, huyangTestProbe(true, 19))
	adapterTestWrite(t, context.Path(".local/share/huyang/bin/huyang"), "binary")
	adapterTestWrite(t, context.Path(huyangTestUnitDir, "huyang.service"), huyangCycleUnit)
	passThrough := runner.handle
	failReload := true
	runner.handle = func(command procx.Command) procx.Result {
		if failReload && adapterTestMatches(command.Argv, "systemctl", "daemon-reload") {
			return procx.Result{Code: 1, Stderr: "transport endpoint is not connected"}
		}
		return passThrough(command)
	}
	if _, _, err := huyangTestApply(t, context); err == nil {
		t.Fatal("a failed reload passed")
	}
	// The bytes on disk are reconciled, but systemd still runs the old graph.
	if matches, observed := huyangTestStatusMatches(t, context); matches {
		t.Fatalf("status is clean right after a failed reload (observed %v)", observed)
	}
	failReload = false
	if _, actions, err := huyangTestApply(t, context); err != nil {
		t.Fatalf("retry: %v (actions %v)", err, actions)
	}
	if matches, observed := huyangTestStatusMatches(t, context); !matches {
		t.Fatalf("status drifts after the successful retry (observed %v)", observed)
	}
}

func TestHuyangStatusTreatsAnUnreadableOrderingObservationAsDrift(t *testing.T) {
	context, _ := huyangTestPrepare(t, huyangConvergedWorld)
	adapterTestWrite(t, context.Path(".local/share/huyang/bin/huyang"), "binary")
	// A directory where the unit file belongs cannot be read as a unit, so
	// the ordering of huyang.service is unknown, which is not clean.
	if err := os.MkdirAll(context.Path(huyangTestUnitDir, "huyang.service"), 0o755); err != nil {
		t.Fatal(err)
	}
	if matches, observed := huyangTestStatusMatches(t, context); matches {
		t.Fatalf("an unknown ordering observation counts as clean (observed %v)", observed)
	}
}

func TestHuyangUnitIsReconciledWhenTheCheckoutIsAbsent(t *testing.T) {
	runner := &adapterTestRunner{}
	context := adapterTestContext(t, runner)
	context.Components["huyang"] = huyangTestSpec()
	huyangTestPatch(t, huyangTestProbe(true, 19))
	unit := context.Path(huyangTestUnitDir, "huyang.service")
	adapterTestWrite(t, unit, huyangCycleUnit)

	_, actions, err := huyangTestApply(t, context)
	if err == nil || !strings.Contains(err.Error(), "a user unit exists; inspect it first") {
		t.Fatalf("want the bootstrap refusal, got %v (actions %v)", err, actions)
	}
	if got := huyangTestRead(t, unit); got != huyangReconciledUnit {
		t.Fatalf("unit without a checkout was not reconciled:\n%s", got)
	}
	if !runner.ran("systemctl", "--user", "daemon-reload") {
		t.Fatalf("the rewritten unit was not reloaded: %v", runner.calls)
	}
}

func TestHuyangObserveReportsAnInstalledOrderingCycleAsDrift(t *testing.T) {
	context, _ := huyangTestPrepare(t, huyangConvergedWorld)
	adapterTestWrite(t, context.Path(".local/share/huyang/bin/huyang"), "binary")
	unit := context.Path(huyangTestUnitDir, "huyang.service")
	other := context.Path(huyangTestUnitDir, "other.service")
	spec := context.Spec("huyang")
	for _, test := range []struct {
		huyang, other string
		matches       bool
	}{
		{huyangCycleUnit, huyangReconciledUnit, false},
		{huyangReconciledUnit, huyangCycleUnit, false},
		{huyangReconciledUnit, huyangReconciledUnit, true},
	} {
		adapterTestWrite(t, unit, test.huyang)
		adapterTestWrite(t, other, test.other)
		observed, err := huyangObserve(context, spec)
		if err != nil {
			t.Fatal(err)
		}
		observed["dirty"] = false
		if got := observationMatches(context, "huyang", spec, observed); got != test.matches {
			t.Fatalf("status match = %v for huyang:\n%s\nother:\n%s(observed %v)",
				got, test.huyang, test.other, observed)
		}
	}
}
