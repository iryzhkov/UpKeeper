package rollout

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func productionHostFixture(t *testing.T) (*ProductionBackend, *backendFake, Config, Runtime) {
	t.Helper()
	b, f, _, s := backendFixture(t)
	c := rc122Fixture(t)
	t.Setenv("HOME", t.TempDir())
	c.Targets = map[string]string{"agent-b": "canary", "normandy": "coord", "homelab": "middle", "agent-a": "worker-a", "omarchy-pc": "local", "laptop": "laptop"}
	if err := validateWalk(c); err != nil {
		t.Fatal(err)
	}
	f.c = c
	fencedFake(f)
	now := time.Unix(10000, 0)
	b.Now = func() time.Time { return now }
	b.Sleep = func(context.Context, time.Duration) error { return nil }
	s.ConfigDigest = c.Digest()
	s.TimerPrior = map[string]string{}
	s.Completed = map[string]bool{"fence": true, "prepare": true, "publish": true, "release-ci": true}
	s.Extra["assets"] = jsonBytes(assetEvidence{Binaries: map[string]string{"linux_amd64": digest([]byte("linux_amd64"))}})
	if err := os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveState(c.StatePath, s); err != nil {
		t.Fatal(err)
	}
	f.saved = true
	return b, f, c, Runtime{Now: b.Now, Sleep: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }, Save: saveState}
}
func productionPulls(f *backendFake) int {
	n := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, "upkeeper pull ") && !strings.Contains(call, "--dry-run") {
			n++
		}
	}
	return n
}
func noProductionContinuation(t *testing.T, b *ProductionBackend, f *backendFake, c Config, r Runtime) {
	t.Helper()
	calls := len(f.calls)
	for _, host := range rc122Order {
		if err := RunHostWithRuntime(context.Background(), c, b, host, true, &bytes.Buffer{}, r); err == nil {
			t.Fatalf("accepted continuation %s", host)
		}
	}
	if err := FinishWorker(context.Background(), c, b, "agent-b", true); err == nil {
		t.Fatal("enrollment crossed uncertainty fence")
	}
	if len(f.calls) != calls {
		t.Fatalf("terminal journal dispatched commands: %v", f.calls[calls:])
	}
}
func TestProductionRunHostEffectThen42FencesNextHostAndEnrollment(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		out, err := f.Run(ctx, dir, stdin, name, args...)
		if name == "upkeeper" && len(args) > 0 && args[0] == "pull" && !contains(args, "--dry-run") {
			if !f.upgraded[c.Canary] {
				t.Fatal("transport effect not applied")
			}
			return out, errors.New("remote command took effect then exited 42")
		}
		return out, err
	})
	err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, &bytes.Buffer{}, r)
	if err == nil || !strings.Contains(err.Error(), "42") {
		t.Fatalf("expected ambiguous refusal: %v", err)
	}
	if productionPulls(f) != 1 {
		t.Fatalf("pull count %d", productionPulls(f))
	}
	noProductionContinuation(t, b, f, c, r)
	if productionPulls(f) != 1 {
		t.Fatal("ambiguous pull retried")
	}
}
func TestProductionRunHostIntentSaveFailureHasZeroPulls(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	r.Save = func(path string, s *State) error {
		if s.InFlight == "host:"+c.Canary {
			return errors.New("injected pre-effect intent fsync failure")
		}
		return saveState(path, s)
	}
	if err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, &bytes.Buffer{}, r); err == nil {
		t.Fatal("accepted failed intent")
	}
	if productionPulls(f) != 0 {
		t.Fatal("pull before saved intent")
	}
	noProductionContinuation(t, b, f, c, r)
}
func TestProductionRunHostCompletionSaveThenErrorFencesContinuation(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == c.ActivityCommand[0] {
			no := false
			return jsonBytes(ActivityReceipt{Host: args[0], CoordinatorEpoch: args[1], CollectionDeferred: &no, RunningWithoutActivity: &no}), nil
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	injected := false
	r.Save = func(path string, s *State) error {
		if err := saveState(path, s); err != nil {
			return err
		}
		if s.Completed["host:"+c.Canary] && !injected {
			injected = true
			return errors.New("checkpoint renamed then directory fsync failed")
		}
		return nil
	}
	err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, &bytes.Buffer{}, r)
	if err == nil || !injected {
		t.Fatalf("completion failure never reached: %v", err)
	}
	if productionPulls(f) != 1 {
		t.Fatal("expected one convergence")
	}
	noProductionContinuation(t, b, f, c, r)
}
func TestProductionRunHostCorruptAndConflictingJournalZeroEffects(t *testing.T) {
	for _, kind := range []string{"truncated", "trailing", "unknown-field", "conflicting-config"} {
		t.Run(kind, func(t *testing.T) {
			b, f, c, r := productionHostFixture(t)
			switch kind {
			case "truncated":
				if err := os.WriteFile(c.StatePath, []byte("{\"ConfigDigest\":"), 0600); err != nil {
					t.Fatal(err)
				}
			case "trailing":
				p, err := os.ReadFile(c.StatePath)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(c.StatePath, append(p, []byte("{}")...), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-field":
				if err := os.WriteFile(c.StatePath, []byte("{\"unknown\":true}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "conflicting-config":
				c.PreviousRelease = strings.Repeat("d", 40)
			}
			if err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, &bytes.Buffer{}, r); err == nil {
				t.Fatal("accepted " + kind)
			}
			if len(f.calls) != 0 {
				t.Fatalf("commands before valid journal: %v", f.calls)
			}
		})
	}
}
func TestProductionRunHostConcurrentLockZeroEffects(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	lock, err := os.OpenFile(c.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err = RunHostWithRuntime(context.Background(), c, b, c.Canary, true, &bytes.Buffer{}, r); err == nil {
		t.Fatal("accepted conflicting lock")
	}
	if len(f.calls) != 0 {
		t.Fatal("commands crossed concurrent lock")
	}
}
func TestProductionRunHostRealNonRootUnreadableAndUnwritableJournal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("genuine owner permission check requires non-root")
	}
	for _, kind := range []string{"unreadable-state", "unwritable-inventory"} {
		t.Run(kind, func(t *testing.T) {
			b, f, c, r := productionHostFixture(t)
			p, mode := c.StatePath, os.FileMode(0200)
			if kind == "unwritable-inventory" {
				p = filepath.Dir(c.StatePath)
				mode = 0500
			}
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(p, 0700)
			if err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, &bytes.Buffer{}, r); err == nil {
				t.Fatal("accepted " + kind)
			}
			if len(f.calls) != 0 {
				t.Fatalf("commands crossed failed journal permissions: %v", f.calls)
			}
		})
	}
}

