package author

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// contextTestOwner is a package-private owner with a complete source26/input13
// inventory shape, so every frontdoor reaches its first verification. Its
// verify callback stands in for custody rechecks; it is never a public owner.
func contextTestOwner(verify func(context.Context) error) *CatalogSnapshot {
	s := &CatalogSnapshot{sources: map[string][]byte{}, inputs: map[string][]byte{},
		commit: strings.Repeat("a", 40), tree: strings.Repeat("b", 40), verify: verify, close: func() error { return nil }}
	for p := range snapshotSourceSpecs() {
		s.sources[p] = []byte("x")
	}
	for p := range snapshotInputSpecs() {
		s.inputs[p] = []byte("x")
	}
	return s
}

type contextTestFrontdoor struct {
	name string
	call func(context.Context, *CatalogSnapshot) (empty bool, err error)
}

func contextTestFrontdoors() []contextTestFrontdoor {
	return []contextTestFrontdoor{
		{"verify", func(ctx context.Context, s *CatalogSnapshot) (bool, error) {
			return true, s.VerifyContext(ctx)
		}},
		{"guidance", func(ctx context.Context, s *CatalogSnapshot) (bool, error) {
			a, e := InspectCatalogGuidanceSnapshotContext(ctx, s)
			c, t := a.Identity()
			return c == "" && t == "" && a.VerifiedCategories() == nil && a.Blockers() == nil && a.FutureRequired() == nil && a.FullCatalogEncoding() == nil && a.ProfileEncoding() == nil, e
		}},
		{"producer", func(ctx context.Context, s *CatalogSnapshot) (bool, error) {
			a, e := InspectCatalogProducerSnapshotContext(ctx, s)
			c, t := a.Identity()
			pc, pt := a.HistoricalProducerIdentity()
			return c == "" && t == "" && pc == "" && pt == "" && a.VerifiedCategories() == nil && a.Blockers() == nil && a.FutureRequired() == nil && a.FullCatalogEncoding() == nil && a.InputEncoding() == nil && a.ReceiptEncoding() == nil, e
		}},
	}
}

func TestCatalogContextFrontdoorRefusals(t *testing.T) {
	var calls atomic.Int32
	counting := func(context.Context) error { calls.Add(1); return nil }
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	closed := contextTestOwner(counting)
	if e := closed.Close(); e != nil {
		t.Fatal(e)
	}
	var nilContext context.Context
	cases := []struct {
		name  string
		ctx   context.Context
		owner *CatalogSnapshot
		want  error
	}{
		{"nil-context", nilContext, contextTestOwner(counting), nil},
		{"nil-owner", context.Background(), nil, nil},
		{"zero-owner", context.Background(), &CatalogSnapshot{}, nil},
		{"closed-owner", context.Background(), closed, nil},
		{"pre-cancelled", cancelled, contextTestOwner(counting), context.Canceled},
		{"pre-expired", expired, contextTestOwner(counting), context.DeadlineExceeded},
	}
	for _, f := range contextTestFrontdoors() {
		for _, c := range cases {
			t.Run(f.name+"/"+c.name, func(t *testing.T) {
				before := calls.Load()
				empty, e := f.call(c.ctx, c.owner)
				if e == nil || !empty {
					t.Fatalf("refusal returned success or partial audit: %v", e)
				}
				if c.want != nil && !errors.Is(e, c.want) {
					t.Fatalf("lost context error: %v", e)
				}
				if calls.Load() != before {
					t.Fatal("refused frontdoor started verification")
				}
			})
		}
	}
	// The incomplete fake inventory is refused by both semantic bodies even
	// after verification succeeds, so the zero audit is not an artifact of ctx.
	for _, f := range contextTestFrontdoors()[1:] {
		if empty, e := f.call(context.Background(), contextTestOwner(counting)); e == nil || !empty {
			t.Fatalf("%s accepted synthetic inventory", f.name)
		}
	}
	if runtime.GOOS != "linux" {
		if s, e := acquireCatalogSnapshot(context.Background(), CatalogSnapshotOptions{}, nil, nil); e == nil || s != nil {
			t.Fatal("non-Linux physical acquisition")
		}
	}
}

