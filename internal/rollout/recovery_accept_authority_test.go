package rollout

import (
	"context"
	"io"
	"testing"
)

func TestRepair2AcceptanceFailureNeverCopiesHistoricalAuthority(t *testing.T) {
	c := rc122Fixture(t)
	r, _ := fakeRuntime()
	f := protocolBackend()
	if err := RunHostWithRuntime(context.Background(), c, f, "agent-b", true, io.Discard, r); err != nil {
		t.Fatal(err)
	}
	f.fail = "release-ci"
	if err := RunHostWithRuntime(context.Background(), c, f, "normandy", true, io.Discard, r); err == nil {
		t.Fatal("accept failure reported success")
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.FailedInvocation == nil || s.FailedInvocation.Host != "normandy" || s.FailedInvocation.Operation != "accept:normandy" || s.FailedInvocation.Attempt != "phase:accept:normandy" {
		t.Fatalf("historical authority copied: %+v", s.FailedInvocation)
	}
	f.actions = nil
	for _, host := range []string{"agent-b", "normandy"} {
		if err := RecoverHost(context.Background(), c, f, host, true); err == nil || len(f.actions) != 0 {
			t.Fatal("prehost failed acceptance dispatched recovery", err, f.actions)
		}
	}
}
