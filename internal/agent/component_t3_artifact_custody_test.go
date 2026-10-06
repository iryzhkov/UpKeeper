package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

const t3CustodySecret = "hunter2custodysecret"

// t3CustodyHost drives the controlled T3 host fixture through the failures the
// custody and reconciliation contract names, and captures every journal write.
type t3CustodyHost struct {
	c        *Context
	spec     map[string]any
	prefix   string
	restarts int
	sleeps   int
	// failChecks fails that many health checks after the first restart;
	// brokenAfter fails every check once restarts reaches it (0 never).
	failChecks, brokenAfter int
	nativeGone              bool
	execStart               string
	dependents              map[string]string
	propagate, unknownDep   bool
	failJournal             string
	journals                map[string][]string
}

type t3CaptureFile struct {
	*os.File
	phase string
	h     *t3CustodyHost
}

func (f *t3CaptureFile) Write(b []byte) (int, error) {
	f.h.journals[f.phase] = append(f.h.journals[f.phase], string(b))
	return f.File.Write(b)
}

func newT3CustodyHost(t *testing.T) *t3CustodyHost {
	t.Helper()
	c, spec, runner, prefix := t3ArtifactHost(t, false, false, 0, 0)
	h := &t3CustodyHost{c: c, spec: spec, prefix: prefix, dependents: map[string]string{}, journals: map[string][]string{}}
	c.Sleep = func(time.Duration) { h.sleeps++ }
	original := runner.handle
	runner.handle = func(cmd procx.Command) procx.Result {
		argv := cmd.Argv
		switch {
		case adapterTestMatches(argv, "ConsistsOf"):
			units := []string{}
			for unit := range h.dependents {
				units = append(units, unit)
			}
			sort.Strings(units)
			return procx.Result{Stdout: strings.Join(units, " ")}
		case adapterTestMatches(argv, "InvocationID"):
			if h.unknownDep {
				return procx.Result{}
			}
			return procx.Result{Stdout: h.dependents[argv[3]]}
		case adapterTestMatches(argv, "ExecStart") && h.execStart != "":
			return procx.Result{Stdout: h.execStart}
		case argv[len(argv)-1] == "check":
			broken := h.brokenAfter > 0 && h.restarts >= h.brokenAfter
			if !broken && h.restarts == 1 && h.failChecks > 0 {
				h.failChecks--
				broken = true
			}
			if broken {
				return procx.Result{Code: 1, Stdout: "error: token=" + t3CustodySecret + " not ready"}
			}
		}
		result := original(cmd)
		if adapterTestMatches(argv, "restart") {
			h.restarts++
			if h.propagate {
				for unit, id := range h.dependents {
					h.dependents[unit] = id + "-next"
				}
			}
			if h.restarts == 1 && h.nativeGone {
				// The served native descendant exits right after the restart.
				if e := os.RemoveAll(filepath.Join(t3ProcRoot, "124")); e != nil {
					t.Fatal(e)
				}
			}
		}
		return result
	}
	c.t3JournalFiles = &t3JournalFiles{create: func(dir, pattern string) (t3JournalFile, error) {
		f, e := os.CreateTemp(dir, pattern)
		phase := strings.TrimSuffix(strings.TrimPrefix(pattern, ".journal-"), "-*")
		if phase == h.failJournal {
			return &t3RepairJournalFile{f, "sync"}, e
		}
		return &t3CaptureFile{f, phase, h}, e
	}}
	return h
}

func (h *t3CustodyHost) apply(t *testing.T) (string, error) {
	t.Helper()
	before, e := t3Observe(h.c, h.spec)
	if e != nil {
		t.Fatal(e)
	}
	actions := []string{}
	return t3Apply(h.c, h.spec, before, &actions)
}

func (h *t3CustodyHost) transaction(t *testing.T) (string, []byte, map[string]any) {
	t.Helper()
	journals, _ := filepath.Glob(filepath.Join(h.prefix, "lib/node_modules/.upkeeper-t3-*/transaction.json"))
	if len(journals) != 1 {
		t.Fatalf("want one transaction, got %v", journals)
	}
	body, e := os.ReadFile(journals[0])
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]any
	if e = json.Unmarshal(body, &v); e != nil {
		t.Fatal(e)
	}
	return filepath.Dir(journals[0]), body, v
}

