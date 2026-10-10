package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func rc122Fixture(t *testing.T) Config {
	c := fixture(t)
	c.Canary = "agent-b"
	c.CoordinatorHost = "normandy"
	c.LocalHost = "omarchy-pc"
	c.LastHost = "omarchy-pc"
	c.LaptopHost = "laptop"
	c.FleetOrder = []string{"normandy", "homelab", "agent-a", "omarchy-pc"}
	c.TimerHosts = []string{"normandy", "homelab", "omarchy-pc", "laptop"}
	c.ActiveWorkers = []string{"agent-b", "agent-a", "omarchy-pc"}
	c.DrainedHosts = []string{"normandy", "homelab"}
	c.Targets = map[string]string{}
	for _, h := range append(append([]string{}, rc122Order...), "laptop") {
		c.Targets[h] = h
	}
	text := "reviewed secretrefs only\n"
	c.CoordinatorConfig = &ConfigChange{BeforeSHA256: strings.Repeat("a", 64), AfterSHA256: digest([]byte(text)), Content: text, ValidateCommand: []string{"validator", "{config}"}}
	c.ActivityCommand = []string{"activity"}
	c.HaikuCheckCommand = []string{"advertisement"}
	c.DropinTestCommand = []string{"isolated-test"}
	return c
}
func fakeRuntime() (Runtime, *time.Time) {
	now := time.Unix(10000, 0)
	return Runtime{Now: func() time.Time { return now }, Sleep: func(ctx context.Context, d time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		now = now.Add(d)
		return nil
	}, Save: saveState}, &now
}
func protocolBackend() *fakeBackend {
	return &fakeBackend{before: func(action string, s *State) {
		if strings.HasPrefix(action, "host:") {
			s.Extra["host-pre:"+strings.TrimPrefix(action, "host:")] = json.RawMessage("{}")
			s.Extra["identity-post:"+strings.TrimPrefix(action, "host:")] = json.RawMessage(`{"Health":"healthy","Epoch":1,"Release":"reviewed"}`)
		}
	}}
}
func TestRunHostOnlyExplicitFixedOrderedHost(t *testing.T) {
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	for _, host := range []string{"", "laptop", "normandy", "agent-a"} {
		if err := RunHostWithRuntime(context.Background(), c, f, host, true, io.Discard, r); err == nil {
			t.Fatalf("accepted %s", host)
		}
		if len(f.actions) != 0 {
			t.Fatal("effects before predecessor", f.actions)
		}
	}
	if err := RunHostWithRuntime(context.Background(), c, f, c.Canary, true, io.Discard, r); err != nil {
		t.Fatal(err)
	}
	for _, host := range c.FleetOrder {
		if contains(f.actions, "host:"+host) {
			t.Fatal("hidden fleet walk", f.actions)
		}
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Completed["host:agent-b"] || !s.Completed["soak"] || s.Completed["backup"] {
		t.Fatal(s)
	}
	var receipt ObservationReceipt
	if err = evidence(s, "observation:agent-b", &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Finished.Sub(receipt.Started) < 30*time.Minute || receipt.Samples < 361 {
		t.Fatal(receipt)
	}
	f.actions = nil
	if err = RunHostWithRuntime(context.Background(), c, f, c.Canary, true, io.Discard, r); err == nil || len(f.actions) != 0 {
		t.Fatal("completed canary reran", err, f.actions)
	}
	if err = RunHostWithRuntime(context.Background(), c, f, "normandy", true, io.Discard, r); err != nil {
		t.Fatal(err)
	}
	if !contains(f.actions, "config-patch") || contains(f.actions, "host:homelab") {
		t.Fatal(f.actions)
	}
}
func TestRunHostAuthorityAndIntentBeforeAnyEffect(t *testing.T) {
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", false, io.Discard, r); err == nil {
		t.Fatal("missing authority")
	}
	if len(f.actions) != 0 {
		t.Fatal(f.actions)
	}
	r.Save = func(string, *State) error { return errors.New("fsync failure") }
	if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err == nil {
		t.Fatal("failed intent accepted")
	}
	if len(f.actions) != 0 {
		t.Fatal("effect before saved intent", f.actions)
	}
}
func TestRunHostCompletionSaveErrorAfterRenameFencesNextHost(t *testing.T) {
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	injected := false
	r.Save = func(path string, s *State) error {
		err := saveState(path, s)
		if err != nil {
			return err
		}
		if s.Completed["host:agent-b"] && !injected {
			injected = true
			return errors.New("directory fsync error after completion rename")
		}
		return nil
	}
	if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err == nil {
		t.Fatal("ambiguous completion accepted")
	}
	if !injected {
		t.Fatal("fault not reached")
	}
	f.actions = nil
	r.Save = saveState
	for _, host := range []string{"agent-b", "normandy"} {
		if err := RunHostWithRuntime(context.Background(), c, f, host, true, io.Discard, r); err == nil {
			t.Fatal("uncertain save resumed")
		}
		if len(f.actions) != 0 {
			t.Fatal("next effect after uncertain save", f.actions)
		}
	}
	if err := FinishWorker(context.Background(), c, f, "agent-b", true); err == nil || len(f.actions) != 0 {
		t.Fatal("enrolled after uncertainty")
	}
}
func TestRunHostObservedFaultRollsBackOnlyCurrentHost(t *testing.T) {
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	f.fail = "observe:agent-b"
	if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err == nil {
		t.Fatal("failed sample accepted")
	}
	if !contains(f.actions, "rollback:agent-b") || contains(f.actions, "host:normandy") {
		t.Fatal(f.actions)
	}
	f.actions = nil
	if err := RunHostWithRuntime(context.Background(), c, f, "normandy", true, io.Discard, r); err == nil || len(f.actions) != 0 {
		t.Fatal("failed gate resumed")
	}
}
func TestRunHostClockJumpDoesNotSubstituteForSoak(t *testing.T) {
	c := rc122Fixture(t)
	r, now := fakeRuntime()
	f := protocolBackend()
	r.Sleep = func(context.Context, time.Duration) error { *now = now.Add(31 * time.Minute); return nil }
	if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err == nil {
		t.Fatal("unobserved interval accepted")
	}
	if !contains(f.actions, "rollback:agent-b") {
		t.Fatal(f.actions)
	}
}
func TestRunHostCorruptAndConcurrentJournalRefuse(t *testing.T) {
	for _, raw := range []string{"{", `{"ConfigDigest":"wrong"}`, `{"unknown":true}`, "{} {}"} {
		c := rc122Fixture(t)
		r, _ := fakeRuntime()
		f := protocolBackend()
		os.MkdirAll(filepath.Dir(c.StatePath), 0700)
		if err := os.WriteFile(c.StatePath, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err == nil || len(f.actions) != 0 {
			t.Fatal("corrupt journal effects", err, f.actions)
		}
	}
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	os.MkdirAll(filepath.Dir(c.StatePath), 0700)
	lock, err := os.OpenFile(c.StatePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err = RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err == nil || len(f.actions) != 0 {
		t.Fatal("concurrent lock allowed effect")
	}
}
func TestRunHostUnreadableAttemptInventoryAsNonRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("genuine permissions require non-root")
	}
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	dir := c.StatePath + ".attempts"
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unfinished"), []byte("saved unfinished intent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0333); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err == nil || len(f.actions) != 0 {
		t.Fatal("unreadable inventory treated as empty", err, f.actions)
	}
}
