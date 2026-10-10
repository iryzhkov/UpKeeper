package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func loseD5Ack(t *testing.T, c Config, action string) {
	t.Helper()
	p := attemptPath(c, action)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.SplitN(string(raw), "\n", 2)[0] + "\n"
	if err := os.WriteFile(p, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
}

func seedD5(t *testing.T, c Config, s *State, host string, now time.Time) {
	t.Helper()
	s.Completed["enroll-worker:"+host] = true
	reviewAcknowledged(t, c, "enroll:"+host, host, now)
	reviewAcknowledged(t, c, "phase:enroll-worker:"+host, "", now)
	if host == "agent-b" || host == "agent-a" {
		s.Completed["dropin:"+host] = true
		reviewAcknowledged(t, c, "phase:dropin:"+host, "", now)
		s.Extra["dropin:"+host] = jsonBytes(dropinReceipt{Host: host, SHA256: strings.Repeat("b", 64), Mode: 0600, UID: 1000, GID: 1000, Done: true, IntentAt: now})
	}
}

func seedD5Observed(t *testing.T, c Config, s *State, now time.Time) {
	t.Helper()
	for _, h := range rc122Order {
		s.Completed["host:"+h] = true
		reviewCompletedHost(t, c, h, now)
		s.Extra["observation:"+h] = jsonBytes(ObservationReceipt{Host: h, ConfigDigest: c.Digest(), ReleaseCommit: s.ReleaseCommit, Started: now.Add(-time.Hour), Finished: now, Samples: 361, Identity: jsonBytes(map[string]string{"epoch": "4"})})
	}
}

func TestRepair2D5MandatoryInventory(t *testing.T) {
	for _, fault := range []string{"none", "receipt-missing", "all-missing", "enroll-phase-ack", "enroll-ack", "dropin-ack", "completed-missing", "corrupt", "duplicate-field", "older-receipt", "unknown-field", "trailing", "restored", "not-done", "wrong-host", "future", "missing-time", "metadata", "unreviewed-absence", "reviewed-absence"} {
		t.Run(fault, func(t *testing.T) {
			_, _, c, s := backendFixture(t)
			c.ActiveWorkers = append([]string{}, d5Order...)
			c.Canary = "agent-b"
			c.CoordinatorHost = "normandy"
			if fault == "reviewed-absence" {
				c.DropinAbsentHosts = []string{"agent-b"}
			}
			now := time.Unix(10000, 0)
			for _, h := range c.ActiveWorkers {
				seedD5(t, c, s, h, now)
			}
			var r dropinReceipt
			if err := evidence(s, "dropin:agent-b", &r); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "receipt-missing":
				delete(s.Extra, "dropin:agent-b")
			case "all-missing":
				delete(s.Extra, "dropin:agent-b")
				delete(s.Completed, "dropin:agent-b")
				if err := os.Remove(attemptPath(c, "phase:dropin:agent-b")); err != nil {
					t.Fatal(err)
				}
			case "enroll-phase-ack", "enroll-ack", "dropin-ack":
				action := map[string]string{"enroll-phase-ack": "phase:enroll-worker:agent-b", "enroll-ack": "enroll:agent-b", "dropin-ack": "phase:dropin:agent-b"}[fault]
				loseD5Ack(t, c, action)
			case "completed-missing":
				delete(s.Completed, "dropin:agent-b")
			case "corrupt":
				s.Extra["dropin:agent-b"] = []byte("{")
			case "duplicate-field":
				raw := s.Extra["dropin:agent-b"]
				s.Extra["dropin:agent-b"] = append(raw[:len(raw)-1], []byte(",\"Host\":\"agent-b\"}")...)
			case "older-receipt":
				r.IntentAt = now.Add(-time.Second)
				s.Extra["dropin:agent-b"] = jsonBytes(r)
			case "unknown-field":
				s.Extra["dropin:agent-b"] = []byte(`{"unknown":true}`)
			case "trailing":
				s.Extra["dropin:agent-b"] = append(s.Extra["dropin:agent-b"], []byte("{}")...)
			case "restored":
				r.Restored = true
				s.Extra["dropin:agent-b"] = jsonBytes(r)
			case "not-done":
				r.Done = false
				s.Extra["dropin:agent-b"] = jsonBytes(r)
			case "wrong-host":
				r.Host = "agent-a"
				s.Extra["dropin:agent-b"] = jsonBytes(r)
			case "future":
				r.IntentAt = now.Add(time.Second)
				s.Extra["dropin:agent-b"] = jsonBytes(r)
			case "missing-time":
				r.IntentAt = time.Time{}
				s.Extra["dropin:agent-b"] = jsonBytes(r)
			case "metadata":
				r.SHA256 = "wrong"
				s.Extra["dropin:agent-b"] = jsonBytes(r)
			case "unreviewed-absence", "reviewed-absence":
				r.Absent = true
				r.SHA256 = ""
				r.Mode = 0
				r.UID = 0
				r.GID = 0
				s.Extra["dropin:agent-b"] = jsonBytes(r)
				if fault == "reviewed-absence" {
					c.DropinAbsentHosts = []string{"agent-b"}
				}
			}
			err := requiredD5(c, s, c.ActiveWorkers, now)
			good := fault == "none" || fault == "reviewed-absence"
			if (err == nil) != good {
				t.Fatalf("mandatory D5 %s: %v", fault, err)
			}
		})
	}
}

