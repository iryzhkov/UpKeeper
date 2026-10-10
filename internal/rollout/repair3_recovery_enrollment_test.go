package rollout

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func repair3OrderedProductionD5(t *testing.T) (*ProductionBackend, *backendFake, Config, Runtime) {
	t.Helper()
	b, f, c, r := productionHostFixture(t)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	seedD5Observed(t, c, s, r.Now())
	// Original immutable generation is sampled before the upgraded fixture
	// state. Production rollback must present this exact generation afterward.
	for _, h := range rc122Order {
		s.Extra["host-pre:"+h] = jsonBytes(f.snapshot(h))
	}
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	for _, h := range append(append([]string{}, rc122Order...), c.DrainedHosts...) {
		f.upgraded[h] = true
	}
	enrolled := map[string]bool{}
	var effects []string
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if strings.Contains(stdin, "worker enroll") {
			for _, h := range d5Order {
				if strings.Contains(stdin, quote(h)) {
					enrolled[h] = true
					effects = append(effects, "enroll:"+h)
					return nil, nil
				}
			}
		}
		if stdin == dropinScript {
			if strings.Contains(strings.Join(args, " "), "inspect") {
				return jsonBytes(dropinReceipt{SHA256: strings.Repeat("b", 64), Mode: 0600, UID: 1000, GID: 1000}), nil
			}
			effects = append(effects, "dropin:"+f.host(args))
			return nil, nil
		}
		if name == c.HaikuCheckCommand[0] {
			return jsonBytes(map[string]string{"worker_id": args[len(args)-2], "coordinator_epoch": args[len(args)-1], "model": "claudeAgent/claude-haiku-5-5"}), nil
		}
		if strings.Contains(stdin, strings.Join(quoted(c.DropinTestCommand), " ")) {
			return nil, nil
		}
		raw, err := f.Run(ctx, dir, stdin, name, args...)
		if err == nil && name == "t3-steward" && len(args) > 0 && args[0] == "worker" {
			var payload struct{ Workers []worker }
			if err = json.Unmarshal(raw, &payload); err != nil {
				return nil, err
			}
			for i := range payload.Workers {
				w := &payload.Workers[i]
				if contains(c.ActiveWorkers, w.Requirement.WorkerID) && !enrolled[w.Requirement.WorkerID] {
					w.State = "draining"
					w.Health = "ready"
					w.Enrollment.Request.CatalogRevision = json.RawMessage("0")
					w.Enrolled = false
				}
			}
			return jsonBytes(payload), nil
		}
		return raw, err
	})
	for _, h := range d5Order {
		if err = FinishWorker(context.Background(), c, b, h, true); err != nil {
			t.Fatalf("actual complete ordered D5 %s: %v", h, err)
		}
	}
	want := []string{"enroll:agent-b", "dropin:agent-b", "enroll:agent-a", "dropin:agent-a", "enroll:omarchy-pc"}
	if strings.Join(effects, ",") != strings.Join(want, ",") {
		t.Fatal("production effect order", effects)
	}
	s, err = loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = requiredD5(c, s, c.ActiveWorkers, r.Now()); err != nil {
		t.Fatal("complete D5 original receipts", err)
	}
	return b, f, c, r
}

