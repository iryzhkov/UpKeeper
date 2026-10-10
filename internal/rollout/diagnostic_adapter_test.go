package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type diagnosticRunner struct {
	workers, identity []byte
	fail              string
	calls             []string
}

func (r *diagnosticRunner) Run(_ context.Context, _, _, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	var p []byte
	if call == "t3-steward worker list --json" {
		p = r.workers
	} else if call == "t3-steward coordinator identity --json" {
		p = r.identity
	} else {
		return nil, errors.New("unexpected mutation")
	}
	if strings.Contains(call, r.fail) && r.fail != "" {
		return p, errors.New("exit status 42")
	}
	return p, nil
}
func diagnosticFixture(now time.Time) map[string]any {
	return map[string]any{"version": "backlog.admin/v1", "kind": "workers", "workers": []any{map[string]any{
		"requirement": map[string]any{"workerId": "agent-b", "workerEpoch": "process"}, "health": "ready", "state": "observed", "stale": false,
		"snapshot": map[string]any{"workerId": "agent-b", "workerEpoch": "process", "coordinatorEpoch": 280, "connected": true, "observedAt": now.Add(-time.Second), "validUntil": now.Add(time.Minute),
			"inventory":   map[string]any{"id": "agent-b", "health": "ready", "capabilities": []string{"turn-end-commands-v1"}, "telemetry": map[string]any{"running_attempts": 0}, "providers": []any{map[string]any{"instanceId": "claudeAgent", "available": true, "models": []string{"claude-haiku-5-5"}}}},
			"assignments": []any{}}}}}
}

type diagnosticConsumerBridge struct {
	raw     *diagnosticRunner
	backend *ProductionBackend
	config  Config
}

func (r *diagnosticConsumerBridge) Run(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
	if name == "supported-haiku" {
		v, err := r.backend.Diagnostic(ctx, r.config, "haiku", args[0], 280)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	}
	return r.raw.Run(ctx, dir, stdin, name, args...)
}

// Exercise the real strict consumer rather than checking only adapter values.
func TestDiagnosticHaikuOutputConsumedByProductionAdvertisement(t *testing.T) {
	now := time.Now().UTC()
	doc := diagnosticFixture(now)
	doc["workers"].([]any)[0].(map[string]any)["state"] = "draining"
	p, _ := json.Marshal(doc)
	raw := &diagnosticRunner{workers: p, identity: []byte(`{"version":"backlog.admin/v1","kind":"coordinator-identity","coordinatorId":"homelab","health":"healthy","epoch":280}`)}
	b := NewProductionBackend()
	b.Now = func() time.Time { return now }
	c := Config{Targets: map[string]string{"agent-b": "remote"}, HaikuCheckCommand: []string{"supported-haiku"}}
	b.Runner = &diagnosticConsumerBridge{raw: raw, backend: b, config: c}
	if err := b.haikuAdvertisement(context.Background(), c, "agent-b", "280"); err != nil {
		t.Fatal(err)
	}
	if err := b.haikuAdvertisement(context.Background(), c, "agent-b", "281"); err == nil {
		t.Fatal("consumer accepted wrong epoch")
	}
}
func TestQuietOnlyDiagnosticSupportsDeclaredDrainedHosts(t *testing.T) {
	now := time.Now().UTC()
	doc := diagnosticFixture(now)
	doc["workers"].([]any)[0].(map[string]any)["state"] = "draining"
	raw := &diagnosticRunner{workers: jsonBytes(doc), identity: []byte(`{"version":"backlog.admin/v1","kind":"coordinator-identity","coordinatorId":"homelab","health":"healthy","epoch":280}`)}
	b := NewProductionBackend()
	b.Now = func() time.Time { return now }
	b.Runner = raw
	c := Config{Targets: map[string]string{"agent-b": "remote"}, DrainedHosts: []string{"agent-b"}}
	if _, err := b.Diagnostic(context.Background(), c, "activity", "agent-b", 280); err != nil {
		t.Fatal(err)
	}
	c.DrainedHosts = nil
	if _, err := b.Diagnostic(context.Background(), c, "activity", "agent-b", 280); err == nil {
		t.Fatal("undeclared drain accepted")
	}
}
func TestDiagnosticProducerJSONRefusesContradictoryEvidence(t *testing.T) {
	for _, p := range []string{
		`{"epoch":280,"epoch":281}`,
		`{"snapshot":{"connected":true,"connected":false}}`,
		`{"epoch":280} {"epoch":280}`,
		`{"epoch":`,
	} {
		var v diagnosticIdentity
		if err := decodeDiagnostic([]byte(p), &v); err == nil {
			t.Fatalf("accepted %s", p)
		}
	}
}

