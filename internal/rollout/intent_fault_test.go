package rollout

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestEveryPhaseCompletionPersistenceErrorRemainsTerminal(t *testing.T) {
	for _, action := range []string{"fence", "prepare", "publish", "release-ci", "backup", "config-patch", "enroll-worker:agent-b", "dropin:agent-b", "verify"} {
		t.Run(action, func(t *testing.T) {
			c := rc122Fixture(t)
			r, _ := fakeRuntime()
			b := protocolBackend()
			if err := durableDirectory(filepath.Dir(c.StatePath)); err != nil {
				t.Fatal(err)
			}
			s, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			s.Save = func() error {
				if err := saveState(c.StatePath, s); err != nil {
					return err
				}
				if s.Completed[action] {
					return errors.New("success-shaped replacement then directory sync42")
				}
				return nil
			}
			if err := executeSaved(context.Background(), c, b, s, action, r.Now()); err == nil {
				t.Fatal("completion fault accepted")
			}
			b.actions = nil
			if err := RunHostWithRuntime(context.Background(), c, b, "agent-b", true, io.Discard, r); err == nil {
				t.Fatal("ambiguous phase resumed")
			}
			if len(b.actions) > 0 {
				t.Fatal("effect crossed permanent phase fence", b.actions)
			}
		})
	}
}
func TestAcceptedHistoryRequiresEveryIntentAndAcknowledgement(t *testing.T) {
	for _, fault := range []string{"inventory", "phase", "phase-ack", "run", "run-ack", "observe", "observe-ack", "newer-unfinished"} {
		t.Run(fault, func(t *testing.T) {
			c := rc122Fixture(t)
			r, _ := fakeRuntime()
			b := protocolBackend()
			if err := RunHostWithRuntime(context.Background(), c, b, "agent-b", true, io.Discard, r); err != nil {
				t.Fatal(err)
			}
			action := "phase:release-ci"
			switch fault {
			case "inventory":
				if err := os.RemoveAll(c.StatePath + ".attempts"); err != nil {
					t.Fatal(err)
				}
			case "phase", "run", "observe":
				if fault == "run" {
					action = "run:agent-b"
				}
				if fault == "observe" {
					action = "phase:observe:agent-b"
				}
				if err := os.Remove(attemptPath(c, action)); err != nil {
					t.Fatal(err)
				}
			case "phase-ack", "run-ack", "observe-ack":
				if fault == "run-ack" {
					action = "run:agent-b"
				}
				if fault == "observe-ack" {
					action = "phase:observe:agent-b"
				}
				raw, err := os.ReadFile(attemptPath(c, action))
				if err != nil {
					t.Fatal(err)
				}
				for i, b := range raw {
					if b == '\n' {
						raw = raw[:i+1]
						break
					}
				}
				if err := os.WriteFile(attemptPath(c, action), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "newer-unfinished":
				if _, err := beginAttempt(c, "run:normandy", "normandy", r.Now()); err != nil {
					t.Fatal(err)
				}
			}
			b.actions = nil
			if err := RunHostWithRuntime(context.Background(), c, b, "normandy", true, io.Discard, r); err == nil {
				t.Fatal("missing/newer evidence accepted")
			}
			if len(b.actions) > 0 {
				t.Fatal("acceptance invoked effects without complete evidence", b.actions)
			}
		})
	}
}