func t3SampleStages(v map[string]any) map[string][]string {
	stages := map[string][]string{}
	samples, _ := v["failed_samples"].([]any)
	for _, raw := range samples {
		s, _ := raw.(map[string]any)
		stage, _ := s["stage"].(string)
		failed, _ := s["failed"].([]any)
		for _, f := range failed {
			name, _ := f.(string)
			stages[stage] = append(stages[stage], name)
		}
	}
	return stages
}

// filesRestored produces the retained state the Normandy rollback left: the
// new native descendant disappears after the restart, and the restored
// service fails its health check until the bounded probe gives up.
func (h *t3CustodyHost) filesRestored(t *testing.T) (error, string) {
	t.Helper()
	h.nativeGone, h.brokenAfter = true, 2
	got, e := h.apply(t)
	if e == nil || got == "updated" || got == "converged" {
		t.Fatalf("failed transaction accepted: %s %v", got, e)
	}
	dir, _, v := h.transaction(t)
	if v["phase"] != "files-restored" {
		t.Fatalf("phase %v, want files-restored: %v", v["phase"], e)
	}
	h.nativeGone, h.brokenAfter = false, 0
	return e, filepath.Base(dir)
}

func TestT3CustodyPostStartAndRestorationFailureRetainBoth(t *testing.T) {
	h := newT3CustodyHost(t)
	old, _ := release.T3TreeDigest(filepath.Join(h.prefix, "lib/node_modules/t3"))
	e, _ := h.filesRestored(t)
	var rolled *RolledBack
	if errors.As(e, &rolled) {
		t.Fatalf("unverified restoration reported rolled back: %v", e)
	}
	message := e.Error()
	for _, want := range []string{"post-restart verification failed", "artifact_served", "previous service health not verified", "check_ok"} {
		if !strings.Contains(message, want) {
			t.Errorf("error lost %q: %s", want, message)
		}
	}
	if h.restarts != 2 {
		t.Fatalf("restarts %d, want transaction and restoration only", h.restarts)
	}
	if h.sleeps != 2*(t3ReadinessAttempts-1) {
		t.Fatalf("readiness waits %d, want bounded %d", h.sleeps, 2*(t3ReadinessAttempts-1))
	}
	dir, body, v := h.transaction(t)
	cause, _ := v["cause"].(string)
	restoration, _ := v["restoration"].(string)
	if !strings.Contains(cause, "post-restart verification failed") || !strings.Contains(restoration, "previous service health not verified") {
		t.Fatalf("journal lost cause or restoration: cause=%q restoration=%q", cause, restoration)
	}
	stages := t3SampleStages(v)
	if !strings.Contains(strings.Join(stages["post-restart"], ","), "artifact_served") || !strings.Contains(strings.Join(stages["restoration"], ","), "check_ok") {
		t.Fatalf("failed predicates not retained: %v", stages)
	}
	// The original cause and its samples were durable before any rollback effect.
	required := h.journals["restoration-required"]
	if len(required) == 0 || !strings.Contains(required[0], "post-restart verification failed") || !strings.Contains(required[0], "artifact_served") {
		t.Fatalf("cause not journaled before rollback: %v", required)
	}
	for _, text := range append([]string{message, string(body)}, required...) {
		if strings.Contains(text, t3CustodySecret) {
			t.Fatal("secret leaked into custody evidence")
		}
	}
	if now, _ := release.T3TreeDigest(filepath.Join(h.prefix, "lib/node_modules/t3")); now != old {
		t.Fatal("original tree not restored")
	}
	if failed, _ := release.T3TreeDigest(filepath.Join(dir, "failed")); failed != v["new_digest"] {
		t.Fatal("failed tree not retained")
	}
}

