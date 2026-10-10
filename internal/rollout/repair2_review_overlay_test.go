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

func independentFinal(t *testing.T) (*ProductionBackend, *backendFake, Config, Runtime) {
	t.Helper()
	b, f, c, r := productionHostFixture(t)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range rc122Order {
		s.Extra["host-pre:"+h] = jsonBytes(f.snapshot(h))
		f.upgraded[h] = true
		s.Completed["host:"+h] = true
		reviewCompletedHost(t, c, h, r.Now())
		s.Extra["observation:"+h] = jsonBytes(ObservationReceipt{Host: h, ConfigDigest: c.Digest(), ReleaseCommit: s.ReleaseCommit, Started: r.Now().Add(-time.Hour), Finished: r.Now(), Samples: 361, Identity: jsonBytes(map[string]string{"epoch": "2"})})
	}
	for _, h := range c.ActiveWorkers {
		s.Completed["enroll-worker:"+h] = true
		reviewAcknowledged(t, c, "enroll:"+h, h, r.Now())
		reviewAcknowledged(t, c, "phase:enroll-worker:"+h, "", r.Now())
		if h == "agent-b" || h == "agent-a" {
			s.Completed["dropin:"+h] = true
			reviewAcknowledged(t, c, "phase:dropin:"+h, "", r.Now())
			s.Extra["dropin:"+h] = jsonBytes(dropinReceipt{Host: h, Done: true, SHA256: strings.Repeat("a", 64), Mode: 0640, UID: os.Getuid(), GID: os.Getgid(), IntentAt: r.Now()})
		}
	}
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		p, e := f.Run(ctx, dir, stdin, name, args...)
		if name == "upkeeper" && len(args) > 0 && args[0] == "status" && e == nil {
			var doc map[string]any
			if err := json.Unmarshal(p, &doc); err != nil {
				t.Fatal(err)
			}
			for _, row := range doc["hosts"].([]any) {
				comps := []any{}
				for _, component := range []string{"omarchy-setup", "agent-config", "agent-tooling", "jocasta", "codex-controlled-runtime", "agent-environment", "huyang", "t3-steward", "t3", "worker-configuration", "steward-fleet-configuration"} {
					comps = append(comps, map[string]any{"name": component, "state": "converged"})
				}
				row.(map[string]any)["components"] = comps
			}
			return jsonBytes(doc), nil
		}
		return p, e
	})
	return b, f, c, r
}
func TestIndependentPackageAssetsBinding(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	s, _ := loadState(c)
	pkg := struct {
		Head, Release, ConfigDigest string
		TimerPrior                  map[string]string
		Evidence                    map[string]json.RawMessage
	}{s.ExpectHead, s.ReleaseSHA256, c.Digest(), s.TimerPrior, s.Extra}
	raw := jsonBytes(pkg)
	s.PackageDigest = digest(raw)
	if err := os.WriteFile(c.StatePath+".review-package.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	rogue := digest([]byte("unreviewed binary"))
	s.Extra["assets"] = jsonBytes(assetEvidence{Binaries: map[string]string{"linux_amd64": rogue}})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == c.ActivityCommand[0] {
			no := false
			return jsonBytes(ActivityReceipt{Host: args[0], CoordinatorEpoch: args[1], CollectionDeferred: &no, RunningWithoutActivity: &no}), nil
		}
		p, e := f.Run(ctx, dir, stdin, name, args...)
		if stdin == snapshotScript && f.upgraded[c.Canary] {
			kv := keyValues(p)
			kv["binary_sha256"] = rogue
			return encodeKV(kv), e
		}
		return p, e
	})
	err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, io.Discard, r)
	t.Logf("journal assets differ from unchanged accepted package: err=%v pulls=%d", err, productionPulls(f))
	if err == nil {
		t.Fatal("UNSAFE accepted binary digest from mutable journal rather than frozen review package")
	}
}
func TestIndependentFinalControl(t *testing.T) {
	b, _, c, _ := independentFinal(t)
	if err := VerifyRollout(context.Background(), c, b, true); err != nil {
		t.Fatal(err)
	}
}
func TestIndependentMissingD5Evidence(t *testing.T) {
	for _, fault := range []string{"receipt", "all-phase-evidence"} {
		t.Run(fault, func(t *testing.T) {
			b, _, c, _ := independentFinal(t)
			s, _ := loadState(c)
			delete(s.Extra, "dropin:agent-b")
			if fault == "all-phase-evidence" {
				delete(s.Completed, "dropin:agent-b")
				if err := os.Remove(attemptPath(c, "phase:dropin:agent-b")); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
			err := VerifyRollout(context.Background(), c, b, true)
			t.Logf("missing %s final verify=%v", fault, err)
			if err == nil {
				t.Fatal("UNSAFE final verification accepted missing required D5 evidence")
			}
		})
	}
}
func TestIndependentOmarchyEnrollmentFirst(t *testing.T) {
	b, f, c, r := independentFinal(t)
	s, _ := loadState(c)
	for _, h := range c.ActiveWorkers {
		delete(s.Completed, "enroll-worker:"+h)
		for _, a := range []string{"enroll:" + h, "phase:enroll-worker:" + h} {
			if err := os.Remove(attemptPath(c, a)); err != nil {
				t.Fatal(err)
			}
		}
		if h == "agent-b" || h == "agent-a" {
			delete(s.Completed, "dropin:"+h)
			delete(s.Extra, "dropin:"+h)
			if err := os.Remove(attemptPath(c, "phase:dropin:"+h)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	b.Now = r.Now
	enrolled := false
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if strings.Contains(stdin, "worker enroll 'omarchy-pc'") {
			enrolled = true
		}
		p, e := f.Run(ctx, dir, stdin, name, args...)
		if name == "t3-steward" && len(args) > 0 && args[0] == "worker" && !enrolled && e == nil {
			var doc map[string]any
			if err := json.Unmarshal(p, &doc); err != nil {
				t.Fatal(err)
			}
			for _, row := range doc["workers"].([]any) {
				w := row.(map[string]any)
				if w["requirement"].(map[string]any)["workerId"] == "omarchy-pc" {
					w["enrolled"] = false
					w["state"] = "draining"
				}
			}
			return jsonBytes(doc), nil
		}
		return p, e
	})
	err := FinishWorker(context.Background(), c, b, "omarchy-pc", true)
	t.Logf("omarchy before D5=%v enrollment dispatched=%v calls=%v", err, enrolled, f.calls)
	if err == nil {
		t.Fatal("UNSAFE omarchy-pc enrollment completed before agent-b/agent-a D5")
	}
}
func TestIndependentWrongHostRecovery(t *testing.T) {
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err != nil {
		t.Fatal(err)
	}
	if err := RunHostWithRuntime(context.Background(), c, f, "normandy", true, io.Discard, r); err != nil {
		t.Fatal(err)
	}
	f.fail = "observe:homelab"
	if err := RunHostWithRuntime(context.Background(), c, f, "homelab", true, io.Discard, r); err == nil {
		t.Fatal("fault missed")
	}
	f.actions = nil
	err := RecoverHost(context.Background(), c, f, "agent-b", true)
	t.Logf("wrong host recovery=%v actions=%v", err, f.actions)
	if contains(f.actions, "rollback:agent-b") {
		t.Fatal("UNSAFE unrelated completed host rolled back after homelab failure")
	}
}
func TestIndependentFinalMissingConfigurationComponent(t *testing.T) {
	b, f, c, _ := independentFinal(t)
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		p, e := f.Run(ctx, dir, stdin, name, args...)
		if name == "upkeeper" && len(args) > 0 && args[0] == "status" && e == nil {
			var doc map[string]any
			if err := json.Unmarshal(p, &doc); err != nil {
				t.Fatal(err)
			}
			for _, row := range doc["hosts"].([]any) {
				row.(map[string]any)["components"] = []any{map[string]any{"name": "t3-steward", "state": "converged"}}
			}
			return jsonBytes(doc), nil
		}
		return p, e
	})
	err := VerifyRollout(context.Background(), c, b, true)
	t.Logf("full status with only t3-steward binary component=%v", err)
	if err == nil {
		t.Fatal("UNSAFE final status missing configuration components accepted")
	}
}
func TestIndependentAdapterWorkerEpoch(t *testing.T) {
	for _, kind := range []string{"activity", "haiku"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC()
			doc := diagnosticFixture(now)
			w := doc["workers"].([]any)[0].(map[string]any)
			w["state"] = "draining"
			w["requirement"].(map[string]any)["workerEpoch"] = "new"
			w["snapshot"].(map[string]any)["workerEpoch"] = "old"
			raw := &diagnosticRunner{workers: jsonBytes(doc), identity: []byte(`{"version":"backlog.admin/v1","kind":"coordinator-identity","coordinatorId":"homelab","health":"healthy","epoch":280}`)}
			b := NewProductionBackend()
			b.Now = func() time.Time { return now }
			b.Runner = raw
			c := Config{Targets: map[string]string{"agent-b": "remote"}, DrainedHosts: []string{"agent-b"}}
			got, err := b.Diagnostic(context.Background(), c, kind, "agent-b", 280)
			t.Logf("mismatched worker epoch got=%v err=%v", got, err)
			if err == nil {
				t.Fatal("UNSAFE wrong worker epoch accepted")
			}
		})
	}
}
func TestIndependentAdapterSlowFinalIdentity(t *testing.T) {
	for _, kind := range []string{"activity", "haiku"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC()
			doc := diagnosticFixture(now)
			snap := doc["workers"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)
			snap["observedAt"] = now.Add(-59 * time.Second)
			snap["validUntil"] = now.Add(time.Second)
			raw := &diagnosticRunner{workers: jsonBytes(doc), identity: []byte(`{"version":"backlog.admin/v1","kind":"coordinator-identity","coordinatorId":"homelab","health":"healthy","epoch":280}`)}
			b := NewProductionBackend()
			b.Now = func() time.Time { return now }
			reads := 0
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				p, e := raw.Run(ctx, dir, stdin, name, args...)
				if strings.Join(args, " ") == "coordinator identity --json" {
					reads++
					if reads == 2 {
						now = now.Add(2 * time.Second)
					}
				}
				return p, e
			})
			c := Config{Targets: map[string]string{"agent-b": "remote"}}
			got, err := b.Diagnostic(context.Background(), c, kind, "agent-b", 280)
			t.Logf("expired snapshot got=%v err=%v", got, err)
			if err == nil {
				t.Fatal("UNSAFE snapshot expired during final identity read accepted")
			}
		})
	}
}

