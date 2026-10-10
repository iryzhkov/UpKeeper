package rollout

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupCannotAgeOutDuringD2IntentOrPullPreflight(t *testing.T) {
	for _, point := range []string{"D2-intent", "pull-before", "pull-verification"} {
		t.Run(point, func(t *testing.T) {
			b, c, s, path := configFixture(t)
			now := s.BackupCreated
			b.Now = func() time.Time { return now }
			if point == "D2-intent" {
				s.Save = func() error {
					if len(s.Extra["config-intent"]) > 0 {
						now = now.Add(2 * time.Hour)
					}
					return nil
				}
				if err := b.configPatch(context.Background(), c, s); err == nil {
					t.Fatal("backup aged out during intent persistence")
				}
				data, err := os.ReadFile(path)
				if err != nil || digest(data) != c.CoordinatorConfig.BeforeSHA256 {
					t.Fatal("stale backup allowed D2 effect", err)
				}
				if b.Runner.(*configShellRunner).calls != 0 {
					t.Fatal("D2 dispatched despite stale intent boundary")
				}
				return
			}
			if err := b.configPatch(context.Background(), c, s); err != nil {
				t.Fatal(err)
			}
			c.StatePath = filepath.Join(t.TempDir(), "state.json")
			acknowledge, err := beginAttempt(c, "phase:config-patch", "", now)
			if err != nil {
				t.Fatal(err)
			}
			if err = acknowledge(); err != nil {
				t.Fatal(err)
			}
			s.Completed = map[string]bool{"config-patch": true}
			runner := b.Runner
			calls := 0
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				calls++
				out, err := runner.Run(ctx, dir, stdin, name, args...)
				if point == "pull-verification" {
					now = now.Add(2 * time.Hour)
				}
				return out, err
			})
			if point == "pull-before" {
				now = now.Add(2 * time.Hour)
			}
			if err := b.configAtPull(context.Background(), c, s); err == nil {
				t.Fatal("stale preflight backup accepted at actual pull boundary")
			}
			expected := 0
			if point == "pull-verification" {
				expected = 1
			}
			if calls != expected {
				t.Fatal("fresh config gate not exercised at intended boundary", calls)
			}
		})
	}
}