func TestRepair2D5ProductionNamedEnrollmentOrder(t *testing.T) {
	for _, host := range []string{"agent-a", "omarchy-pc"} {
		for _, fault := range []string{"none-completed", "missing-receipt", "deleted-phase-and-receipt", "ack-lost", "restored", "wrong-host"} {
			t.Run(host+"/"+fault, func(t *testing.T) {
				b, f, c, r := productionHostFixture(t)
				s, err := loadState(c)
				if err != nil {
					t.Fatal(err)
				}
				seedD5Observed(t, c, s, r.Now())
				if fault != "none-completed" {
					seedD5(t, c, s, "agent-b", r.Now())
					if host == "omarchy-pc" {
						seedD5(t, c, s, "agent-a", r.Now())
					}
					switch fault {
					case "missing-receipt":
						delete(s.Extra, "dropin:agent-b")
					case "deleted-phase-and-receipt":
						delete(s.Extra, "dropin:agent-b")
						delete(s.Completed, "dropin:agent-b")
						if err := os.Remove(attemptPath(c, "phase:dropin:agent-b")); err != nil {
							t.Fatal(err)
						}
					case "ack-lost":
						loseD5Ack(t, c, "phase:dropin:agent-b")
					case "restored", "wrong-host":
						var receipt dropinReceipt
						if err := evidence(s, "dropin:agent-b", &receipt); err != nil {
							t.Fatal(err)
						}
						if fault == "restored" {
							receipt.Restored = true
						} else {
							receipt.Host = "agent-a"
						}
						s.Extra["dropin:agent-b"] = jsonBytes(receipt)
					}
				}
				if err = s.Save(); err != nil {
					t.Fatal(err)
				}
				if err = FinishWorker(context.Background(), c, b, host, true); err == nil {
					t.Fatal("out-of-order D5 accepted")
				}
				if len(f.calls) != 0 {
					t.Fatalf("commands before required D5: %v", f.calls)
				}
			})
		}
	}
}

func TestRepair2D5ProductionFinalMissingReceiptZeroCommands(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	seedD5Observed(t, c, s, r.Now())
	for _, h := range c.ActiveWorkers {
		seedD5(t, c, s, h, r.Now())
	}
	delete(s.Extra, "dropin:agent-b")
	delete(s.Completed, "dropin:agent-b")
	if err = os.Remove(attemptPath(c, "phase:dropin:agent-b")); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	if err = VerifyRollout(context.Background(), c, b, true); err == nil {
		t.Fatal("missing declared D5 accepted")
	}
	if err = b.verify(context.Background(), c, s); err == nil {
		t.Fatal("direct production verification omitted D5")
	}
	if len(f.calls) != 0 {
		t.Fatalf("commands before D5 proof: %v", f.calls)
	}
}

func TestRepair2D5ProductionCompleteOrder(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
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
		if err == nil && name == "t3-steward" && args[0] == "worker" {
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
			t.Fatalf("valid named production order %s: %v", h, err)
		}
	}
	want := []string{"enroll:agent-b", "dropin:agent-b", "enroll:agent-a", "dropin:agent-a", "enroll:omarchy-pc"}
	if strings.Join(effects, ",") != strings.Join(want, ",") {
		t.Fatalf("effects order: %v", effects)
	}
	s, err = loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = requiredD5(c, s, c.ActiveWorkers, r.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestRepair2D5IntentSaveFailureZeroProductionCommands(t *testing.T) {
	for _, action := range []string{"enroll-worker:agent-b", "dropin:agent-b"} {
		t.Run(action, func(t *testing.T) {
			b, f, c, r := productionHostFixture(t)
			s, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			s.Save = func() error { return errors.New("D5 intent directory fsync failure") }
			if err = executeSaved(context.Background(), c, b, s, action, r.Now()); err == nil {
				t.Fatal("D5 intent persistence failure accepted")
			}
			if len(f.calls) != 0 {
				t.Fatalf("D5 command before durable intent: %v", f.calls)
			}
			if err = requireAttempt(c, "phase:"+action, r.Now()); err == nil {
				t.Fatal("failed durable intent acknowledged")
			}
		})
	}
}

func TestRepair2D5PriorOrderPositive(t *testing.T) {
	_, _, c, s := backendFixture(t)
	c.ActiveWorkers = append([]string{}, d5Order...)
	c.Canary = "agent-b"
	c.CoordinatorHost = "normandy"
	now := time.Unix(10000, 0)
	for _, host := range d5Order {
		if err := priorD5(c, s, host, now); err != nil {
			t.Fatalf("valid order %s: %v", host, err)
		}
		seedD5(t, c, s, host, now)
	}
	if err := requiredD5(c, s, c.ActiveWorkers, now); err != nil {
		t.Fatal(err)
	}
	if validPhaseIntent("phase:dropin:normandy", "") || validPhaseIntent("phase:enroll-worker:homelab", "") {
		t.Fatal("undeclared D5 phase accepted")
	}
}
