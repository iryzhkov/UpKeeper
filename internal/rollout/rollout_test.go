package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{ReviewFile: filepath.Join(dir, "review.md"), Version: "0.11.0-rc.119", PreviousRelease: strings.Repeat("a", 40), StewardRepo: "owner/steward", UpKeeperRepo: "owner/upkeeper", Checkout: dir, StatePath: filepath.Join(dir, "journal", "state.json"), LocalHost: "controller", CoordinatorHost: "coordinator", Canary: "canary", LastHost: "controller", LaptopHost: "excluded", Remotes: []string{"github", "mirror"}, FleetOrder: []string{"coordinator", "middle", "controller"}, TimerHosts: []string{"controller", "coordinator", "excluded"}, DrainedHosts: []string{"coordinator", "middle"}, ActiveWorkers: []string{"canary", "controller"}, Targets: map[string]string{"controller": "local", "coordinator": "coord", "canary": "canary", "middle": "middle", "excluded": "laptop"}, QuietCommand: []string{"quiet"}, BackupMaxAgeSeconds: 3600, ReadinessSeconds: 180, SoakSeconds: 1800, FleetApproval: "approved window"}
}

type fakeBackend struct {
	actions   []string
	checks    int
	fail      string
	interrupt string
	cancel    context.CancelFunc
	before    func(string, *State)
}

