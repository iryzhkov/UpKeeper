package rollout

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func recoveryCompletedHost(t *testing.T, c Config, host string, now time.Time) {
	t.Helper()
	for _, v := range []struct{ action, host string }{{"run:" + host, host}, {"phase:observe:" + host, ""}, {"phase:accept:" + host, ""}} {
		ack, err := beginAttempt(c, v.action, v.host, now)
		if err != nil {
			t.Fatal(err)
		}
		if err = ack(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestRepair2ProductionFailedInvocationHostAuthority(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"agent-b", "normandy"} {
		s.Extra["host-pre:"+host] = jsonBytes(f.snapshot(host))
		f.upgraded[host] = true
		s.Completed["host:"+host] = true
		recoveryCompletedHost(t, c, host, r.Now())
		s.Extra["observation:"+host] = jsonBytes(ObservationReceipt{Host: host, ConfigDigest: c.Digest(), ReleaseCommit: s.ReleaseCommit, Started: r.Now().Add(-time.Hour), Finished: r.Now(), Samples: 361, Identity: jsonBytes(map[string]string{"epoch": "2"})})
	}
	if err = saveState(c.StatePath, s); err != nil {
		t.Fatal(err)
	}
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		p, e := f.Run(ctx, dir, stdin, name, args...)
		if name == "upkeeper" && len(args) > 2 && args[0] == "pull" && args[2] == "homelab" {
			if !f.upgraded["homelab"] {
				t.Fatal("missing real transport effect")
			}
			return p, errors.New("homelab pull took effect then42")
		}
		return p, e
	})
	err = RunHostWithRuntime(context.Background(), c, b, "homelab", true, io.Discard, r)
	if err == nil || !strings.Contains(err.Error(), "rollback completed") {
		t.Fatal("homelab failure not recovered", err)
	}
	s, err = loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.InFlight != "" || s.Failed == "" || s.FailedInvocation == nil || s.FailedInvocation.Host != "homelab" || s.FailedInvocation.Operation != "host:homelab" || !s.FailedInvocation.RecoveryAcknowledged {
		t.Fatalf("lost durable exact authority: %+v", s)
	}
	f.calls = nil
	if err = RecoverHost(context.Background(), c, b, "agent-b", true); err == nil || len(f.calls) != 0 {
		t.Fatal("unrelated completed host recovery", err, f.calls)
	}
	if err = RecoverHost(context.Background(), c, b, "homelab", true); err == nil || !strings.Contains(err.Error(), "rollback completed") {
		t.Fatal("exact failed invocation not recoverable", err)
	}
	s, _ = loadState(c)
	if !strings.Contains(s.FailedInvocation.Failure, "42") {
		t.Fatal("original invocation failure erased", s.FailedInvocation)
	}
	if len(s.FailedInvocation.RecoveryHistory) != 2 {
		t.Fatal("recovery history erased", s.FailedInvocation)
	}
}

