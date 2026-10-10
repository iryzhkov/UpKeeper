package rollout

import (
	"context"
	"reflect"
	"testing"
	"time"
	"unsafe"
)

// Synthesize a system wall-clock step while retaining an advancing monotonic
// reading, as time.Now does in production. Only test fixture time is modified.
func independentWallStep(t *testing.T, initial time.Time, wallDelta, monotonicDelta time.Duration) time.Time {
	t.Helper()
	result := initial.Add(wallDelta)
	initialFields := reflect.ValueOf(&initial).Elem()
	resultFields := reflect.ValueOf(&result).Elem()
	wall := initialFields.FieldByName("wall").Uint()
	if wall>>63 != 1 {
		t.Fatal("time.Now has no monotonic reading")
	}
	ext := resultFields.FieldByName("ext")
	reflect.NewAt(ext.Type(), unsafe.Pointer(ext.UnsafeAddr())).Elem().SetInt(initialFields.FieldByName("ext").Int() + int64(monotonicDelta))
	if !result.Before(initial) && result.Round(0).Before(initial.Round(0)) {
		return result
	}
	t.Fatalf("invalid wall-step fixture: initial=%v final=%v", initial, result)
	return time.Time{}
}

func TestIndependentDiagnosticMonotonicWallRegression(t *testing.T) {
	for _, kind := range []string{"activity", "haiku"} {
		for _, representation := range []string{"monotonic", "wall-only"} {
			for _, fault := range []bool{false, true} {
				name := kind + "/" + representation + "/positive"
				if fault {
					name = kind + "/" + representation + "/wall-regression"
				}
				t.Run(name, func(t *testing.T) {
					initial := time.Now()
					final := initial.Add(2 * time.Second)
					if fault {
						final = independentWallStep(t, initial, -500*time.Millisecond, 2*time.Second)
					}
					if representation == "wall-only" {
						initial, final = initial.Round(0), final.Round(0)
					}
					raw := &diagnosticRunner{workers: jsonBytes(diagnosticFixture(initial)), identity: []byte(`{"version":"backlog.admin/v1","kind":"coordinator-identity","coordinatorId":"homelab","health":"healthy","epoch":280}`)}
					b := NewProductionBackend()
					b.Runner = raw
					reads := 0
					b.Now = func() time.Time {
						reads++
						if reads == 1 {
							return initial
						}
						return final
					}
					got, err := b.Diagnostic(context.Background(), Config{Targets: map[string]string{"agent-b": "remote"}}, kind, "agent-b", 280)
					t.Logf("initial=%v final=%v monotonicElapsed=%v wallDelta=%v", initial, final, final.Sub(initial), final.Round(0).Sub(initial.Round(0)))
					if fault && (err == nil || got != nil) {
						t.Fatalf("accepted real-clock wall regression: got=%v err=%v", got, err)
					}
					if !fault && (err != nil || got == nil) {
						t.Fatalf("positive control refused: got=%v err=%v", got, err)
					}
				})
			}
		}
	}
}
