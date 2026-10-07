//go:build linux

package author

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// contextTestWrap replaces the private verification of a genuinely acquired
// owner under its lock; the wrapper always runs the real custody recheck.
func contextTestWrap(s *CatalogSnapshot, wrap func(real func(context.Context) error, ctx context.Context) error) (restore func()) {
	s.mu.Lock()
	real := s.verify
	s.verify = func(ctx context.Context) error { return wrap(real, ctx) }
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.verify = real
		s.mu.Unlock()
	}
}

// Context frontdoors return byte-identical audits to the legacy frontdoors
// under one sequential parent, and the acquisition context is not retained.
func TestCatalogContextActualParity(t *testing.T) {
	t.Parallel()
	o := snapshotFixture(t)
	acquisition, cancelAcquisition := context.WithCancel(context.Background())
	s, e := AcquireCatalogSnapshot(acquisition, o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	cancelAcquisition()

	parent, cancel := context.WithTimeout(context.Background(), catalogContextLimit)
	defer cancel()
	if e := s.VerifyContext(parent); e != nil {
		t.Fatalf("verification bound to acquisition context: %v", e)
	}
	legacyGuidance, e := InspectCatalogGuidanceSnapshot(s)
	if e != nil {
		t.Fatal(e)
	}
	guidance, e := InspectCatalogGuidanceSnapshotContext(parent, s)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(guidance, legacyGuidance) || len(guidance.VerifiedCategories()) != 9 || len(guidance.Blockers()) != 2 || len(guidance.FutureRequired()) != 8 {
		t.Fatal("guidance audit parity")
	}
	legacyProducer, e := InspectCatalogProducerSnapshot(s)
	if e != nil {
		t.Fatal(e)
	}
	producer, e := InspectCatalogProducerSnapshotContext(parent, s)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(producer, legacyProducer) || len(producer.VerifiedCategories()) != 7 || len(producer.Blockers()) != 2 || len(producer.FutureRequired()) != 10 {
		t.Fatal("producer audit parity")
	}
	// Returned encodings stay defensive copies through the context frontdoor.
	guidance.FullCatalogEncoding()[0] = '!'
	producer.ReceiptEncoding()[0] = '!'
	if guidance.FullCatalogEncoding()[0] == '!' || producer.ReceiptEncoding()[0] == '!' || s.InputBytes("inputs/tool-catalog.json")[0] == '!' {
		t.Fatal("mutable audit")
	}
	if e := s.Verify(); e != nil {
		t.Fatal(e)
	}
}

// Real held custody changes are refused by every context and legacy frontdoor.
func TestCatalogContextActualMutations(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*testing.T, CatalogSnapshotOptions){
		"input": func(t *testing.T, o CatalogSnapshotOptions) {
			p := filepath.Join(o.InputRoot, "inputs/guidance-profile.json")
			snapshotTestWrite(t, p, append(snapshotTestRead(t, p), ' '), 0600)
		},
		"source": func(t *testing.T, o CatalogSnapshotOptions) {
			p := filepath.Join(o.Repository, "shared/catalog-guidance/guidance/huyang.json")
			snapshotTestWrite(t, p, append(snapshotTestRead(t, p), ' '), 0644)
		},
		"config": func(t *testing.T, o CatalogSnapshotOptions) {
			p := filepath.Join(o.Repository, ".git/config")
			snapshotTestWrite(t, p, append(snapshotTestRead(t, p), "\n"...), 0644)
		},
		"head": func(t *testing.T, o CatalogSnapshotOptions) {
			snapshotTestWrite(t, filepath.Join(o.Repository, ".git/HEAD"), []byte("ref: refs/heads/elsewhere\n"), 0644)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := snapshotFixture(t)
			s := snapshotTestAcquire(t, o)
			if e := s.VerifyContext(context.Background()); e != nil {
				t.Fatal(e)
			}
			mutate(t, o)
			for _, f := range contextTestFrontdoors() {
				if empty, e := f.call(context.Background(), s); e == nil || !empty {
					t.Fatalf("%s accepted %s mutation", f.name, name)
				}
			}
			if e := s.Verify(); e == nil {
				t.Fatal("legacy verify accepted mutation")
			}
			if a, e := InspectCatalogGuidanceSnapshot(s); e == nil || a.VerifiedCategories() != nil {
				t.Fatal("legacy guidance accepted mutation")
			}
			if a, e := InspectCatalogProducerSnapshot(s); e == nil || a.VerifiedCategories() != nil {
				t.Fatal("legacy producer accepted mutation")
			}
		})
	}
}

// Cancellation observed after a real, successful recheck (the first one or
// the final one) still yields no audit.
func TestCatalogContextActualFinalStage(t *testing.T) {
	t.Parallel()
	s := snapshotTestAcquire(t, snapshotFixture(t))
	for _, f := range contextTestFrontdoors() {
		stages := []int{1, 2}
		if f.name == "verify" {
			stages = []int{1}
		}
		for _, stage := range stages {
			t.Run(f.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				restore := contextTestWrap(s, func(real func(context.Context) error, c context.Context) error {
					calls++
					e := real(c)
					if calls == stage {
						cancel()
					}
					return e
				})
				defer restore()
				empty, e := f.call(ctx, s)
				if !errors.Is(e, context.Canceled) || !empty || calls != stage {
					t.Fatalf("stage %d: calls %d: %v", stage, calls, e)
				}
			})
		}
	}
	if e := s.VerifyContext(context.Background()); e != nil {
		t.Fatal(e)
	}
}