func TestT3CustodyBoundedReadinessProbe(t *testing.T) {
	t.Run("transient", func(t *testing.T) {
		h := newT3CustodyHost(t)
		h.failChecks = 2
		got, e := h.apply(t)
		if e != nil || got != "updated" || h.restarts != 1 || h.sleeps != 2 {
			t.Fatalf("transient readiness not awaited: %s %v restarts=%d sleeps=%d", got, e, h.restarts, h.sleeps)
		}
		_, _, v := h.transaction(t)
		if v["phase"] != "committed" || len(t3SampleStages(v)["post-restart"]) == 0 {
			t.Fatalf("transient samples not retained with commit: %v", v)
		}
	})
	t.Run("never-ready", func(t *testing.T) {
		h := newT3CustodyHost(t)
		h.failChecks = t3ReadinessAttempts
		got, e := h.apply(t)
		var rolled *RolledBack
		if !errors.As(e, &rolled) || got == "updated" || !strings.Contains(e.Error(), "after 6 bounded samples") {
			t.Fatalf("unbounded or unreported readiness: %s %v", got, e)
		}
		_, _, v := h.transaction(t)
		samples, _ := v["failed_samples"].([]any)
		if v["phase"] != "restored" || len(samples) != t3ReadinessAttempts || h.sleeps != t3ReadinessAttempts-1 {
			t.Fatalf("samples %d phase %v sleeps %d", len(samples), v["phase"], h.sleeps)
		}
	})
	t.Run("native-descendant-disappears", func(t *testing.T) {
		h := newT3CustodyHost(t)
		h.nativeGone = true
		got, e := h.apply(t)
		var rolled *RolledBack
		if !errors.As(e, &rolled) || !strings.Contains(e.Error(), "artifact_served") {
			t.Fatalf("vanished native accepted or cause lost: %s %v", got, e)
		}
		if _, _, v := h.transaction(t); !strings.Contains(v["cause"].(string), "artifact_served") {
			t.Fatalf("cause not journaled: %v", v["cause"])
		}
	})
}

func TestT3CustodyDependentStewardRestart(t *testing.T) {
	for _, mode := range []string{"propagated", "not-restarted", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			h := newT3CustodyHost(t)
			h.dependents["t3-steward.service"] = "invocation-1"
			h.propagate = mode == "propagated"
			h.unknownDep = mode == "unknown"
			got, e := h.apply(t)
			switch mode {
			case "propagated":
				if e != nil || got != "updated" {
					t.Fatalf("restarted dependent refused: %s %v", got, e)
				}
			case "not-restarted":
				if e == nil || !strings.Contains(e.Error(), "dependent t3-steward.service") {
					t.Fatalf("stale dependent accepted: %s %v", got, e)
				}
			case "unknown":
				var rolled *RolledBack
				if !errors.As(e, &rolled) || h.restarts != 0 || !strings.Contains(e.Error(), "identity unknown") {
					t.Fatalf("restart without dependent identity: %s %v restarts=%d", got, e, h.restarts)
				}
			}
		})
	}
}

func (h *t3CustodyHost) reconcile(t *testing.T, name string) ([]string, string, error) {
	t.Helper()
	h.c.Payload.ReconcileT3Transaction = name
	h.c.Selected = []string{"t3"}
	before, e := t3Observe(h.c, h.spec)
	if e != nil {
		t.Fatal(e)
	}
	actions := []string{}
	restarts := h.restarts
	got, e := t3Apply(h.c, h.spec, before, &actions)
	if h.restarts != restarts {
		t.Fatal("reconciliation restarted T3")
	}
	return actions, got, e
}

func TestT3ReconcileRetainedFilesRestored(t *testing.T) {
	h := newT3CustodyHost(t)
	_, name := h.filesRestored(t)
	dir, original, v := h.transaction(t)
	failedState := filepath.Join(h.c.Home, release.T3StatePath, "failed-"+name)
	failedStateDigest, _ := release.T3TreeDigest(failedState)
	if failedStateDigest == "" || failedStateDigest != v["new_state_digest"] {
		t.Fatal("fixture lacks retained failed provenance")
	}
	h.c.DryRun = true
	if _, got, e := h.reconcile(t, name); e != nil || got != "updated" {
		t.Fatalf("read-only reconciliation refused: %s %v", got, e)
	}
	if _, body, _ := h.transaction(t); string(body) != string(original) {
		t.Fatal("dry run wrote the journal")
	}
	h.c.DryRun = false
	// An interrupted reconciliation leaves the transaction exactly as it was.
	h.failJournal = "reconciled"
	if _, got, e := h.reconcile(t, name); e == nil || got == "updated" || !strings.Contains(e.Error(), "journal") {
		t.Fatalf("interrupted reconciliation reported success: %s %v", got, e)
	}
	if _, body, _ := h.transaction(t); string(body) != string(original) {
		t.Fatal("interrupted reconciliation changed the journal")
	}
	h.failJournal = ""
	actions, got, e := h.reconcile(t, name)
	if e != nil || got != "updated" {
		t.Fatalf("resume refused: %s %v", got, e)
	}
	_, body, v := h.transaction(t)
	if v["phase"] != "restored" || v["reconciliation"] == nil || !strings.Contains(v["cause"].(string), "post-restart") ||
		!strings.Contains(v["restoration"].(string), "previous service health not verified") {
		t.Fatalf("reconciled record lost provenance: %s", body)
	}
	from, _ := v["reconciled_from"].(map[string]any)
	if from["sha256"] != release.T3Digest(original) {
		t.Fatal("original files-restored journal not preserved")
	}
	if failed, _ := release.T3TreeDigest(filepath.Join(dir, "failed")); failed != v["new_digest"] {
		t.Fatal("failed tree not preserved")
	}
	if now, _ := release.T3TreeDigest(failedState); now != failedStateDigest {
		t.Fatal("failed provenance not preserved")
	}
	if len(actions) != 1 || !strings.Contains(actions[0], "retained at "+dir) {
		t.Fatalf("actions %v", actions)
	}
	if _, got, e = h.reconcile(t, name); e != nil || got != "converged" {
		t.Fatalf("repeated reconciliation not idempotent: %s %v", got, e)
	}
	if _, again, _ := h.transaction(t); string(again) != string(body) {
		t.Fatal("repeated reconciliation rewrote the journal")
	}
}

