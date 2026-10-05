package agent

import (
	"errors"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func TestT3MutationBoundaryGuardPreservesBytesAndBackup(t *testing.T) {
	_, tx, stage, state := t3TxPrepare(t, true)
	tx.guard = func() error { return errT3BoundaryIdle }
	if e := tx.activate(stage, state); !errors.Is(e, errT3BoundaryIdle) {
		t.Fatalf("activation not fenced: %v", e)
	}
	if tx.packageMoved || tx.packageActivated || tx.linkActivated || tx.stateMoved || tx.stateActivated {
		t.Fatal("mutation before final idle proof")
	}
	if e := tx.unchanged(); e != nil {
		t.Fatal(e)
	}
	tx.guard = nil
	if e := tx.activate(stage, state); e != nil {
		t.Fatal(e)
	}
	current, e := release.T3TreeDigest(tx.packagePath)
	if e != nil {
		t.Fatal(e)
	}
	tx.guard = func() error { return errT3BoundaryIdle }
	if e = tx.restore(); !errors.Is(e, errT3BoundaryIdle) {
		t.Fatalf("busy rollback not fenced: %v", e)
	}
	after, e := release.T3TreeDigest(tx.packagePath)
	if e != nil || after != current {
		t.Fatal("busy restoration replaced installation")
	}
	tx.guard = nil
	if e = tx.restore(); e != nil {
		t.Fatal(e)
	}
}
func TestT3CheckOnlyBoundaryNeedsExplicitSuccessfulZero(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		code         int
		want         bool
	}{
		{"zero", "shell snapshot: 4 threads, 0 running", 0, true},
		{"busy", "shell snapshot: 4 threads, 1 running", 0, false},
		{"missing", "T3 server 0.0.45 serving", 0, false},
		{"failed-zero", "shell snapshot: 4 threads, 0 running", 1, false},
		{"malformed", "shell snapshot: 4 threads, unknown running", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &adapterTestRunner{}
			c := adapterTestContext(t, runner)
			c.Components["t3-steward"] = stewardTestSpec(false)
			runner.handle = func(cmd procx.Command) procx.Result { return procx.Result{Code: tc.code, Stdout: tc.output} }
			if got := t3BoundaryIdle(c); got != tc.want {
				t.Fatalf("got %t", got)
			}
		})
	}
}