func TestDiagnosticRequiredEpochIndependentOfDraining(t *testing.T) {
	now := time.Now().UTC()
	for _, kind := range []string{"activity", "haiku"} {
		for _, defect := range []string{"valid", "mismatch", "missing-required", "empty-required", "missing-snapshot", "missing-id", "missing-coordinator", "duplicate", "capability"} {
			t.Run(kind+"/"+defect, func(t *testing.T) {
				d := diagnosticFixture(now)
				w := d["workers"].([]any)[0].(map[string]any)
				w["state"] = "draining"
				req := w["requirement"].(map[string]any)
				s := w["snapshot"].(map[string]any)
				switch defect {
				case "mismatch":
					req["workerEpoch"] = "new"
					s["workerEpoch"] = "old"
				case "missing-required":
					delete(req, "workerEpoch")
				case "empty-required":
					req["workerEpoch"] = ""
				case "missing-snapshot":
					delete(s, "workerEpoch")
				case "missing-id":
					delete(s, "workerId")
				case "missing-coordinator":
					delete(s, "coordinatorEpoch")
				case "duplicate":
					d["workers"] = []any{w, w}
				case "capability":
					s["inventory"].(map[string]any)["capabilities"] = []string{"older"}
				}
				raw := &diagnosticRunner{workers: jsonBytes(d), identity: []byte(`{"version":"backlog.admin/v1","kind":"coordinator-identity","coordinatorId":"homelab","health":"healthy","epoch":280}`)}
				b := NewProductionBackend()
				b.Runner = raw
				b.Now = func() time.Time { return now }
				got, err := b.Diagnostic(context.Background(), Config{Targets: map[string]string{"agent-b": "remote"}, DrainedHosts: []string{"agent-b"}}, kind, "agent-b", 280)
				if (err == nil) != (defect == "valid") || (err != nil && got != nil) {
					t.Fatalf("got=%v err=%v", got, err)
				}
				for _, call := range raw.calls {
					if call != "t3-steward worker list --json" && call != "t3-steward coordinator identity --json" {
						t.Fatalf("unexpected effect %s", call)
					}
				}
			})
		}
	}
}

func TestDiagnosticFinalReadFreshness(t *testing.T) {
	for _, kind := range []string{"activity", "haiku"} {
		for _, fault := range []string{"valid-boundary", "age", "lease", "age-and-lease", "clock-regression", "stale-assignment", "future-assignment", "future-journal", "final-producer-failure"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				initial := time.Now().UTC()
				now := initial
				d := diagnosticFixture(initial)
				s := d["workers"].([]any)[0].(map[string]any)["snapshot"].(map[string]any)
				advance := 2 * time.Second
				switch fault {
				case "valid-boundary":
					s["observedAt"] = initial.Add(-58 * time.Second)
				case "age":
					s["observedAt"] = initial.Add(-59 * time.Second)
				case "lease":
					s["validUntil"] = initial.Add(2 * time.Second)
				case "age-and-lease":
					s["observedAt"] = initial.Add(-59 * time.Second)
					s["validUntil"] = initial.Add(time.Second)
				case "clock-regression":
					advance = -time.Nanosecond
				case "stale-assignment", "future-assignment", "future-journal":
					observed := initial.Add(-59 * time.Second)
					updated := initial.Add(-time.Hour)
					if fault == "future-assignment" {
						observed = initial.Add(time.Minute)
					}
					if fault == "future-journal" {
						updated = initial.Add(time.Minute)
					}
					s["assignments"] = []any{map[string]any{"assignmentId": "done", "state": "completed", "observedAt": observed, "journal": map[string]any{"phase": "completed", "updatedAt": updated}}}
				}
				raw := &diagnosticRunner{workers: jsonBytes(d), identity: []byte(`{"version":"backlog.admin/v1","kind":"coordinator-identity","coordinatorId":"homelab","health":"healthy","epoch":280}`)}
				b := NewProductionBackend()
				b.Now = func() time.Time { return now }
				reads := 0
				b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
					p, e := raw.Run(ctx, dir, stdin, name, args...)
					if strings.Join(args, " ") == "coordinator identity --json" {
						reads++
						if reads == 2 {
							now = now.Add(advance)
							if fault == "final-producer-failure" {
								return p, errors.New("exit status 42")
							}
						}
					}
					return p, e
				})
				got, err := b.Diagnostic(context.Background(), Config{Targets: map[string]string{"agent-b": "remote"}}, kind, "agent-b", 280)
				if (err == nil) != (fault == "valid-boundary") || (err != nil && got != nil) {
					t.Fatalf("got=%v err=%v", got, err)
				}
			})
		}
	}
}

