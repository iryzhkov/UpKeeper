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

// Capture the on-disk journal inside the production preflight, then restore it
// after the error unwinds. This models process death without running a process
// or allowing the refusal handler to repair the persisted kill-point state.
func TestReviewHardInterruptDuringHostPreflightResumes(t *testing.T) {
	for _, host := range []string{"canary", "middle", "coordinator"} {
		t.Run(host, func(t *testing.T) {
			b, f, c, now := approvalBoundary(t)
			c.FleetApproval = "operator go"
			if host == c.Canary {
				// Restore the fake host as well as its journal to before canary convergence.
				f.upgraded[host] = false
				s, err := loadState(c)
				if err != nil {
					t.Fatal(err)
				}
				delete(s.Completed, "host:"+host)
				delete(s.Completed, "soak")
				delete(s.Extra, "host-pre:"+host)
				delete(s.Extra, "identity-pre:"+host)
				delete(s.Extra, "pull:"+host)
				if err = s.Save(); err != nil {
					t.Fatal(err)
				}
			}
			var killed []byte
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				if killed == nil && stdin == snapshotScript && strings.Contains(strings.Join(args, " "), c.Targets[host]) {
					var err error
					killed, err = os.ReadFile(c.StatePath)
					if err != nil {
						t.Fatal(err)
					}
					return nil, errors.New("simulated hard interruption")
				}
				return f.Run(ctx, dir, stdin, name, args...)
			})
			if err := Run(context.Background(), c, b, true, io.Discard); err == nil || !strings.Contains(err.Error(), "simulated hard interruption") {
				t.Fatalf("kill point not reached: %v", err)
			}
			if len(killed) == 0 {
				t.Fatal("journal not captured")
			}
			if err := os.WriteFile(c.StatePath, killed, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			if s.InFlight != "host:"+host || s.Extra["host-pre:"+host] != nil || s.Extra["pull:"+host] != nil {
				t.Fatalf("wrong kill-point journal: %+v", s)
			}
			// Resume still requires mutation authority and must leave the journal alone.
			if err = Run(context.Background(), c, b, false, io.Discard); err == nil {
				t.Fatal("resume without --yes accepted")
			}
			persisted, err := os.ReadFile(c.StatePath)
			if err != nil || string(persisted) != string(killed) {
				t.Fatalf("unauthorized resume changed journal: %v", err)
			}
			// A second failed preflight proves clearing the hard-interruption checkpoint
			// is durable, including if the gate still cannot pass on the first resume.
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				if stdin == snapshotScript && strings.Contains(strings.Join(args, " "), c.Targets[host]) {
					return nil, errors.New("snapshot still unavailable")
				}
				return f.Run(ctx, dir, stdin, name, args...)
			})
			if err = Run(context.Background(), c, b, true, io.Discard); err == nil || !strings.Contains(err.Error(), "snapshot still unavailable") {
				t.Fatalf("resume did not retry preflight: %v", err)
			}
			assertNoRollback(t, f, c)
			s, err = loadState(c)
			if err != nil || s.InFlight != "" {
				t.Fatalf("preflight checkpoint not cleared: %+v %v", s, err)
			}
			if host == c.CoordinatorHost {
				*now = now.Add(61 * time.Minute)
				b.Runner = f
				if err = Run(context.Background(), c, b, true, io.Discard); err == nil || !strings.Contains(err.Error(), "backup absent or stale") {
					t.Fatalf("stale backup accepted after interruption: %v", err)
				}
				assertNoRollback(t, f, c)
			}
			b.Runner = f
			if err = Run(context.Background(), c, b, true, io.Discard); err != nil {
				t.Fatal(err)
			}
			assertNoRollback(t, f, c)
			s, err = loadState(c)
			if err != nil || !s.Completed["verify"] || s.InFlight != "" {
				t.Fatalf("resume incomplete: %+v %v", s, err)
			}
			if host == c.CoordinatorHost && !s.BackupCreated.Equal(*now) {
				t.Fatal("stale backup not retaken")
			}
			if err = Run(context.Background(), c, b, true, io.Discard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A snapshot checkpoint, even without a pull receipt, remains ambiguous:
// the controller can die after dispatch but before recording the receipt.
func TestInterruptedHostSnapshotWithoutPullStillRollsBack(t *testing.T) {
	c := fixture(t)
	if err := os.MkdirAll(c.Checkout+"/journal", 0700); err != nil {
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
		t.Fatal("ambiguous effect advanced")
	}
	if len(f.actions) != 1 || f.actions[0] != "rollback:middle" {
		t.Fatalf("expected only immutable rollback: %v", f.actions)
	}
}
