package rollout

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// This is the independent review's reproduction, extended to prove the
// authorization is durable before the first fleet host is dispatched.
func TestReviewFleetApprovalAfterCanary(t *testing.T) {
	c := fixture(t)
	c.FleetApproval = ""
	f := &fakeBackend{}
	if err := Run(context.Background(), c, f, true, io.Discard); err == nil || !strings.Contains(err.Error(), "fleet approval") {
		t.Fatalf("expected fleet approval boundary, got %v", err)
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Completed["host:"+c.Canary] || !s.Completed["soak"] || !s.Completed["backup"] || s.FleetApproval != "" {
		t.Fatalf("wrong approval boundary: %+v", s)
	}
	for _, h := range c.FleetOrder {
		if s.Completed["host:"+h] {
			t.Fatalf("fleet host %s ran without approval", h)
		}
	}
	c.FleetApproval = "operator go after reviewing canary and soak"
	f.actions = nil
	f.before = func(a string, _ *State) {
		if a == "host:"+c.FleetOrder[0] {
			disk, err := loadState(c)
			if err != nil {
				t.Fatal(err)
			}
			if disk.FleetApproval != c.FleetApproval {
				t.Fatal("approval was not persisted before fleet dispatch")
			}
		}
	}
	if err := Run(context.Background(), c, f, true, io.Discard); err != nil {
		t.Fatalf("cannot resume after fleet approval: %v; actions=%v", err, f.actions)
	}
	if contains(f.actions, "host:"+c.Canary) || contains(f.actions, "backup") || contains(f.actions, "canary-observe") {
		t.Fatalf("repeated completed canary/soak/backup: %v", f.actions)
	}
}

func TestFleetApprovalResumeAuthorityAndDrift(t *testing.T) {
	c := fixture(t)
	c.FleetApproval = ""
	f := &fakeBackend{}
	_ = Run(context.Background(), c, f, true, io.Discard)
	before, err := os.ReadFile(c.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	c.FleetApproval = "operator go"
	f.actions = nil
	if err := Run(context.Background(), c, f, false, io.Discard); err == nil {
		t.Fatal("approval recorded without --yes")
	}
	after, err := os.ReadFile(c.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || len(f.actions) != 0 {
		t.Fatal("unauthorized resume changed state or backend")
	}
	c.Version = "0.11.0-rc.120"
	if err := Run(context.Background(), c, f, true, io.Discard); err == nil || !strings.Contains(err.Error(), "configuration differs") {
		t.Fatal(err)
	}
	if len(f.actions) != 0 {
		t.Fatal(f.actions)
	}
}

func TestRecordedFleetApprovalSurvivesResumeAndRejectsReplacement(t *testing.T) {
	c := fixture(t)
	f := &fakeBackend{fail: "release-ci"}
	// Stop at the second fleet host's CI gate, after authorization is recorded.
	f.before = func(a string, s *State) {
		if a == "release-ci" && s.Completed["host:"+c.FleetOrder[0]] {
			f.fail = a
		} else {
			f.fail = ""
		}
	}
	if err := Run(context.Background(), c, f, true, io.Discard); err == nil {
		t.Fatal("expected CI refusal")
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.FleetApproval != c.FleetApproval {
		t.Fatal("authorization missing")
	}
	original := c.FleetApproval
	c.FleetApproval = "different operator go"
	f.actions = nil
	if err := Run(context.Background(), c, f, true, io.Discard); err == nil || !strings.Contains(err.Error(), "approval differs") {
		t.Fatal(err)
	}
	if len(f.actions) != 0 {
		t.Fatal(f.actions)
	}
	c.FleetApproval = ""
	f.before = nil
	f.fail = ""
	if err := Run(context.Background(), c, f, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, err = loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.FleetApproval != original {
		t.Fatal("recorded approval was lost")
	}
}

func TestWhitespaceFleetApprovalRefuses(t *testing.T) {
	c := fixture(t)
	c.FleetApproval = " \n\t"
	f := &fakeBackend{}
	if err := Run(context.Background(), c, f, true, io.Discard); err == nil || !strings.Contains(err.Error(), "fleet approval") {
		t.Fatal(err)
	}
	for _, h := range c.FleetOrder {
		if contains(f.actions, "host:"+h) {
			t.Fatalf("host %s ran without explicit approval", h)
		}
	}
}
