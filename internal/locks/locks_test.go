package locks_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/locks"
)

func metadata(t *testing.T, runID string) locks.Metadata {
	t.Helper()
	value, err := locks.NewMetadata(
		runID, "normandy", 4242, "a"+repeat("0", 39),
		[]string{"huyang", "huyang", "t3-steward"},
		time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewMetadata: %v", err)
	}
	return value
}

func TestMetadataNormalizesComponentsAndTimestamp(t *testing.T) {
	value := metadata(t, "20260914T090000Z-0a1b2c3d")
	if len(value.Components) != 2 ||
		value.Components[0] != "huyang" || value.Components[1] != "t3-steward" {
		t.Errorf("components were not deduplicated and sorted: %v", value.Components)
	}
	if value.StartedAt != "2026-09-14T09:00:00Z" {
		t.Errorf("started_at is %q", value.StartedAt)
	}
}

func TestMetadataRefusesAnImpossibleController(t *testing.T) {
	_, err := locks.NewMetadata(
		"20260914T090000Z-0a1b2c3d", "normandy", 0, "x", nil, time.Now())
	if err == nil || err.Error() != "controller_pid must be positive" {
		t.Fatalf("expected a refusal, got %v", err)
	}
}

func TestAcquireIsExclusiveAndReleaseIsExact(t *testing.T) {
	store := &locks.Store{Root: filepath.Join(t.TempDir(), "locks")}
	mine := metadata(t, "20260914T090000Z-0a1b2c3d")
	theirs := metadata(t, "20260914T090100Z-11112222")

	if _, err := store.Acquire("omarchy-pc", mine); err != nil {
		t.Fatalf("first acquisition failed: %v", err)
	}
	_, err := store.Acquire("omarchy-pc", theirs)
	if err == nil || !failure.Is(err, failure.KindLock) {
		t.Fatalf("a held lock must report contention, got %v", err)
	}
	// Releasing someone else's lock is the one thing that must never work.
	if err := store.Release("omarchy-pc", theirs.RunID); err == nil {
		t.Fatal("released a lock this run does not own")
	}
	if err := store.Release("omarchy-pc", mine.RunID); err != nil {
		t.Fatalf("owner could not release: %v", err)
	}
	inspection, err := store.Inspect("omarchy-pc")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspection.State != "absent" {
		t.Errorf("after release the lock is %q", inspection.State)
	}
}

func TestInspectReportsAnUnreadableOwnerAsInvalid(t *testing.T) {
	root := filepath.Join(t.TempDir(), "locks")
	store := &locks.Store{Root: root}
	if _, err := store.Acquire("homelab", metadata(t, "20260914T090000Z-0a1b2c3d")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	owner := filepath.Join(root, "homelab.lock", "owner.json")
	if err := os.WriteFile(owner, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	inspection, err := store.Inspect("homelab")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspection.State != "invalid" {
		t.Errorf("state is %q, expected invalid", inspection.State)
	}
}

func TestAcquireAllRollsBackAPartialAcquisition(t *testing.T) {
	store := &locks.Store{Root: filepath.Join(t.TempDir(), "locks")}
	blocker := metadata(t, "20260914T090100Z-11112222")
	if _, err := store.Acquire("normandy", blocker); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	mine := metadata(t, "20260914T090000Z-0a1b2c3d")
	if _, err := store.AcquireAll([]string{"homelab", "normandy"}, mine); err == nil {
		t.Fatal("expected contention")
	}
	inspection, err := store.Inspect("homelab")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspection.State != "absent" {
		t.Errorf("the already-taken lock was not rolled back: %q", inspection.State)
	}
}

func TestStaleIsOnlyConfirmedWhenTheControllerIsKnownGone(t *testing.T) {
	value := metadata(t, "20260914T090000Z-0a1b2c3d")
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	yes, no := true, false
	cases := []struct {
		name     string
		alive    *bool
		age      time.Duration
		expected bool
	}{
		{"young lock is never stale", &no, 24 * time.Hour, false},
		{"old lock with a live controller is not stale", &yes, time.Minute, false},
		{"old lock with an unknown controller is not stale", nil, time.Minute, false},
		{"old lock with a dead controller is stale", &no, time.Minute, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stale, err := locks.StaleConfirmed(value, now, testCase.age,
				func(string, int) *bool { return testCase.alive })
			if err != nil {
				t.Fatalf("StaleConfirmed: %v", err)
			}
			if stale != testCase.expected {
				t.Errorf("produced %v, expected %v", stale, testCase.expected)
			}
		})
	}
}

func TestOrderedHostsRefusesDuplicates(t *testing.T) {
	if _, err := locks.OrderedHosts([]string{"a", "a"}); err == nil {
		t.Fatal("expected a refusal")
	}
	ordered, err := locks.OrderedHosts([]string{"normandy", "homelab"})
	if err != nil {
		t.Fatalf("OrderedHosts: %v", err)
	}
	if ordered[0] != "homelab" || ordered[1] != "normandy" {
		t.Errorf("acquisition order is not deterministic: %v", ordered)
	}
}

func repeat(value string, count int) string {
	out := ""
	for index := 0; index < count; index++ {
		out += value
	}
	return out
}
