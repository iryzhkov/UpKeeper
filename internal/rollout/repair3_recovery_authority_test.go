package rollout

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every case completes the real production entrypoint first. Removing a final
// unsynced append models its documented loss; it does not claim a power-cut test.
func repair3CompletedForward(t *testing.T, kind string, absent bool) (*ProductionBackend, *backendFake, Config, Runtime, *int) {
	t.Helper()
	b, f, c, r := productionHostFixture(t)
	if absent {
		// Construct a separate, reviewed absence fixture before any D5 effect.
		b, f, _, s := backendFixture(t)
		c = rc122Fixture(t)
		t.Setenv("HOME", t.TempDir())
		c.Targets = map[string]string{"agent-b": "canary", "normandy": "coord", "homelab": "middle", "agent-a": "worker-a", "omarchy-pc": "local", "laptop": "laptop"}
		c.DropinAbsentHosts = []string{"agent-b"}
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
		seedPublication(t, c, s)
		for action := range s.Completed {
			ack, err := beginAttempt(c, "phase:"+action, "", b.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err = ack(); err != nil {
				t.Fatal(err)
			}
		}
		if err := saveState(c.StatePath, s); err != nil {
			t.Fatal(err)
		}
		f.saved = true
		return repair3CompleteFixture(t, kind, absent, b, f, c, Runtime{Now: b.Now, Sleep: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }, Save: saveState})
	}
	return repair3CompleteFixture(t, kind, absent, b, f, c, r)
}

func repair3CompleteFixture(t *testing.T, kind string, absent bool, b *ProductionBackend, f *backendFake, c Config, r Runtime) (*ProductionBackend, *backendFake, Config, Runtime, *int) {
	t.Helper()
	restores := new(int)
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == c.ActivityCommand[0] {
			no := false
			return jsonBytes(ActivityReceipt{Host: args[0], CoordinatorEpoch: args[1], CollectionDeferred: &no, RunningWithoutActivity: &no}), nil
		}
		if stdin == dropinScript {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "inspect") {
				if absent {
					return jsonBytes(dropinReceipt{Absent: true}), nil
				}
				return jsonBytes(dropinReceipt{SHA256: strings.Repeat("b", 64), Mode: 0600, UID: 1000, GID: 1000}), nil
			}
			if strings.Contains(joined, "restore") {
				*restores++
			}
			return nil, nil
		}
		if name == c.HaikuCheckCommand[0] {
			return jsonBytes(map[string]string{"worker_id": args[len(args)-2], "coordinator_epoch": args[len(args)-1], "model": "claudeAgent/claude-haiku-5-5"}), nil
		}
		if strings.Contains(stdin, strings.Join(quoted(c.DropinTestCommand), " ")) {
			return nil, nil
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	if kind == "run" {
		if err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, io.Discard, r); err != nil {
			t.Fatal("complete production run", err)
		}
	} else {
		s, err := loadState(c)
		if err != nil {
			t.Fatal(err)
		}
		seedD5Observed(t, c, s, r.Now())
		if err = s.Save(); err != nil {
			t.Fatal(err)
		}
		for _, h := range append(append([]string{}, rc122Order...), c.DrainedHosts...) {
			f.upgraded[h] = true
		}
		if err = FinishWorker(context.Background(), c, b, c.Canary, true); err != nil {
			t.Fatal("complete production D5", err)
		}
		if s, err = loadState(c); err != nil {
			t.Fatal(err)
		}
		if err = requiredD5(c, s, []string{c.Canary}, r.Now()); err != nil {
			t.Fatal("complete original D5 receipt", err)
		}
	}
	return b, f, c, r, restores
}

func repair3HasRecovery(f *backendFake, restores int) bool {
	if restores > 0 {
		return true
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "upkeeper rollback ") {
			return true
		}
	}
	return false
}

