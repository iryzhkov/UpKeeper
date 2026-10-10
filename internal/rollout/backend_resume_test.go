package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

type commandFunc func(context.Context, string, string, string, ...string) ([]byte, error)

func (f commandFunc) Run(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
	return f(ctx, dir, stdin, name, args...)
}
func TestOSRunnerPinsUpKeeperConfigurationToFencedCheckout(t *testing.T) {
	dir := t.TempDir()
	tool := dir + "/upkeeper"
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s' \"$DEV_FLEET_CONFIG_ROOT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEV_FLEET_CONFIG_ROOT", "/unrelated/installed/checkout")
	got, err := (OSRunner{}).Run(context.Background(), dir, "", "upkeeper")
	if err != nil || string(got) != dir {
		t.Fatalf("helper configuration was not fenced: %q %v", got, err)
	}
}
func TestProductionMissingReviewThenCIResumeCompletes(t *testing.T) {
	b, f, c, _ := backendFixture(t)
	c.ReviewFile = c.Checkout + "/external-review.md"
	f.c = c
	f.saved = true
	if err := Run(context.Background(), c, b, true, io.Discard); err == nil {
		t.Fatal("missing review accepted")
	}
	s, err := loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if s.InFlight != "publish" || s.Extra["publication-started"] != nil || f.pushes != 0 {
		t.Fatal("review refusal recorded publication")
	}
	review := "VERDICT: ACCEPT\n" + s.ExpectHead + "\n" + s.ReleaseSHA256 + "\nPACKAGE " + s.PackageDigest + "\n"
	if err = os.WriteFile(c.ReviewFile, []byte(review), 0600); err != nil {
		t.Fatal(err)
	}
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		p, e := f.Run(ctx, dir, stdin, name, args...)
		if name == "upkeeper" && args[0] == "push" {
			f.ciBad = true
		}
		return p, e
	})
	if err = Run(context.Background(), c, b, true, io.Discard); err == nil {
		t.Fatal("pending release CI accepted")
	}
	s, err = loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Completed["publish"] || s.Completed["release-ci"] || f.pushes != 1 {
		t.Fatal("publication not separately journaled")
	}
	// Soak clock is orchestration-owned; simulate the elapsed operator maintenance
	// window without sleeping or performing a network request.
	raw, _ := json.Marshal(time.Now().Add(-31 * time.Minute))
	s.Extra["soak-start"] = raw
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	f.ciBad = false
	b.Runner = f
	if err = Run(context.Background(), c, b, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, err = loadState(c)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Completed["verify"] || f.pushes != 1 {
		t.Fatal("resume did not complete without duplicate publication")
	}
}
func TestProductionInterruptedPrepareAndReachableFenceResume(t *testing.T) {
	b, f, c, s := backendFixture(t)
	fencedFake(f)
	if err := b.prepare(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	before := s.PackageDigest
	if err := b.prepare(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	authors := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, "upkeeper author") && !strings.Contains(call, "--dry-run") {
			authors++
		}
	}
	if authors != 1 || s.PackageDigest != before {
		t.Fatal("resumed prepare reauthored or changed reviewed bytes")
	}
	s.TimerPrior = map[string]string{c.LaptopHost: "unreachable"}
	if err := b.fence(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	if s.TimerPrior[c.LaptopHost] == "unreachable" {
		t.Fatal("reachable laptop stopped without recording actual prior state")
	}
}
func TestProductionPartialPublicationReceiptSurvives(t *testing.T) {
	b, f, c, s := backendFixture(t)
	fencedFake(f)
	if err := b.prepare(context.Background(), c, s); err != nil {
		t.Fatal(err)
	}
	writeReview(t, &c, s)
	b.Runner = commandFunc(func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, error) {
		if name == "upkeeper" && args[0] == "push" {
			return []byte("origin pushed; mirror refused"), errors.New("partial remote failure")
		}
		return f.Run(ctx, dir, stdin, name, args...)
	})
	if err := b.publish(context.Background(), c, s); err == nil {
		t.Fatal("partial publish accepted")
	}
	if s.Extra["publication-started"] == nil || !strings.Contains(string(s.Extra["push-receipt"]), "mirror refused") || s.Extra["push-error"] == nil {
		t.Fatal("partial receipt/reconciliation evidence lost")
	}
}