func TestProductionRunHostActivityProducer42CannotBecomeReceipt(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == c.ActivityCommand[0] {
			no := false
			return jsonBytes(ActivityReceipt{Host: args[0], CoordinatorEpoch: args[1], CollectionDeferred: &no, RunningWithoutActivity: &no}), errors.New("valid activity then exit42")
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	if err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, &bytes.Buffer{}, r); err == nil {
		t.Fatal("failed producer accepted")
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Completed["host:"+c.Canary] || len(s.Extra["observation:"+c.Canary]) > 0 {
		t.Fatal("observation accepted nonzero producer")
	}
	if productionPulls(f) != 1 {
		t.Fatal("wrong effect count")
	}
	noProductionContinuation(t, b, f, c, r)
}
func TestProductionRecoverRefusesCorruptIntentBeforeCommands(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	if err := os.MkdirAll(c.StatePath+".attempts", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attemptPath(c, "run:"+c.Canary), []byte("corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	s.InFlight = "host:" + c.Canary
	s.Extra["host-pre:"+c.Canary] = jsonBytes(map[string]string{})
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	if err = RecoverHost(context.Background(), c, b, c.Canary, true); err == nil {
		t.Fatal("corrupt intent authorized rollback")
	}
	if len(f.calls) != 0 {
		t.Fatal("commands before valid recovery intent", f.calls)
	}
	_ = r
}
