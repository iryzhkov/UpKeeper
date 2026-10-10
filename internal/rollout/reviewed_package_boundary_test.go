package rollout

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRepair2FrozenPackageSchemaAndRequiredReferences(t *testing.T) {
	faults := []string{"unknown-field", "case-conflict", "duplicate-field", "missing-timers", "timer-subset"}
	for _, key := range []string{"author-started", "release.diff", "release-before", "release-after", "fleet-plan", "preview-full", "preview-scoped", "preview-rollback", "assets", "rc118-findings"} {
		faults = append(faults, "missing:"+key)
	}
	for _, fault := range faults {
		t.Run(fault, func(t *testing.T) {
			b, f, c, _ := productionHostFixture(t)
			s, _ := loadState(c)
			raw, err := os.ReadFile(c.StatePath + ".review-package.json")
			if err != nil {
				t.Fatal(err)
			}
			var pkg map[string]any
			if err := json.Unmarshal(raw, &pkg); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "unknown-field":
				pkg["unreviewed-authority"] = true
			case "case-conflict":
				pkg["head"] = "conflicting"
			case "missing-timers":
				delete(pkg, "TimerPrior")
				s.TimerPrior = nil
			case "timer-subset":
				timers := pkg["TimerPrior"].(map[string]any)
				delete(timers, c.TimerHosts[0])
				delete(s.TimerPrior, c.TimerHosts[0])
			default:
				if strings.HasPrefix(fault, "missing:") {
					key := strings.TrimPrefix(fault, "missing:")
					delete(pkg["Evidence"].(map[string]any), key)
					delete(s.Extra, key)
				}
			}
			raw = jsonBytes(pkg)
			if fault == "duplicate-field" {
				raw = append(raw[:len(raw)-1], []byte(",\"Head\":\"duplicate\"}")...)
			}
			s.PackageDigest = digest(raw)
			if err := os.WriteFile(c.StatePath+".review-package.json", raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(c.ReviewFile, []byte("VERDICT: ACCEPT\n"+s.ExpectHead+"\n"+s.ReleaseSHA256+"\nPACKAGE "+s.PackageDigest+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := b.Execute(context.Background(), c, "host:"+c.Canary, s); err == nil || len(f.calls) > 0 {
				t.Fatalf("invalid frozen references accepted: %v calls=%v", err, f.calls)
			}
		})
	}
}
func TestRepair2FinalAndObservationRejectFrozenMutations(t *testing.T) {
	for _, key := range []string{"assets", "preview-full", "release-after", "fleet-plan", "rc118-findings"} {
		t.Run(key, func(t *testing.T) {
			b, f, c, _ := independentFinal(t)
			s, _ := loadState(c)
			s.Extra[key] = jsonBytes("wrong")
			f.calls = nil
			if err := b.Execute(context.Background(), c, "observe:"+c.Canary, s); err == nil {
				t.Fatal("observation accepted mutated authority")
			}
			if err := b.Execute(context.Background(), c, "verify", s); err == nil {
				t.Fatal("final acceptance used mutated authority")
			}
			if len(f.calls) != 0 {
				t.Fatalf("commands dispatched before immutable gate: %v", f.calls)
			}
		})
	}
}
