package agent

import (
	"fmt"
	"os"
	"strings"
	"testing"
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

func TestHuyangApplyFailsOnAnyUserUnitWithAnOrderingCycle(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		// failing names the unit the failure has to name; empty means pass.
		failing string
	}{
		{
			name: "another unit wanted by and ordered after default.target",
			files: map[string]string{
				"bellerophon.service": strings.Replace(huyangCycleUnit, "Huyang", "Other", 1),
			},
			failing: "bellerophon.service",
		},
		{
			name: "a drop-in that adds the ordering",
			files: map[string]string{
				"other.service":                 huyangReconciledUnit,
				"other.service.d/ordering.conf": "[Unit]\nAfter=default.target\n",
			},
			failing: "other.service",
		},
		{
			name: "a drop-in that resets the ordering",
			files: map[string]string{
				"other.service":              strings.Replace(huyangCycleUnit, "Huyang", "Other", 1),
				"other.service.d/reset.conf": "[Unit]\nAfter=\n",
			},
		},
		{
			name: "ordering after default.target without being wanted by it",
			files: map[string]string{
				"late.service": "[Unit]\nAfter=default.target\n\n[Install]\nWantedBy=graphical-session.target\n",
			},
		},
		{
			name: "a backup file is not a unit",
			files: map[string]string{
				"t3code.service.bak.lan-20260905": huyangCycleUnit,
			},
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
			state, actions, err := huyangTestApply(t, context)
			if test.failing == "" {
				if err != nil || state != "converged" {
					t.Fatalf("state = %q, error = %v (actions %v)", state, err, actions)
				}
				return
			}
			if err == nil {
				t.Fatalf("an ordering cycle in %s passed as %q", test.failing, state)
			}
			if failed, _ := classify(err); failed != "failed" ||
				!strings.Contains(err.Error(), test.failing) ||
				!strings.Contains(err.Error(), "ordering cycle") {
				t.Fatalf("failure does not name %s as an ordering cycle: %v", test.failing, err)
			}
			if strings.Contains(err.Error(), "huyang.service") {
				t.Fatalf("failure names the reconciled huyang.service: %v", err)
			}
		})
	}
}

func TestHuyangObserveReportsAnInstalledOrderingCycleAsDrift(t *testing.T) {
	context, _ := huyangTestPrepare(t, huyangConvergedWorld)
	adapterTestWrite(t, context.Path(".local/share/huyang/bin/huyang"), "binary")
	unit := context.Path(huyangTestUnitDir, "huyang.service")
	spec := context.Spec("huyang")
	for _, test := range []struct {
		content string
		matches bool
	}{{huyangCycleUnit, false}, {huyangReconciledUnit, true}} {
		adapterTestWrite(t, unit, test.content)
		observed, err := huyangObserve(context, spec)
		if err != nil {
			t.Fatal(err)
		}
		observed["dirty"] = false
		if got := observationMatches(context, "huyang", spec, observed); got != test.matches {
			t.Fatalf("status match = %v for unit:\n%s(observed %v)", got, test.content, observed)
		}
	}
}