func (f *fakeBackend) Check(context.Context, Config) error { f.checks++; return nil }
func (f *fakeBackend) Execute(ctx context.Context, c Config, a string, s *State) error {
	f.actions = append(f.actions, a)
	if f.before != nil {
		f.before(a, s)
	}
	if a == f.fail {
		return errors.New("failed gate")
	}
	if a == f.interrupt {
		f.cancel()
		return ctx.Err()
	}
	if a == "host:"+c.Canary {
		raw, _ := json.Marshal(time.Now().Add(-time.Hour))
		s.Extra["soak-start"] = raw
	}
	return nil
}
func TestCriterion10EveryMutationNeedsYes(t *testing.T) {
	c := fixture(t)
	f := &fakeBackend{}
	if err := Run(context.Background(), c, f, false, io.Discard); err == nil {
		t.Fatal("missing authority accepted")
	}
	if len(f.actions) != 0 || f.checks != 0 {
		t.Fatal("backend touched")
	}
	if _, err := os.Stat(filepath.Dir(c.StatePath)); !os.IsNotExist(err) {
		t.Fatal("journal directory created without --yes")
	}
}
func TestCheckAndPlanReadOnly(t *testing.T) {
	c := fixture(t)
	f := &fakeBackend{}
	if _, err := Plan(c); err != nil {
		t.Fatal(err)
	}
	if err := f.Check(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(c.StatePath)); !os.IsNotExist(err) {
		t.Fatal("read-only operation wrote state")
	}
	if len(f.actions) != 0 {
		t.Fatal("effect reached")
	}
}
func TestCriterion8OrderExclusionAndEnrollment(t *testing.T) {
	c := fixture(t)
	f := &fakeBackend{}
	if err := Run(context.Background(), c, f, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, a := range f.actions {
		if strings.HasPrefix(a, "host:") {
			hosts = append(hosts, a)
		}
	}
	if !reflect.DeepEqual(hosts, []string{"host:canary", "host:coordinator", "host:middle", "host:controller"}) {
		t.Fatal(hosts)
	}
	if f.actions[len(f.actions)-2] != "enroll" || f.actions[len(f.actions)-1] != "verify" {
		t.Fatal(f.actions)
	}
	f.actions = nil
	if err := Run(context.Background(), c, f, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 0 {
		t.Fatal("completed hosts repeated", f.actions)
	}
}
func TestCriterion9FailedCanaryAndMiddleRollback(t *testing.T) {
	for _, host := range []string{"canary", "middle"} {
		t.Run(host, func(t *testing.T) {
			c := fixture(t)
			f := &fakeBackend{fail: "host:" + host}
			if err := Run(context.Background(), c, f, true, io.Discard); err == nil {
				t.Fatal("failure accepted")
			}
			if f.actions[len(f.actions)-1] != "rollback:"+host {
				t.Fatal(f.actions)
			}
			if contains(f.actions, "enroll") {
				t.Fatal("advanced after failure")
			}
			f.actions = nil
			if err := Run(context.Background(), c, f, true, io.Discard); err == nil {
				t.Fatal("terminal failure resumed")
			}
			if len(f.actions) != 0 {
				t.Fatal(f.actions)
			}
		})
	}
}
func TestResumeBetweenHosts(t *testing.T) {
	c := fixture(t)
	f := &fakeBackend{fail: "backup"}
	if err := Run(context.Background(), c, f, true, io.Discard); err == nil {
		t.Fatal("expected interrupted backup")
	}
	f.actions = nil
	f.fail = ""
	if err := Run(context.Background(), c, f, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if contains(f.actions, "host:canary") {
		t.Fatal("canary repeated")
	}
	if !contains(f.actions, "host:coordinator") {
		t.Fatal("coordinator skipped")
	}
}
func TestInterruptedHostRollsBackOnResume(t *testing.T) {
	c := fixture(t)
	if err := os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	s.InFlight = "host:middle"
	s.Extra["host-pre:middle"] = json.RawMessage(`{"prior":"snapshot"}`)
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	f := &fakeBackend{}
	if err = Run(context.Background(), c, f, true, io.Discard); err == nil {
		t.Fatal("interruption accepted")
	}
	if !reflect.DeepEqual(f.actions, []string{"rollback:middle"}) {
		t.Fatal(f.actions)
	}
}
func TestInterruptedRollbackRetriesEvenWithFailedJournal(t *testing.T) {
	c := fixture(t)
	f := &fakeBackend{fail: "host:middle", before: func(a string, s *State) {
		if a == "rollback:middle" {
			s.Save = func() error { return nil }
		}
	}}
	// Seed exactly the durable checkpoint of a killed rollback.
	if err := os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	s.Failed = "host:middle failed"
	s.InFlight = "rollback:middle"
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	f = &fakeBackend{}
	if err = Run(context.Background(), c, f, true, io.Discard); err == nil {
		t.Fatal("terminal state lost")
	}
	if !reflect.DeepEqual(f.actions, []string{"rollback:middle"}) {
		t.Fatal(f.actions)
	}
}
func TestResumeRejectsConfigDrift(t *testing.T) {
	c := fixture(t)
	f := &fakeBackend{fail: "backup"}
	_ = Run(context.Background(), c, f, true, io.Discard)
	c.Version = "0.11.0-rc.120"
	f.actions = nil
	if err := Run(context.Background(), c, f, true, io.Discard); err == nil || !strings.Contains(err.Error(), "configuration differs") {
		t.Fatal(err)
	}
	if len(f.actions) != 0 {
		t.Fatal(f.actions)
	}
}
func TestInterruptedPublicationRequiresReconciliation(t *testing.T) {
	c := fixture(t)
	os.MkdirAll(filepath.Dir(c.StatePath), 0700)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	s.InFlight = "publish"
	s.Extra["publication-started"] = json.RawMessage("true")
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	f := &fakeBackend{}
	if err = Run(context.Background(), c, f, true, io.Discard); err == nil || !strings.Contains(err.Error(), "publication was interrupted") {
		t.Fatal(err)
	}
	if len(f.actions) != 0 {
		t.Fatal(f.actions)
	}
}
func TestFinalGateRollsBackCompletedFleetInReverseOrder(t *testing.T) {
	for _, gate := range []string{"enroll", "verify"} {
		t.Run(gate, func(t *testing.T) {
			c := fixture(t)
			f := &fakeBackend{fail: gate}
			if err := Run(context.Background(), c, f, true, io.Discard); err == nil {
				t.Fatal("failed final gate accepted")
			}
			var got []string
			for _, a := range f.actions {
				if strings.HasPrefix(a, "rollback:") {
					got = append(got, a)
				}
			}
			want := []string{"rollback:controller", "rollback:middle", "rollback:coordinator", "rollback:canary"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%v", got)
			}
		})
	}
}
func TestFleetRollbackQueueResumesAtInterruptedHost(t *testing.T) {
	c := fixture(t)
	f := &fakeBackend{}
	// Persist a failure at the middle of a reverse walk without rerolling the completed controller.
	if err := os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	s.Failed = "verify gate"
	s.InFlight = "rollback:middle"
	s.Extra["rollback-queue"] = json.RawMessage(`["rollback:middle","rollback:coordinator","rollback:canary"]`)
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	f = &fakeBackend{}
	if err = Run(context.Background(), c, f, true, io.Discard); err == nil {
		t.Fatal("terminal failure lost")
	}
	if !reflect.DeepEqual(f.actions, []string{"rollback:middle", "rollback:coordinator", "rollback:canary"}) {
		t.Fatal(f.actions)
	}
}
func TestReviewRefusalCanResumeWithoutAmbiguousPublication(t *testing.T) {
	c := fixture(t)
	f := &fakeBackend{fail: "publish"}
	if err := Run(context.Background(), c, f, true, io.Discard); err == nil {
		t.Fatal("review failure accepted")
	}
	f.fail = ""
	f.actions = nil
	if err := Run(context.Background(), c, f, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if contains(f.actions, "prepare") {
		t.Fatal("prepared candidate repeated")
	}
}
func TestConfigurationRules(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.PreviousRelease = "main" },
		func(c *Config) { c.FleetOrder = append(c.FleetOrder, c.LaptopHost) },
		func(c *Config) { c.Canary = c.CoordinatorHost },
		func(c *Config) { c.ActiveWorkers = append(c.ActiveWorkers, c.DrainedHosts[0]) },
		func(c *Config) { c.TimerHosts = []string{c.LocalHost} },
		func(c *Config) { c.SoakSeconds = 1 },
	} {
		c := fixture(t)
		change(&c)
		if err := c.Validate(); err == nil {
			t.Fatal("unsafe config accepted")
		}
	}
}
func TestRollbackNotCanceledByInterrupt(t *testing.T) {
	c := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeBackend{interrupt: "host:canary", cancel: cancel, before: func(a string, s *State) {
		if a == "rollback:canary" && ctx.Err() == nil {
			t.Fatal("expected cancellation")
		}
	}}
	if err := Run(ctx, c, f, true, io.Discard); err == nil {
		t.Fatal("interruption accepted")
	}
	if f.actions[len(f.actions)-1] != "rollback:canary" {
		t.Fatal(f.actions)
	}
}