// Cancellation lands at varying points of the real Git recheck. Whatever stage
// it interrupts, the real verification error is either nil (it finished
// first) or carries the cancellation, and the frontdoor returns no success.
func TestCatalogContextActualInFlightGit(t *testing.T) {
	s := snapshotTestAcquire(t, snapshotFixture(t))
	interrupted := 0
	for _, delay := range []time.Duration{0, time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond, 60 * time.Millisecond} {
		ctx, cancel := context.WithCancel(context.Background())
		var realErr error
		restore := contextTestWrap(s, func(real func(context.Context) error, c context.Context) error {
			timer := time.AfterFunc(delay, cancel)
			defer timer.Stop()
			realErr = real(c)
			// Hold until cancellation so the frontdoor outcome is deterministic.
			<-c.Done()
			return realErr
		})
		err := s.VerifyContext(ctx)
		restore()
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("delay %v: %v", delay, err)
		}
		if realErr != nil {
			interrupted++
			if !errors.Is(realErr, context.Canceled) {
				t.Fatalf("delay %v: cancellation lost in real verification: %v", delay, realErr)
			}
		}
	}
	if interrupted == 0 {
		t.Fatal("no delay interrupted the real verification")
	}
	if e := s.Verify(); e != nil {
		t.Fatal(e)
	}
}

// A real /usr/bin/git child blocked reading its configuration is killed through
// exec.CommandContext and reaped before the helper returns; the helper's
// integrity error is restored to the cancellation. This is one deterministic
// subprocess path, not timeout/kill/reap fault qualification.
func TestCatalogContextGitCommandCancellation(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if e := os.Mkdir(repo, 0700); e != nil {
		t.Fatal(e)
	}
	snapshotTestGit(t, repo, "init", "--quiet")
	config := filepath.Join(repo, ".git/config")
	if e := os.Remove(config); e != nil {
		t.Fatal(e)
	}
	if e := syscall.Mkfifo(config, 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- snapshotTreeObjects(ctx, CatalogSnapshotOptions{Repository: repo, Tree: strings.Repeat("a", 40)}, map[string]snapshotSpec{})
	}()
	// A nonblocking writer open succeeds only once git holds the FIFO for
	// reading; git then blocks reading because nothing is written.
	writer, deadline := -1, time.Now().Add(10*time.Second)
	for writer < 0 {
		fd, e := syscall.Open(config, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		switch {
		case e == nil:
			writer = fd
		case e != syscall.ENXIO:
			t.Fatal(e)
		case time.Now().After(deadline):
			t.Fatal("git never opened its configuration")
		default:
			select {
			case e := <-result:
				t.Fatalf("git exited before reading configuration: %v", e)
			case <-time.After(time.Millisecond):
			}
		}
	}
	defer syscall.Close(writer)
	select {
	case e := <-result:
		t.Fatalf("git returned while blocked on configuration: %v", e)
	default:
	}
	cancel()
	var err error
	select {
	case err = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled Git child not reaped")
	}
	if err == nil || !strings.Contains(err.Error(), "catalog Git tree object hash") {
		t.Fatalf("helper result %v", err)
	}
	if recovered := catalogContextErr(ctx, err); !errors.Is(recovered, context.Canceled) {
		t.Fatalf("cancellation not recovered: %v", recovered)
	}
}

// Context and legacy frontdoors, getters and Close share one owner without
// deadlock, partial audits or use after Close.
func TestCatalogContextActualConcurrency(t *testing.T) {
	t.Parallel()
	s := snapshotTestAcquire(t, snapshotFixture(t))
	var wg sync.WaitGroup
	started := make(chan struct{})
	var once sync.Once
	work := []func(i int){
		func(i int) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i)*time.Millisecond)
			defer cancel()
			_ = s.VerifyContext(ctx)
		},
		func(int) { _ = s.Verify() },
		func(int) {
			if a, e := InspectCatalogGuidanceSnapshotContext(context.Background(), s); e == nil && len(a.VerifiedCategories()) != 9 {
				t.Error("partial guidance audit")
			}
		},
		func(int) {
			if a, e := InspectCatalogGuidanceSnapshot(s); e == nil && len(a.VerifiedCategories()) != 9 {
				t.Error("partial legacy guidance audit")
			}
		},
		func(int) {
			if a, e := InspectCatalogProducerSnapshotContext(context.Background(), s); e == nil && len(a.VerifiedCategories()) != 7 {
				t.Error("partial producer audit")
			}
		},
		func(int) {
			if a, e := InspectCatalogProducerSnapshot(s); e == nil && len(a.VerifiedCategories()) != 7 {
				t.Error("partial legacy producer audit")
			}
		},
		func(int) {
			if b := s.InputBytes("inputs/guidance-profile.json"); b != nil {
				b[0] = '!'
			}
			_ = s.SourceBytes("shared/catalog-guidance/owner.json")
			_, _ = s.Identity()
		},
	}
	for _, w := range work {
		wg.Add(1)
		go func() {
			defer wg.Done()
			once.Do(func() { close(started) })
			for i := 1; i <= 2; i++ {
				w(i)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-started
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	}()
	wg.Wait()
	for _, f := range contextTestFrontdoors() {
		if empty, e := f.call(context.Background(), s); e == nil || !empty {
			t.Fatalf("%s after Close", f.name)
		}
	}
}