// A held owner lock is never acquired by a cancelled or expired waiter, and
// no verification starts. The owner remains usable after the holder unlocks.
func TestCatalogContextHeldLock(t *testing.T) {
	for _, f := range contextTestFrontdoors() {
		t.Run(f.name, func(t *testing.T) {
			var calls atomic.Int32
			s := contextTestOwner(func(context.Context) error { calls.Add(1); return nil })
			s.mu.Lock()

			const wait = 30 * time.Millisecond
			// Measure before creating the timeout so setup time is included.
			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			empty, e := f.call(ctx, s)
			elapsed := time.Since(start)
			cancel()
			if !errors.Is(e, context.DeadlineExceeded) || !empty {
				t.Fatalf("held lock deadline: %v", e)
			}
			// The deadline itself ends the wait, so it cannot return earlier.
			// No upper bound: a loaded host may schedule the waiter late, and
			// the DeadlineExceeded result above already proves the deadline ended it.
			if elapsed < wait {
				t.Fatalf("held lock wait %v", elapsed)
			}

			ctx, cancel = context.WithCancel(context.Background())
			started, result := make(chan struct{}), make(chan error, 1)
			go func() {
				close(started)
				_, e := f.call(ctx, s)
				result <- e
			}()
			<-started
			cancel()
			select {
			case e := <-result:
				if !errors.Is(e, context.Canceled) {
					t.Fatalf("held lock cancel: %v", e)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("cancelled waiter did not return while lock held")
			}
			if calls.Load() != 0 {
				t.Fatal("verification started without the owner lock")
			}
			s.mu.Unlock()
			if e := s.VerifyContext(context.Background()); e != nil || calls.Load() != 1 {
				t.Fatalf("owner unusable after unlock: %v", e)
			}
			if e := s.Close(); e != nil {
				t.Fatal(e)
			}
		})
	}
}

// Cancellation reaches an in-flight verification, which completes before the
// frontdoor returns. A replaced (integrity) error keeps errors.Is for ctx.
func TestCatalogContextInFlightVerification(t *testing.T) {
	for _, f := range contextTestFrontdoors() {
		for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
			t.Run(f.name+"/"+want.Error(), func(t *testing.T) {
				entered, finished := make(chan struct{}), make(chan struct{})
				s := contextTestOwner(func(ctx context.Context) error {
					close(entered)
					<-ctx.Done()
					close(finished)
					return errors.New("catalog Git tree object hash")
				})
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx := parent
				if want == context.DeadlineExceeded {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(parent, 50*time.Millisecond)
					defer stop()
				}
				result := make(chan error, 1)
				var empty bool
				go func() {
					var e error
					empty, e = f.call(ctx, s)
					result <- e
				}()
				<-entered
				if want == context.Canceled {
					cancel()
				}
				var e error
				select {
				case e = <-result:
				case <-time.After(10 * time.Second):
					t.Fatal("in-flight verification ignored cancellation")
				}
				select {
				case <-finished:
				default:
					t.Fatal("frontdoor returned before verification completed")
				}
				if !errors.Is(e, want) || !strings.Contains(e.Error(), "catalog Git tree object hash") || !empty {
					t.Fatalf("in-flight result: %v", e)
				}
				if !s.mu.TryLock() {
					t.Fatal("owner lock leaked")
				}
				s.mu.Unlock()
			})
		}
	}
}

// One parent deadline reaches every verification of every sequential
// frontdoor unchanged; no frontdoor renews it or substitutes Background.
func TestCatalogContextDeadlineInheritance(t *testing.T) {
	var seen []time.Time
	s := contextTestOwner(func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			return errors.New("verification without deadline")
		}
		seen = append(seen, d)
		return nil
	})
	parent, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	deadline, _ := parent.Deadline()
	for _, f := range contextTestFrontdoors() {
		_, _ = f.call(parent, s)
	}
	if e := s.VerifyContext(parent); e != nil {
		t.Fatal(e)
	}
	// verify once, guidance and producer each reach their first recheck before
	// refusing the synthetic inventory, then verify again.
	if len(seen) != 4 {
		t.Fatalf("verification calls %d", len(seen))
	}
	for _, d := range seen {
		if !d.Equal(deadline) {
			t.Fatalf("deadline renewed: %v != %v", d, deadline)
		}
	}
	seen = nil
	before := time.Now()
	if e := s.VerifyContext(context.Background()); e != nil {
		t.Fatal(e)
	}
	// Without a parent deadline the frontdoor supplies catalogContextLimit.
	if len(seen) != 1 || seen[0].After(time.Now().Add(catalogContextLimit)) || seen[0].Before(before.Add(catalogContextLimit-time.Second)) {
		t.Fatalf("frontdoor bound %v", seen[0])
	}
}

// The legacy entry points keep Background: they never see a deadline from
// the context frontdoors and remain unbounded by catalogContextLimit.
func TestCatalogContextLegacyBackground(t *testing.T) {
	var withDeadline atomic.Int32
	s := contextTestOwner(func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); ok || ctx.Done() != nil {
			withDeadline.Add(1)
		}
		return nil
	})
	if e := s.Verify(); e != nil {
		t.Fatal(e)
	}
	_, _ = InspectCatalogGuidanceSnapshot(s)
	_, _ = InspectCatalogProducerSnapshot(s)
	if withDeadline.Load() != 0 {
		t.Fatal("legacy frontdoor gained a context bound")
	}
}

func TestCatalogContextLock(t *testing.T) {
	var s CatalogSnapshot
	s.mu.Lock()
	acquired := make(chan error, 1)
	go func() { acquired <- catalogLock(context.Background(), &s.mu) }()
	select {
	case <-acquired:
		t.Fatal("lock acquired while held")
	case <-time.After(4 * catalogLockBackoff):
	}
	s.mu.Unlock()
	select {
	case e := <-acquired:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("released lock not acquired")
	}
	if s.mu.TryLock() {
		t.Fatal("successful acquisition did not hold the lock")
	}
	s.mu.Unlock()
}

func TestCatalogContextErr(t *testing.T) {
	integrity := errors.New("catalog Git blob bound")
	if catalogContextErr(context.Background(), integrity) != integrity || catalogContextErr(context.Background(), nil) != nil {
		t.Fatal("live context rewrote error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if catalogContextErr(ctx, nil) != nil {
		t.Fatal("success invented an error")
	}
	e := catalogContextErr(ctx, integrity)
	if !errors.Is(e, context.Canceled) || !errors.Is(e, integrity) {
		t.Fatalf("recovered error %v", e)
	}
	if already := catalogContextErr(ctx, e); already != e {
		t.Fatal("double wrapped")
	}
}