func TestProductionDiagnosticAdapters(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name, kind string
		change     func(map[string]any, map[string]any, map[string]any)
		fail, tail string
		want       bool
	}{
		{"quiet", "activity", nil, "", "", true}, {"haiku", "haiku", nil, "", "", true},
		{"supported-catalog-drift-haiku", "haiku", func(d, w, s map[string]any) {
			// workersResponse uses draining for both enrollment requirement drift and
			// worker inventory catalog drift; outer health still reflects ready inventory.
			w["state"] = "draining"
			w["enrolled"] = false
			w["requirement"] = map[string]any{"workerId": "agent-b", "workerEpoch": "process", "catalogRevision": "new-catalog"}
			w["enrollment"] = map[string]any{"request": map[string]any{"catalogRevision": "old-catalog"}}
			s["inventory"].(map[string]any)["catalogRevision"] = "old-catalog"
		}, "", "", true},
		{"catalog-drift-activity-refuses", "activity", func(d, w, s map[string]any) { w["state"] = "draining" }, "", "", false},
		{"invented-catalog-state-refuses", "haiku", func(d, w, s map[string]any) { w["state"] = "catalog-drift" }, "", "", false},
		{"terminal-history", "activity", func(d, w, s map[string]any) {
			s["assignments"] = []any{map[string]any{"assignmentId": "old", "state": "completed", "observedAt": now, "journal": map[string]any{"phase": "completed", "updatedAt": now.Add(-24 * time.Hour)}}}
		}, "", "", true},
		{"active-recent-progress", "activity", func(d, w, s map[string]any) {
			s["assignments"] = []any{map[string]any{"assignmentId": "live", "state": "claimed", "observedAt": now, "journal": map[string]any{"phase": "running", "updatedAt": now}}}
		}, "", "", false},
		{"unknown-assignment", "activity", func(d, w, s map[string]any) {
			s["assignments"] = []any{map[string]any{"assignmentId": "live", "state": "unknown"}}
		}, "", "", false},
		{"missing-journal", "activity", func(d, w, s map[string]any) {
			s["assignments"] = []any{map[string]any{"assignmentId": "old", "state": "released", "observedAt": now}}
		}, "", "", false},
		{"deferred", "activity", func(d, w, s map[string]any) {
			s["assignments"] = []any{map[string]any{"assignmentId": "old", "state": "completed", "observedAt": now, "journal": map[string]any{"phase": "completed", "updatedAt": now, "turnEnd": "collection deferred since " + now.Format(time.RFC3339) + ": background verification"}}}
		}, "", "", false},
		{"unknown-capability", "activity", func(d, w, s map[string]any) { s["inventory"].(map[string]any)["capabilities"] = []string{"older-peer"} }, "", "", false},
		{"missing-running-count", "activity", func(d, w, s map[string]any) { delete(s["inventory"].(map[string]any), "telemetry") }, "", "", false},
		{"stale", "activity", func(d, w, s map[string]any) { s["observedAt"] = now.Add(-2 * time.Minute) }, "", "", false},
		{"future", "haiku", func(d, w, s map[string]any) { s["observedAt"] = now.Add(time.Second) }, "", "", false},
		{"epoch", "haiku", func(d, w, s map[string]any) { s["coordinatorEpoch"] = 279 }, "", "", false},
		{"worker-missing", "haiku", func(d, w, s map[string]any) { w["requirement"] = map[string]any{"workerId": "agent-a"} }, "", "", false},
		{"duplicate-worker", "haiku", func(d, w, s map[string]any) { d["workers"] = []any{w, w} }, "", "", false},
		{"wildcard-authorization-only", "haiku", func(d, w, s map[string]any) {
			w["providers"] = []any{map[string]any{"models": []string{"*"}}}
			s["inventory"].(map[string]any)["providers"] = []any{}
		}, "", "", false},
		{"duplicate-provider", "haiku", func(d, w, s map[string]any) {
			i := s["inventory"].(map[string]any)
			p := i["providers"].([]any)[0]
			i["providers"] = []any{p, p}
		}, "", "", false},
		{"unavailable-provider", "haiku", func(d, w, s map[string]any) {
			s["inventory"].(map[string]any)["providers"].([]any)[0].(map[string]any)["available"] = false
		}, "", "", false},
		{"producer-success-then42", "activity", nil, "worker list", "", false},
		{"identity-success-then42", "haiku", nil, "coordinator identity", "", false},
		{"trailing-json", "activity", nil, "", " {}", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := diagnosticFixture(now)
			w := d["workers"].([]any)[0].(map[string]any)
			s := w["snapshot"].(map[string]any)
			if tt.change != nil {
				tt.change(d, w, s)
			}
			p, _ := json.Marshal(d)
			r := &diagnosticRunner{workers: append(p, tt.tail...), identity: []byte(`{"version":"backlog.admin/v1","kind":"coordinator-identity","coordinatorId":"homelab","health":"healthy","epoch":280}`), fail: tt.fail}
			b := NewProductionBackend()
			b.Runner = r
			b.Now = func() time.Time { return now }
			result, err := b.Diagnostic(context.Background(), Config{Targets: map[string]string{"agent-b": "remote"}}, tt.kind, "agent-b", 280)
			if (err == nil) != tt.want {
				t.Fatalf("result=%v err=%v", result, err)
			}
			if !tt.want && result != nil {
				t.Fatal("refusal produced success evidence")
			}
			if tt.want {
				p, _ := json.Marshal(result)
				if tt.kind == "activity" && string(p) != `{"host":"agent-b","coordinator_epoch":"280","collection_deferred":false,"running_without_activity":false}` {
					t.Fatal(string(p))
				}
			}
		})
	}
}
