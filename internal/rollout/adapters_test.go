package rollout

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type holdFenceFake struct {
	set    int
	checks []bool
}

func (f *holdFenceFake) Check(_ context.Context, _ Config, fenced bool) error {
	f.checks = append(f.checks, fenced)
	if fenced && f.set == 0 {
		return errors.New("hold missing")
	}
	return nil
}
func (f *holdFenceFake) Set(_ context.Context, c Config, s *State) error {
	s.TimerPrior = map[string]string{c.LocalHost: "hold prior"}
	if s.Save != nil {
		if err := s.Save(); err != nil {
			return err
		}
	}
	f.set++
	return nil
}
func TestHoldAdapterReplacesBothSettingAndVerification(t *testing.T) {
	b, runner, c, s := backendFixture(t)
	f := &holdFenceFake{}
	b.Fence = f
	// Timers remain active in this fake: a hold need not stop them.
	if err := b.Check(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := b.Execute(context.Background(), c, "fence", s); err != nil {
		t.Fatal(err)
	}
	if f.set != 1 || len(f.checks) != 2 || f.checks[0] || !f.checks[1] {
		t.Fatalf("%+v", f)
	}
	for _, script := range runner.stdin {
		if strings.Contains(script, "stop upkeeper-self.timer") || script == timerScript {
			t.Fatal("hold adapter still coupled to timer state")
		}
	}
	runner.mask = "enabled"
	if err := b.timers(context.Background(), c, true); err == nil {
		t.Fatal("hold adapter bypassed excluded laptop mask")
	}
}

type backupAdapterFake struct {
	now     time.Time
	invalid bool
	calls   int
}

func (f *backupAdapterFake) CreateAndVerify(_ context.Context, _ Config, s *State) error {
	f.calls++
	if f.invalid {
		return nil
	}
	s.BackupCreated = f.now
	s.BackupSnapshot = "/local/immutable/snapshot"
	s.BackupScratch = "/local/scratch"
	return record(s, "backup", map[string]string{"restore_integrity": "ok", "restore_fk_violations": "0"})
}
func TestStewardBackupAdapterStillRequiresDrillEvidence(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		b, runner, c, s := backendFixture(t)
		fencedFake(runner)
		provider := &backupAdapterFake{now: b.Now(), invalid: invalid}
		b.CoordinatorBackup = provider
		err := b.Execute(context.Background(), c, "backup", s)
		if (err != nil) != invalid {
			t.Fatalf("invalid=%v error=%v", invalid, err)
		}
		if provider.calls != 1 {
			t.Fatal("provider not called")
		}
		for _, script := range runner.stdin {
			if script == legacyBackupScript {
				t.Fatal("new backup provider still ran legacy stopped snapshot")
			}
		}
	}
}
