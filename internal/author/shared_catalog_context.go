package author

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// catalogContextLimit is the logical bound of one context-aware catalog
// operation. A shorter parent deadline always prevails; a frontdoor never
// renews it. This is cooperative cancellation, not a hard realtime bound:
// finite file reads, decoding and child reaping may delay return.
const catalogContextLimit = 120 * time.Second

// catalogLockBackoff bounds how long a waiting lock attempt sleeps between
// TryLock attempts; ctx.Done always interrupts the wait.
const catalogLockBackoff = 5 * time.Millisecond

// VerifyContext is Verify bounded by ctx and catalogContextLimit. Lock waits
// and in-flight Git verification observe cancellation; it returns only after
// verification and its Git children have completed. Cancellation errors stay
// discoverable through errors.Is. It confers no semantic or runtime acceptance.
func (s *CatalogSnapshot) VerifyContext(ctx context.Context) error {
	_, err := catalogWithOwner(ctx, s, func(ctx context.Context, s *CatalogSnapshot) (struct{}, error) {
		return struct{}{}, s.verifyLocked(ctx)
	})
	return err
}

// InspectCatalogGuidanceSnapshotContext is InspectCatalogGuidanceSnapshot
// with one bounded logical context across lock acquisition, both custody
// rechecks and every semantic stage. Failure or cancellation returns a zero
// audit; a successful audit is byte-identical to the legacy frontdoor.
func InspectCatalogGuidanceSnapshotContext(ctx context.Context, s *CatalogSnapshot) (CatalogGuidanceAudit, error) {
	return catalogWithOwner(ctx, s, inspectCatalogGuidanceLocked)
}

// InspectCatalogProducerSnapshotContext is InspectCatalogProducerSnapshot
// with one bounded logical context, under the same rules as
// InspectCatalogGuidanceSnapshotContext.
func InspectCatalogProducerSnapshotContext(ctx context.Context, s *CatalogSnapshot) (CatalogProducerAudit, error) {
	return catalogWithOwner(ctx, s, inspectCatalogProducerLocked)
}

// catalogWithOwner derives one child of ctx bounded by catalogContextLimit,
// acquires s.mu under it and runs body synchronously while holding the lock.
// The lock is released only after body, including any Git child, returns.
func catalogWithOwner[T any](ctx context.Context, s *CatalogSnapshot, body func(context.Context, *CatalogSnapshot) (T, error)) (T, error) {
	var zero T
	if ctx == nil {
		return zero, errors.New("catalog context required")
	}
	if s == nil {
		return zero, errors.New("catalog snapshot nil owner")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	ctx, cancel := context.WithTimeout(ctx, catalogContextLimit)
	defer cancel()
	if err := catalogLock(ctx, &s.mu); err != nil {
		return zero, err
	}
	defer s.mu.Unlock()
	v, err := body(ctx, s)
	if err != nil {
		return zero, catalogContextErr(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return v, nil
}

// catalogLock acquires mu on the calling goroutine or returns ctx's error
// without holding it. No helper goroutine is started, and a waiter never
// sleeps past ctx.Done. Close keeps its unconditional lock.
func catalogLock(ctx context.Context, mu *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if mu.TryLock() {
			if err := ctx.Err(); err != nil {
				mu.Unlock()
				return err
			}
			return nil
		}
		// Go 1.23+ timers need no drain before Reset or after Stop.
		if timer == nil {
			timer = time.NewTimer(catalogLockBackoff)
		} else {
			timer.Reset(catalogLockBackoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// catalogContextErr restores ctx's cancellation or deadline error after a
// synchronous step has completed, because Git helpers may replace a killed
// command's error with an integrity error. err itself is kept as well.
func catalogContextErr(ctx context.Context, err error) error {
	cause := ctx.Err()
	if err == nil || cause == nil || errors.Is(err, cause) {
		return err
	}
	return fmt.Errorf("%w: %w", cause, err)
}
