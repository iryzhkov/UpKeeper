package rollout

import (
	"context"
	"io"
	"testing"
)

func TestRepair2EnrollmentFailureDoesNotAuthorizeHistoricalBinaryRollback(t *testing.T) {
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err != nil {
		t.Fatal(err)
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = beginAttempt(c, "enroll:agent-b", "agent-b", r.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = beginAttempt(c, "phase:enroll-worker:agent-b", "", r.Now()); err != nil {
		t.Fatal(err)
	}
	s.Invocation = &InvocationAuthority{Host: "agent-b", Operation: "enroll-worker:agent-b", Attempt: "phase:enroll-worker:agent-b", ConfigDigest: c.Digest(), Started: r.Now()}
	failInvocation(s)
	s.Failed = "catalog enrollment effect uncertain"
	s.InFlight = ""
	if err = saveState(c.StatePath, s); err != nil {
		t.Fatal(err)
	}
	f.actions = nil
	if err = RecoverHost(context.Background(), c, f, "agent-b", true); err == nil || len(f.actions) != 0 {
		t.Fatal("catalog uncertainty authorized historical binary rollback", err, f.actions)
	}
}
