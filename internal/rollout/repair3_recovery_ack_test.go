package rollout

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func independentLoseFinalAck(t *testing.T, c Config, action string) {
	t.Helper()
	p := attemptPath(c, action)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "complete:") {
		t.Fatal("positive control lacks completed final acknowledgement", string(raw))
	}
	if err = os.WriteFile(p, []byte(lines[0]+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestIndependentForwardRunFinalAckLossRecovery(t *testing.T) {
	b, f, c, r := productionHostFixture(t)
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == c.ActivityCommand[0] {
			no := false
			return jsonBytes(ActivityReceipt{Host: args[0], CoordinatorEpoch: args[1], CollectionDeferred: &no, RunningWithoutActivity: &no}), nil
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	if err := RunHostWithRuntime(context.Background(), c, b, c.Canary, true, io.Discard, r); err != nil {
		t.Fatal("positive complete production run", err)
	}
	independentLoseFinalAck(t, c, "run:"+c.Canary)
	noProductionContinuation(t, b, f, c, r)
	f.calls = nil
	err := RecoverHost(context.Background(), c, b, c.Canary, true)
	t.Logf("lost final run ack: recovery=%v calls=%v", err, f.calls)
	for _, call := range f.calls {
		if strings.HasPrefix(call, "upkeeper rollback ") && strings.Contains(call, "--hosts "+c.Canary) {
			return
		}
	}
	t.Fatal("exact unfinished invocation cannot recover after lost final run acknowledgement")
}

func TestIndependentForwardD5FinalAckLossRecovery(t *testing.T) {
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
	restored := false
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if stdin == dropinScript {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "inspect") {
				return jsonBytes(dropinReceipt{SHA256: strings.Repeat("b", 64), Mode: 0600, UID: 1000, GID: 1000}), nil
			}
			if strings.Contains(joined, "restore") {
				restored = true
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
	if err = FinishWorker(context.Background(), c, b, "agent-b", true); err != nil {
		t.Fatal("positive complete production D5", err)
	}
	s, err = loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = requiredD5(c, s, []string{"agent-b"}, r.Now()); err != nil {
		t.Fatal("positive valid original D5 receipt", err)
	}
	independentLoseFinalAck(t, c, "enroll:agent-b")
	noProductionContinuation(t, b, f, c, r)
	f.calls = nil
	err = RecoverHost(context.Background(), c, b, "agent-b", true)
	t.Logf("lost final enroll ack: recovery=%v restored=%v calls=%v", err, restored, f.calls)
	if !restored {
		t.Fatal("exact unfinished D5 cannot restore original after lost final enrollment acknowledgement")
	}
}