func TestT3ReconcileRefusesMismatch(t *testing.T) {
	reasons := map[string]string{
		"live-tree": "live T3 tree differs", "launcher": "live T3 launcher differs",
		"failed-tree": "retained failed T3 tree differs", "failed-provenance": "retained failed T3 provenance or receipts differ",
		"unit-drift": "unit binding differs", "unit-foreign": "unknown T3 service invocation",
		"unhealthy": "reconciliation verification failed after 6 bounded samples: check_ok, served",
		"name":      "not a transaction directory", "missing": "is missing",
	}
	for _, drift := range []string{"live-tree", "launcher", "failed-tree", "failed-provenance", "unit-drift", "unit-foreign", "unhealthy", "name", "missing", "other-component"} {
		t.Run(drift, func(t *testing.T) {
			h := newT3CustodyHost(t)
			_, name := h.filesRestored(t)
			dir, original, _ := h.transaction(t)
			packagePath := filepath.Join(h.prefix, "lib/node_modules/t3")
			failedState := filepath.Join(h.c.Home, release.T3StatePath, "failed-"+name)
			switch drift {
			case "live-tree":
				adapterTestWrite(t, filepath.Join(packagePath, "foreign"), "foreign")
			case "launcher":
				if e := t3SwapLink("../lib/node_modules/t3/package.json", filepath.Join(h.prefix, "bin/t3")); e != nil {
					t.Fatal(e)
				}
			case "failed-tree":
				adapterTestWrite(t, filepath.Join(dir, "failed/foreign"), "foreign")
			case "failed-provenance":
				adapterTestWrite(t, filepath.Join(failedState, "receipt.json"), "{}")
			case "unit-drift":
				launcher := filepath.Join(h.prefix, "bin/t3")
				h.execStart = "{ path=" + launcher + " ; argv[]=" + launcher + " serve --host 127.0.0.1 --port 7392 --no-browser ; ignore_errors=no ; }"
			case "unit-foreign":
				h.execStart = "{ path=/foreign/t3 ; argv[]=/foreign/t3 start ; }"
			case "unhealthy":
				h.brokenAfter = 1
			case "name":
				name = "../t3"
			case "missing":
				name = ".upkeeper-t3-1"
			}
			if drift == "other-component" {
				h.c.Payload.ReconcileT3Transaction = name
				h.c.Selected = []string{"t3-steward", "t3"}
				actions := []string{}
				if got, e := t3Apply(h.c, h.spec, map[string]any{}, &actions); e == nil {
					t.Fatalf("reconciled beside another component: %s", got)
				}
			} else if _, got, e := h.reconcile(t, name); e == nil || got != "" || !strings.Contains(e.Error(), reasons[drift]) {
				t.Fatalf("%s accepted or refused for another reason: %s %v", drift, got, e)
			}
			if _, body, _ := h.transaction(t); string(body) != string(original) {
				t.Fatal("refusal changed the journal")
			}
			for _, p := range []string{filepath.Join(dir, "failed"), failedState} {
				if _, e := os.Lstat(p); e != nil {
					t.Fatalf("retained evidence removed: %s", p)
				}
			}
		})
	}
}
