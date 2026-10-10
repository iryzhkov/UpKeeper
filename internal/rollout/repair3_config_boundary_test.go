package rollout

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRepair3ConfigurationRecoveryBoundary(t *testing.T) {
	for _, fault := range []string{"missing", "restore-failure", "verification-failure", "receipt-save-failure", "valid"} {
		t.Run(fault, func(t *testing.T) {
			b, c, s, _ := configFixture(t)
			if err := b.configPatch(context.Background(), c, s); err != nil {
				t.Fatal(err)
			}
			original := b.Runner
			restored, checked := false, false
			b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
				mode := strings.Join(args, " ")
				if stdin == configTransactionScript && strings.Contains(mode, "restore") {
					if fault == "restore-failure" {
						return nil, errors.New("restore before effect failed")
					}
					restored = true
				}
				if stdin == configTransactionScript && strings.Contains(mode, "before") {
					if !restored {
						t.Fatal("verification before restoration")
					}
					if fault == "verification-failure" {
						return nil, errors.New("original verification failed")
					}
					checked = true
				}
				return original.Run(ctx, dir, stdin, name, args...)
			})
			if fault == "missing" {
				delete(s.Extra, "config-intent")
			}
			if fault == "receipt-save-failure" {
				saves := 0
				s.Save = func() error {
					saves++
					if saves == 2 {
						return errors.New("restored receipt not durable")
					}
					return nil
				}
			}
			err := b.configRestore(context.Background(), c, s)
			if (err == nil) != (fault == "valid") {
				t.Fatalf("fault=%s err=%v", fault, err)
			}
			if fault == "missing" && (restored || checked) {
				t.Fatal("missing receipt dispatched recovery")
			}
			if fault == "valid" && (!restored || !checked) {
				t.Fatal("restore/verification absent")
			}
			if err != nil {
				var receipt configReceipt
				if fault != "missing" {
					if e := evidence(s, "config-intent", &receipt); e != nil || receipt.Restored {
						t.Fatal("failure accepted as restored", e, receipt)
					}
				}
			}
		})
	}
}
