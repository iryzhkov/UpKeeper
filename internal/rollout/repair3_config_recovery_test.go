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

func TestIndependentCoordinatorRecoveryMissingConfigIntent(t *testing.T) {
	for _, omit := range []bool{false, true} {
		name := "valid-receipt"
		if omit {
			name = "missing-receipt"
		}
		t.Run(name, func(t *testing.T) {
			b, f, c, r := productionHostFixture(t)
			cfgBackend, cfg, _, configPath := configFixture(t)
			c.CoordinatorConfig = cfg.CoordinatorConfig
			f.c = c
			f.configHome = cfgBackend.Runner.(*configShellRunner).home
			var s *State
			// Rebind the fixture's publication and pre-host intents before any tested operation.
			// The seed journal used productionHostFixture's original configuration digest.
			raw, err := os.ReadFile(c.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			var state State
			if err = json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			state.ConfigDigest = c.Digest()
			state.Save = func() error { return saveState(c.StatePath, &state) }
			for action := range state.Completed {
				if err = os.Remove(attemptPath(c, "phase:"+action)); err != nil {
					t.Fatal(err)
				}
				ack, err := beginAttempt(c, "phase:"+action, "", r.Now())
				if err != nil {
					t.Fatal(err)
				}
				if err = ack(); err != nil {
					t.Fatal(err)
				}
			}
			seedPublication(t, c, &state)
			state.Extra["host-pre:agent-b"] = jsonBytes(f.snapshot("agent-b"))
			f.upgraded["agent-b"] = true
			state.Completed["host:agent-b"] = true
			recoveryCompletedHost(t, c, "agent-b", r.Now())
			state.Extra["observation:agent-b"] = jsonBytes(ObservationReceipt{Host: "agent-b", ConfigDigest: c.Digest(), ReleaseCommit: state.ReleaseCommit, Started: r.Now().Add(-time.Hour), Finished: r.Now(), Samples: 361, Identity: jsonBytes(map[string]string{"epoch": "1"})})
			if err = state.Save(); err != nil {
				t.Fatal(err)
			}
			blockRestore := true
			rollbacks := 0
			var rollbackConfig string
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				if stdin == configTransactionScript && strings.Contains(strings.Join(args, " "), "'restore'") && blockRestore {
					return nil, errors.New("injected restore transport failure before effect")
				}
				if name == "upkeeper" && len(args) > 0 && args[0] == "rollback" && !contains(args, "--dry-run") {
					rollbacks++
					data, e := os.ReadFile(configPath)
					if e != nil {
						return nil, e
					}
					rollbackConfig = string(data)
				}
				out, e := f.Run(ctx, dir, stdin, name, args...)
				if name == "upkeeper" && len(args) > 2 && args[0] == "pull" && args[2] == "normandy" && !contains(args, "--dry-run") {
					if !f.upgraded["normandy"] {
						t.Fatal("missing real dispatched pull effect")
					}
					return out, errors.New("coordinator pull took effect then42")
				}
				return out, e
			})
			err = RunHostWithRuntime(context.Background(), c, b, "normandy", true, io.Discard, r)
			if err == nil || !strings.Contains(err.Error(), "restore transport failure") {
				t.Fatal("expected initial restoration failure after pull", err)
			}
			if rollbacks != 0 {
				t.Fatal("old binary dispatched despite restore failure")
			}
			data, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != c.CoordinatorConfig.Content {
				t.Fatal("fixture did not retain actual installed after bytes", string(data))
			}
			s, err = loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			if s.FailedInvocation == nil || s.FailedInvocation.Host != "normandy" || s.FailedInvocation.Operation != "host:normandy" {
				t.Fatal("missing genuine failed coordinator authority", s.FailedInvocation)
			}
			var receipt configReceipt
			if err = evidence(s, "config-intent", &receipt); err != nil || !receipt.Done || receipt.Restored {
				t.Fatal("expected genuine installed receipt", err, receipt)
			}
			if omit {
				delete(s.Extra, "config-intent")
				if err = saveState(c.StatePath, s); err != nil {
					t.Fatal(err)
				}
			}
			blockRestore = false
			err = RecoverHost(context.Background(), c, b, "normandy", true)
			t.Logf("omit=%v recovery=%v rollbacks=%d rollbackConfigDigest=%s before=%s after=%s", omit, err, rollbacks, digest([]byte(rollbackConfig)), c.CoordinatorConfig.BeforeSHA256, c.CoordinatorConfig.AfterSHA256)
			if omit {
				if err == nil || !strings.Contains(err.Error(), "missing configuration intent") {
					t.Fatal("missing receipt did not fence at original-config evidence", err)
				}
				if retry := RecoverHost(context.Background(), c, b, "normandy", true); retry == nil {
					t.Fatal("missing receipt reentry accepted")
				}
				if rollbacks != 0 {
					t.Fatal("old binary rollback dispatched while patched configuration remained and required original receipt was absent")
				}
			} else {
				if rollbacks != 1 || digest([]byte(rollbackConfig)) != c.CoordinatorConfig.BeforeSHA256 {
					t.Fatal("positive recovery did not restore actual original bytes before old binary", err, rollbacks, rollbackConfig)
				}
			}
			noProductionContinuation(t, b, f, c, r)
		})
	}
}