func TestIndependentProductionWrongHostRecovery(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	s, _ := loadState(c)
	for _, h := range []string{"agent-b", "normandy"} {
		s.Extra["host-pre:"+h] = jsonBytes(f.snapshot(h))
		f.upgraded[h] = true
		s.Completed["host:"+h] = true
		reviewCompletedHost(t, c, h, r.Now())
		s.Extra["observation:"+h] = jsonBytes(ObservationReceipt{Host: h, ConfigDigest: c.Digest(), ReleaseCommit: s.ReleaseCommit, Started: r.Now().Add(-time.Hour), Finished: r.Now(), Samples: 361, Identity: jsonBytes(map[string]string{"epoch": "2"})})
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		p, e := f.Run(ctx, dir, stdin, name, args...)
		if name == "upkeeper" && len(args) > 2 && args[0] == "pull" && args[2] == "homelab" {
			return p, errors.New("homelab pull took effect then42")
		}
		return p, e
	})
	err := RunHostWithRuntime(context.Background(), c, b, "homelab", true, io.Discard, r)
	if err == nil || !strings.Contains(err.Error(), "rollback completed") {
		t.Fatal("failed homelab did not reach recovered terminal state", err)
	}
	f.calls = nil
	err = RecoverHost(context.Background(), c, b, "agent-b", true)
	t.Logf("unrelated production recovery=%v calls=%v", err, f.calls)
	for _, call := range f.calls {
		if strings.HasPrefix(call, "upkeeper rollback ") && strings.Contains(call, "--hosts agent-b") {
			t.Fatal("UNSAFE dispatched rollback for already-completed unrelated canary")
		}
	}
}