func TestRepair3NonDropinEnrollmentFinalAckRecovery(t *testing.T) {
	b, f, c, r := repair3OrderedProductionD5(t)
	host := "omarchy-pc"
	independentLoseFinalAck(t, c, "enroll:"+host)
	noProductionContinuation(t, b, f, c, r)
	f.calls = nil
	if err := RecoverHost(context.Background(), c, b, "agent-a", true); err == nil || len(f.calls) != 0 {
		t.Fatal("historical other host authorized", err, f.calls)
	}
	for i := 1; i <= 2; i++ {
		f.calls = nil
		err := RecoverHost(context.Background(), c, b, host, true)
		if err == nil {
			t.Fatal("recovery cleared terminal failure")
		}
		rollbacks := 0
		for _, call := range f.calls {
			if strings.HasPrefix(call, "upkeeper rollback ") {
				rollbacks++
				if !strings.Contains(call, "--hosts "+host+" ") {
					t.Fatal("unnamed host rollback", call)
				}
			}
		}
		if rollbacks != 1 {
			t.Fatal("exact final-enrollment recovery not dispatched once", err, f.calls)
		}
		s, err := loadState(c)
		if err != nil {
			t.Fatal(err)
		}
		if s.Failed == "" || s.FailedInvocation == nil || s.FailedInvocation.Host != host || s.FailedInvocation.Operation != "enroll-worker:"+host || len(s.FailedInvocation.RecoveryHistory) != i || !s.FailedInvocation.RecoveryHistory[i-1].Acknowledged {
			t.Fatal("terminal exact authority/history lost", s.FailedInvocation)
		}
		if _, done, err := readAttempt(c, attemptPath(c, "enroll:"+host), r.Now()); err != nil || done {
			t.Fatal("recovery rewrote enclosing intent", done, err)
		}
		if _, exists := s.Extra["dropin:"+host]; exists {
			t.Fatal("invented non-applicable drop-in receipt")
		}
		noProductionContinuation(t, b, f, c, r)
	}
}

func TestRepair3NonDropinEnrollmentFinalAckEvidenceFailures(t *testing.T) {
	for _, fault := range []string{"missing-snapshot", "corrupt-snapshot", "missing-original-digest", "missing-original-service", "missing-run-ack", "missing-observation-ack", "missing-final-phase-ack", "missing-observation-receipt", "wrong-observation-host", "missing-final-authority", "ack-present"} {
		t.Run(fault, func(t *testing.T) {
			b, f, c, r := repair3OrderedProductionD5(t)
			host := "omarchy-pc"
			if fault != "ack-present" {
				independentLoseFinalAck(t, c, "enroll:"+host)
			}
			s, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "missing-snapshot":
				delete(s.Extra, "host-pre:"+host)
			case "corrupt-snapshot":
				s.Extra["host-pre:"+host] = jsonBytes("corrupt snapshot")
			case "missing-original-digest", "missing-original-service":
				var pre map[string]string
				if err = evidence(s, "host-pre:"+host, &pre); err != nil {
					t.Fatal(err)
				}
				key := "binary_sha256"
				if fault == "missing-original-service" {
					key = "t3-steward-worker.service.MainPID"
				}
				delete(pre, key)
				s.Extra["host-pre:"+host] = jsonBytes(pre)
			case "missing-run-ack":
				independentLoseFinalAck(t, c, "run:"+host)
			case "missing-observation-ack":
				independentLoseFinalAck(t, c, "phase:observe:"+host)
			case "missing-final-phase-ack":
				independentLoseFinalAck(t, c, "phase:enroll-worker:"+host)
			case "missing-observation-receipt":
				delete(s.Extra, "observation:"+host)
			case "wrong-observation-host":
				var receipt ObservationReceipt
				if err = evidence(s, "observation:"+host, &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.Host = "agent-b"
				s.Extra["observation:"+host] = jsonBytes(receipt)
			case "missing-final-authority":
				s.Invocation.EnclosingAttempt = ""
				s.Invocation.EnclosingStarted = time.Time{}
			}
			if err = saveState(c.StatePath, s); err != nil {
				t.Fatal(err)
			}
			f.calls = nil
			err = RecoverHost(context.Background(), c, b, host, true)
			if err == nil || len(f.calls) != 0 {
				t.Fatal("missing exact final-enrollment evidence dispatched", err, f.calls)
			}
			if fault != "ack-present" {
				noProductionContinuation(t, b, f, c, r)
			}
			// No append is introduced by a refusal.
			entries, e := os.ReadDir(c.StatePath + ".attempts")
			if e != nil {
				t.Fatal(e)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "recovery-rollback-"+host+"-") {
					t.Fatal("refusal authored recovery intent", entry.Name())
				}
			}
		})
	}
}
