package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The supported writers of a T3 transaction journal are UpKeeper runs and the
// reconcile command, serialized by the host's target lock. These tests prove
// the lock is required for publication and that serialized supported writers
// publish correctly.

func TestTargetLockHeldOnlyWhileAcquired(t *testing.T) {
	home := t.TempDir()
	lock := filepath.Join(home, "locks", "host.lock")
	owner := &Context{RunID: "run-a", Payload: Payload{Lock: map[string]any{"run_id": "run-a"}}, lockDirectory: lock}
	if o, e := owner.acquireLock(); e != nil || o != nil || !owner.lockHeld {
		t.Fatalf("lock not taken: %v %v held=%v", o, e, owner.lockHeld)
	}
	other := &Context{RunID: "run-b", lockDirectory: lock}
	if o, e := other.acquireLock(); e != nil || o == nil || other.lockHeld {
		t.Fatalf("contending run treated as holding the lock: %v %v held=%v", o, e, other.lockHeld)
	}
	// The agent of the controller's own run re-enters the lock it holds.
	same := &Context{RunID: "run-a", lockDirectory: lock}
	if o, e := same.acquireLock(); e != nil || o != nil || !same.lockHeld {
		t.Fatalf("same-run re-entry not holding the lock: %v %v held=%v", o, e, same.lockHeld)
	}
	owner.releaseLock()
	if owner.lockHeld {
		t.Fatal("lock still held after release")
	}
}

func TestT3JournalRequiresTargetLock(t *testing.T) {
	c, tx, _, _ := t3TxPrepare(t, true)
	c.lockHeld = false
	e := tx.record("staging")
	if e == nil || !strings.Contains(e.Error(), "target lock") {
		t.Fatalf("journal published without the target lock: %v", e)
	}
	for name := range t3DirFiles(t, tx.dir) {
		if name != "stage" {
			t.Fatalf("refusal without the lock wrote %s", name)
		}
	}
}

// A failed transaction run and the reconcile command are both supported
// writers. While one holds the target lock the other cannot publish; each
// publishes correctly once it holds the lock in turn.
func TestT3JournalSupportedWritersSerializedByLock(t *testing.T) {
	h := newT3CustodyHost(t)
	lock := filepath.Join(h.c.Home, ".local/state/dev-fleet/locks/testhost.lock")
	acquire := func(c *Context, run string) {
		t.Helper()
		c.lockHeld, c.lockOwned = false, false
		c.RunID, c.lockDirectory = run, lock
		c.Payload.Lock = map[string]any{"run_id": run}
		if o, e := c.acquireLock(); e != nil || o != nil {
			t.Fatalf("%s did not take the lock: %v %v", run, o, e)
		}
	}
	acquire(h.c, "run-apply")
	_, name := h.filesRestored(t)
	h.c.releaseLock()
	dir, original, _ := h.transaction(t)

	acquire(h.c, "run-reconcile")
	// A second supported writer that cannot take the lock cannot publish.
	contender := &Context{Home: h.c.Home, Host: h.c.Host, RunID: "run-other", Components: h.c.Components,
		Env: h.c.Env, Runner: h.c.Runner, Sleep: h.c.Sleep, lockDirectory: lock,
		Selected: []string{"t3"}, Payload: Payload{ReconcileT3Transaction: name}}
	if o, _ := contender.acquireLock(); o == nil || contender.lockHeld {
		t.Fatal("contending writer took the held lock")
	}
	actions := []string{}
	if got, e := t3ReconcileRetained(contender, h.spec, name, &actions); e == nil || got != "" || !strings.Contains(e.Error(), "target lock") {
		t.Fatalf("writer without the lock reconciled: %s %v", got, e)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "transaction.json")); string(b) != string(original) {
		t.Fatal("writer without the lock changed the journal")
	}

	_, got, e := h.reconcile(t, name)
	h.c.releaseLock()
	_, _, v := h.transaction(t)
	if e != nil || got != "updated" || v["phase"] != "restored" || v["reconciliation"] == nil {
		t.Fatalf("serialized reconciliation did not publish: %s %v phase=%v", got, e, v["phase"])
	}
	if len(t3Quarantined(t3DirFiles(t, dir))) != 0 {
		t.Fatal("supported publication quarantined a record")
	}
}
