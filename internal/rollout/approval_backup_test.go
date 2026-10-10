package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Follow the review's production-backend reproduction through review, CI,
// canary and soak. All commands and clock changes are fake.
func approvalBoundary(t *testing.T) (*ProductionBackend, *backendFake, Config, *time.Time) {
	t.Helper()
	b, f, c, _ := backendFixture(t)
	c.FleetApproval = ""
	c.ReviewFile = c.Checkout + "/external-review.md"
	f.c, f.saved = c, true
	now := time.Unix(10000, 0)
	b.Now = func() time.Time { return now }
	if err := Run(context.Background(), c, b, true, io.Discard); err == nil {
		t.Fatal("missing review accepted")
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	review := "VERDICT: ACCEPT\n" + s.ExpectHead + "\n" + s.ReleaseSHA256 + "\nPACKAGE " + s.PackageDigest + "\n"
	if err = os.WriteFile(c.ReviewFile, []byte(review), 0600); err != nil {
		t.Fatal(err)
	}
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		p, e := f.Run(ctx, dir, stdin, name, args...)
		if name == "upkeeper" && args[0] == "push" {
			f.ciBad = true
		}
		return p, e
	})
	if err = Run(context.Background(), c, b, true, io.Discard); err == nil {
		t.Fatal("pending CI accepted")
	}
	s, err = loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(time.Now().Add(-31 * time.Minute))
	s.Extra["soak-start"] = raw
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	f.ciBad, b.Runner = false, f
	if err = Run(context.Background(), c, b, true, io.Discard); err == nil || !strings.Contains(err.Error(), "fleet approval") {
		t.Fatalf("approval boundary: %v", err)
	}
	s, err = loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Completed["soak"] || s.Completed["backup"] || !s.BackupCreated.IsZero() || s.InFlight != "" {
		t.Fatalf("backup before approval: %+v", s)
	}
	for _, script := range f.stdin {
		if script == legacyBackupScript {
			t.Fatal("coordinator stopped before approval")
		}
	}
	return b, f, c, &now
}

func assertNoRollback(t *testing.T, f *backendFake, c Config) {
	t.Helper()
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Failed != "" || strings.HasPrefix(s.InFlight, "rollback:") {
		t.Fatalf("stranded journal: %+v", s)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "upkeeper rollback") && !strings.Contains(call, "--dry-run") {
			t.Fatalf("unconverged host rolled back: %s", call)
		}
	}
}

func TestReviewApprovalPauseStalesBackupAndStrandsJournal(t *testing.T) {
	b, f, c, now := approvalBoundary(t)
	*now = now.Add(61 * time.Minute)
	c.FleetApproval = "operator go after reviewing canary and soak"
	if err := Run(context.Background(), c, b, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Completed["verify"] || !s.BackupCreated.Equal(*now) {
		t.Fatalf("fresh binding backup not used: %+v", s)
	}
	assertNoRollback(t, f, c)
	if err = Run(context.Background(), c, b, true, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestProductionPreConvergenceRefusalResumesWithoutRollback(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "snapshot unavailable", true: "stale backup"}[stale], func(t *testing.T) {
			b, f, c, now := approvalBoundary(t)
			c.FleetApproval = "operator go"
			injected := false
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				if !injected && stdin == snapshotScript && strings.Contains(strings.Join(args, " "), "coord") {
					injected = true
					return nil, errors.New("pre-convergence snapshot unavailable")
				}
				return f.Run(ctx, dir, stdin, name, args...)
			})
			err := Run(context.Background(), c, b, true, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "snapshot unavailable") {
				t.Fatalf("expected snapshot refusal: %v", err)
			}
			assertNoRollback(t, f, c)
			s, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			if s.InFlight != "" || s.Extra["host-pre:"+c.CoordinatorHost] != nil {
				t.Fatalf("preflight stranded state: %+v", s)
			}
			b.Runner = f
			if stale {
				*now = now.Add(61 * time.Minute)
				err = Run(context.Background(), c, b, true, io.Discard)
				if err == nil || !strings.Contains(err.Error(), "backup absent or stale") {
					t.Fatalf("stale backup accepted: %v", err)
				}
				assertNoRollback(t, f, c)
				s, err = loadState(c)
				if err != nil {
					t.Fatal(err)
				}
				if s.Completed["backup"] || s.InFlight != "" {
					t.Fatalf("stale backup not scheduled for refresh: %+v", s)
				}
			}
			if err = Run(context.Background(), c, b, true, io.Discard); err != nil {
				t.Fatal(err)
			}
			assertNoRollback(t, f, c)
			s, err = loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			if !s.Completed["verify"] || !s.BackupCreated.Equal(*now) {
				t.Fatal("resume failed to use fresh backup")
			}
		})
	}
}