func TestRepair3FinalAckLossAuthorityBoundaries(t *testing.T) {
	for _, kind := range []string{"run", "enroll"} {
		for _, fault := range []string{"ack-present", "missing-reference", "wrong-reference", "wrong-enclosing-start", "missing-child-ack", "double-child-ack", "double-enclosing-ack", "corrupt-enclosing-intent", "missing-receipt", "missing-completion", "wrong-receipt-host", "missing-dispatch-fence", "missing-original-evidence", "corrupt-original-evidence", "conflicting-identity", "incomplete-original-evidence"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				b, f, c, r, restores := repair3CompletedForward(t, kind, false)
				action := kind + ":" + c.Canary
				if fault != "ack-present" {
					independentLoseFinalAck(t, c, action)
				}
				s, err := loadState(c)
				if err != nil {
					t.Fatal(err)
				}
				switch fault {
				case "missing-reference":
					s.Invocation.EnclosingAttempt = ""
					s.Invocation.EnclosingStarted = time.Time{}
				case "wrong-reference":
					s.Invocation.EnclosingAttempt = kind + ":agent-a"
				case "wrong-enclosing-start":
					s.Invocation.EnclosingStarted = s.Invocation.EnclosingStarted.Add(-time.Second)
				case "missing-child-ack":
					independentLoseFinalAck(t, c, s.Invocation.Attempt)
				case "double-child-ack", "double-enclosing-ack":
					p := attemptPath(c, s.Invocation.Attempt)
					if fault == "double-enclosing-ack" {
						p = attemptPath(c, action)
					}
					raw, e := os.ReadFile(p)
					if e != nil {
						t.Fatal(e)
					}
					line := strings.Split(string(raw), "\n")[0] + "\n"
					ack := "complete:" + digest([]byte(line)) + "\n"
					if e = os.WriteFile(p, []byte(line+ack+ack), 0600); e != nil {
						t.Fatal(e)
					}
				case "corrupt-enclosing-intent":
					if err = os.WriteFile(attemptPath(c, action), []byte("{corrupt}\n"), 0600); err != nil {
						t.Fatal(err)
					}
				case "missing-receipt":
					key := "observation:" + c.Canary
					if kind == "enroll" {
						key = "dropin:" + c.Canary
					}
					delete(s.Extra, key)
				case "missing-completion":
					key := "host:" + c.Canary
					if kind == "enroll" {
						key = "dropin:" + c.Canary
					}
					delete(s.Completed, key)
				case "wrong-receipt-host":
					if kind == "run" {
						var receipt ObservationReceipt
						if err = evidence(s, "observation:"+c.Canary, &receipt); err != nil {
							t.Fatal(err)
						}
						receipt.Host = "agent-a"
						s.Extra["observation:"+c.Canary] = jsonBytes(receipt)
					} else {
						var receipt dropinReceipt
						if err = evidence(s, "dropin:"+c.Canary, &receipt); err != nil {
							t.Fatal(err)
						}
						receipt.Host = "agent-a"
						s.Extra["dropin:"+c.Canary] = jsonBytes(receipt)
					}
				case "missing-original-evidence":
					key := "host-pre:" + c.Canary
					if kind == "enroll" {
						key = "dropin:" + c.Canary
					}
					delete(s.Extra, key)
				case "corrupt-original-evidence":
					key := "host-pre:" + c.Canary
					if kind == "enroll" {
						key = "dropin:" + c.Canary
					}
					s.Extra[key] = jsonBytes("corrupt evidence")
				case "incomplete-original-evidence":
					if kind == "run" {
						var pre map[string]string
						if err = evidence(s, "host-pre:"+c.Canary, &pre); err != nil {
							t.Fatal(err)
						}
						delete(pre, "binary_sha256")
						s.Extra["host-pre:"+c.Canary] = jsonBytes(pre)
					} else {
						var receipt dropinReceipt
						if err = evidence(s, "dropin:"+c.Canary, &receipt); err != nil {
							t.Fatal(err)
						}
						receipt.SHA256 = ""
						s.Extra["dropin:"+c.Canary] = jsonBytes(receipt)
					}
				case "conflicting-identity":
					if kind == "run" {
						s.Extra["identity-post:"+c.Canary] = jsonBytes(identity{Health: "healthy", Epoch: "2", Release: "wrong"})
					} else {
						var receipt dropinReceipt
						if err = evidence(s, "dropin:"+c.Canary, &receipt); err != nil {
							t.Fatal(err)
						}
						receipt.IntentAt = receipt.IntentAt.Add(-time.Second)
						s.Extra["dropin:"+c.Canary] = jsonBytes(receipt)
					}
				case "missing-dispatch-fence":
					// A merely unfinished child cannot use the final-ack exception.
					s.Invocation.EnclosingAttempt = ""
					s.Invocation.EnclosingStarted = time.Time{}
					s.Invocation.EffectReturned = false
					independentLoseFinalAck(t, c, s.Invocation.Attempt)
				}
				if err = saveState(c.StatePath, s); err != nil {
					t.Fatal(err)
				}
				f.calls = nil
				err = RecoverHost(context.Background(), c, b, c.Canary, true)
				if err == nil || len(f.calls) != 0 || *restores != 0 {
					t.Fatal("ambiguous authority dispatched recovery", err, f.calls, *restores)
				}
				if fault != "ack-present" {
					noProductionContinuation(t, b, f, c, r)
				}
			})
		}
	}
}