func TestRepair2ProtocolRecoveryAuthorityAndPersistenceFaults(t *testing.T) {
	for _, fault := range []string{"none", "before-effect-save", "after-effect-save", "after-clear-save", "after-ack-save", "ack-lost"} {
		t.Run(fault, func(t *testing.T) {
			c := rc122Fixture(t)
			r, _ := fakeRuntime()
			f := protocolBackend()
			f.fail = "observe:agent-b"
			injected := false
			r.Save = func(path string, s *State) error {
				if !injected && s.FailedInvocation != nil {
					a := s.FailedInvocation
					hit := fault == "before-effect-save" && s.InFlight == "rollback:agent-b" && !a.RecoveryEffectReturned ||
						fault == "after-effect-save" && a.RecoveryEffectReturned ||
						fault == "after-clear-save" && a.RecoveryEffectReturned && s.InFlight == "" || fault == "after-ack-save" && a.RecoveryAcknowledged
					if hit {
						injected = true
						if fault != "before-effect-save" {
							if err := saveState(path, s); err != nil {
								return err
							}
						}
						return errors.New("injected recovery save fault")
					}
				}
				return saveState(path, s)
			}
			if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err == nil {
				t.Fatal("failed run accepted")
			}
			if fault == "ack-lost" {
				dir := c.StatePath + ".attempts"
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "recovery-rollback-agent-b-") {
						p := dir + "/" + e.Name()
						raw, err := os.ReadFile(p)
						if err != nil {
							t.Fatal(err)
						}
						line := strings.Split(string(raw), "\n")[0] + "\n"
						if err = os.WriteFile(p, []byte(line), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			s, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			if s.FailedInvocation == nil || s.FailedInvocation.Host != "agent-b" || s.FailedInvocation.Operation != "observe:agent-b" {
				t.Fatal("lost exact failure", s)
			}
			f.actions = nil
			if err = RecoverHost(context.Background(), c, f, "normandy", true); err == nil || len(f.actions) != 0 {
				t.Fatal("wrong host recovered", err, f.actions)
			}
			f.fail = ""
			if err = RecoverHost(context.Background(), c, f, "agent-b", true); err == nil || !contains(f.actions, "rollback:agent-b") {
				t.Fatal("exact same failed recovery refused", err, f.actions)
			}
			s, _ = loadState(c)
			if s.Failed == "" || s.FailedInvocation == nil {
				t.Fatal("failure authority erased")
			}
		})
	}
}

func TestRepair2MissingCorruptConflictingRecoveryAuthority(t *testing.T) {
	for _, fault := range []string{"missing", "wrong-host", "wrong-operation", "wrong-start", "wrong-config", "missing-intent", "completed-historical", "missing-terminal-failure", "wrong-invocation-kind", "missing-recovery-history", "wrong-recovery-kind", "wrong-recovery-start", "corrupt-recovery-ack"} {
		t.Run(fault, func(t *testing.T) {
			c := rc122Fixture(t)
			r, _ := fakeRuntime()
			f := protocolBackend()
			f.fail = "observe:agent-b"
			if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err == nil {
				t.Fatal("missing failure")
			}
			s, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "missing":
				s.Invocation = nil
				s.FailedInvocation = nil
			case "wrong-host":
				s.FailedInvocation.Host = "normandy"
			case "wrong-operation":
				s.FailedInvocation.Operation = "host:normandy"
			case "wrong-start":
				s.FailedInvocation.Started = s.FailedInvocation.Started.Add(time.Second)
			case "wrong-config":
				s.FailedInvocation.ConfigDigest = "bad"
			case "missing-terminal-failure":
				s.Failed = ""
			case "wrong-invocation-kind":
				intent, _, e := readAttempt(c, attemptPath(c, "run:agent-b"), r.Now())
				if e != nil {
					t.Fatal(e)
				}
				s.FailedInvocation.Attempt = "run:agent-b"
				s.FailedInvocation.Started = intent.Started
			case "missing-recovery-history":
				s.FailedInvocation.RecoveryHistory = nil
			case "wrong-recovery-kind":
				s.FailedInvocation.RecoveryHistory[0].Operation = "dropin-restore:agent-b"
				s.FailedInvocation.RecoveryOperation = "dropin-restore:agent-b"
			case "wrong-recovery-start":
				s.FailedInvocation.RecoveryHistory[0].Started = s.FailedInvocation.RecoveryHistory[0].Started.Add(time.Second)
			case "corrupt-recovery-ack":
				p := attemptPath(c, s.FailedInvocation.RecoveryHistory[0].Attempt)
				raw, e := os.ReadFile(p)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(p, append(raw, []byte("corrupt\\n")...), 0600); e != nil {
					t.Fatal(e)
				}
			case "missing-intent":
				if err = os.Remove(attemptPath(c, s.FailedInvocation.Attempt)); err != nil {
					t.Fatal(err)
				}
			case "completed-historical":
				ack, err := beginAttempt(c, "run:normandy", "normandy", r.Now())
				if err != nil {
					t.Fatal(err)
				}
				if err = ack(); err != nil {
					t.Fatal(err)
				}
				s.FailedInvocation = &InvocationAuthority{Host: "normandy", Operation: "host:normandy", Attempt: "run:normandy", ConfigDigest: c.Digest(), Started: r.Now()}
			}
			if err = saveState(c.StatePath, s); err != nil {
				t.Fatal(err)
			}
			f.actions = nil
			host := "agent-b"
			if fault == "completed-historical" {
				host = "normandy"
			}
			if err = RecoverHost(context.Background(), c, f, host, true); err == nil || len(f.actions) != 0 {
				t.Fatal("bad authority effect", err, f.actions)
			}
		})
	}
}