func TestRepair3FinalAckLossExactRecoveryReentry(t *testing.T) {
	for _, kind := range []string{"run", "enroll"} {
		t.Run(kind, func(t *testing.T) {
			b, f, c, r, restores := repair3CompletedForward(t, kind, false)
			action := kind + ":" + c.Canary
			independentLoseFinalAck(t, c, action)
			f.calls = nil
			if err := RecoverHost(context.Background(), c, b, "agent-a", true); err == nil || len(f.calls) > 0 || *restores > 0 {
				t.Fatal("other host recovered", err, f.calls, *restores)
			}
			for i := 1; i <= 2; i++ {
				f.calls = nil
				if err := RecoverHost(context.Background(), c, b, c.Canary, true); err == nil || !repair3HasRecovery(f, *restores) {
					t.Fatal("exact named recovery missing", err, f.calls, *restores)
				}
				s, err := loadState(c)
				if err != nil {
					t.Fatal(err)
				}
				if s.Failed == "" || s.FailedInvocation == nil || len(s.FailedInvocation.RecoveryHistory) != i || !s.FailedInvocation.RecoveryHistory[i-1].Acknowledged {
					t.Fatal("terminal recovery history lost", s.FailedInvocation)
				}
				if _, complete, err := readAttempt(c, attemptPath(c, action), r.Now()); err != nil || complete {
					t.Fatal("recovery rewrote original enclosing intent", complete, err)
				}
				noProductionContinuation(t, b, f, c, r)
			}
			if kind == "enroll" && *restores != 2 {
				t.Fatal("D5 reentry failed to restore twice", *restores)
			}
		})
	}
}

func TestRepair3ReviewedAbsentD5FinalCompletion(t *testing.T) {
	b, f, c, r, restores := repair3CompletedForward(t, "enroll", true)
	independentLoseFinalAck(t, c, "enroll:"+c.Canary)
	f.calls = nil
	if err := RecoverHost(context.Background(), c, b, c.Canary, true); err == nil || len(f.calls) > 0 || *restores > 0 {
		t.Fatal("absent original invented restoration", err, f.calls, *restores)
	}
	noProductionContinuation(t, b, f, c, r)
}

func TestRepair3FinalAuthoritySaveFailureRemainsTerminal(t *testing.T) {
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	failed := false
	r.Save = func(path string, s *State) error {
		if !failed && s.Invocation != nil && s.Invocation.EnclosingAttempt != "" {
			failed = true
			if err := saveState(path, s); err != nil {
				return err
			}
			return io.ErrUnexpectedEOF
		}
		return saveState(path, s)
	}
	if err := RunHostWithRuntime(context.Background(), c, f, c.Canary, true, io.Discard, r); err == nil || !failed {
		t.Fatal("enclosing save failure accepted", err)
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Failed == "" || s.FailedInvocation == nil {
		t.Fatal("completion fault lost terminal authority")
	}
	if _, done, err := readAttempt(c, attemptPath(c, "run:"+c.Canary), r.Now()); err != nil || done {
		t.Fatal("failed save acknowledged enclosing run", done, err)
	}
	f.actions = nil
	if err := RecoverHost(context.Background(), c, f, c.Canary, true); err == nil || !contains(f.actions, "rollback:"+c.Canary) {
		t.Fatal("exact terminal save fault could not recover", err, f.actions)
	}
}
