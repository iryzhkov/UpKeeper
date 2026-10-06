package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// These regressions come from the second independent review of the custody
// repair (findings F5-F7). Each drives the real apply, rollback,
// reconciliation or journal path with synthetic runner results and synthetic
// values; no live service or real credential is touched.

// F5: a failed restart reports a password assignment whose value is longer
// than the tail Must keeps. Cutting the tail first removes the label and
// leaves a value fragment redaction cannot recognise.
func TestT3CustodyRedactsAssignmentBeforeCommandTail(t *testing.T) {
	const secret = "SYNTHETIC_ASSIGNMENT_PAYLOAD"
	for name, output := range map[string]string{
		"must-tail": "password=" + strings.Repeat("A", 500) + secret + strings.Repeat("B", 200) + " end",
		// Past the 2000-byte tail Run keeps as well.
		"run-tail": "password=" + strings.Repeat("A", 2500) + secret + strings.Repeat("B", 200) + " end",
	} {
		t.Run(name, func(t *testing.T) {
			h := newT3CustodyHost(t)
			prior := h.c.Runner
			injected := false
			h.c.Runner = func(cmd procx.Command) procx.Result {
				if !injected && adapterTestMatches(cmd.Argv, "restart") {
					injected = true
					h.restarts++
					return procx.Result{Code: 1, Stdout: output}
				}
				return prior(cmd)
			}
			_, e := h.apply(t)
			_, body, _ := h.transaction(t)
			if !injected || e == nil {
				t.Fatal("failure not exercised")
			}
			if strings.Contains(e.Error(), secret) || strings.Contains(string(body), secret) {
				t.Fatal("assignment secret survives the command tail in the returned error or the durable journal")
			}
			if !strings.Contains(e.Error(), "restart idle T3 failed") {
				t.Fatalf("original cause lost: %v", e)
			}
		})
	}
}

// F6: the T3 unit was known inactive before activation while a PartOf
// dependent was active. The failed restart leaves the dependent inactive, so
// restoration must not be accepted even though the T3 unit is inactive again.
func TestT3CustodyInactiveParentKeepsDependentObligations(t *testing.T) {
	h := newT3CustodyHost(t)
	h.dependents["t3-steward.service"] = "invocation-1"
	prior := h.c.Runner
	h.c.Runner = func(cmd procx.Command) procx.Result {
		if adapterTestMatches(cmd.Argv, "is-active") {
			unit := cmd.Argv[len(cmd.Argv)-1]
			if unit == adapterSpecString(h.spec, "service") || (unit == "t3-steward.service" && h.restarts > 0) {
				return procx.Result{Code: 3, Stdout: "inactive"}
			}
		}
		return prior(cmd)
	}
	got, e := h.apply(t)
	_, _, v := h.transaction(t)
	var rolled *RolledBack
	if h.restarts == 0 || e == nil {
		t.Fatalf("failure not exercised: %s %v", got, e)
	}
	if errors.As(e, &rolled) || v["phase"] != "files-restored" {
		t.Fatalf("active original dependent remains inactive but restoration accepted: phase=%v restarts=%d", v["phase"], h.restarts)
	}
	if h.restarts != 1 {
		t.Fatalf("restoration added a restart: %d", h.restarts)
	}
	if cause, _ := v["cause"].(string); cause == "" || !strings.Contains(e.Error(), "dependent") {
		t.Fatalf("cause or unverified dependent restoration not retained: %v %v", v["cause"], e)
	}
}

type t3SyncFailureDirectory struct{ *os.File }

func (f *t3SyncFailureDirectory) Sync() error { return errors.New("synthetic directory sync failure") }

// F7: reconciliation publishes its record, then another writer records an
// operator intervention while the directory sync runs, and the sync fails.
// Withdrawing the publication must not overwrite the newer record.
func TestT3ReconcileDurabilityRecoveryKeepsNewerJournal(t *testing.T) {
	h := newT3CustodyHost(t)
	_, name := h.filesRestored(t)
	dir, original, _ := h.transaction(t)
	changed := []byte(strings.Replace(string(original), `"phase":"files-restored"`, `"phase":"operator-intervened"`, 1))
	calls := 0
	h.c.t3JournalFiles.directory = func(p string) (t3JournalFile, error) {
		calls++
		f, e := os.Open(p)
		if calls == 2 {
			if e := os.WriteFile(filepath.Join(dir, "transaction.json"), changed, 0600); e != nil {
				t.Fatal(e)
			}
			return &t3SyncFailureDirectory{f}, e
		}
		return f, e
	}
	_, got, e := h.reconcile(t, name)
	body, _ := os.ReadFile(filepath.Join(dir, "transaction.json"))
	if calls < 2 || e == nil || got != "" {
		t.Fatal("publication durability failure not exercised")
	}
	if string(body) != string(changed) {
		t.Fatal("durability recovery overwrote the newer operator journal after the publish fence")
	}
	// The superseded record stays available beside the newer one.
	kept := false
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "-previous") {
			b, _ := os.ReadFile(filepath.Join(dir, entry.Name()))
			kept = kept || string(b) == string(original)
		}
	}
	if !kept {
		t.Fatal("superseded record needed for recovery was not kept")
	}
}

// F7: another writer acts after the publish fence passed and before the new
// record is published, by replacing the journal or changing it in place.
// Publication refuses and the other writer's record survives byte-identical.
func TestT3JournalPublicationKeepsWriterAfterFence(t *testing.T) {
	for _, writer := range []string{"replaced", "changed-in-place"} {
		t.Run(writer, func(t *testing.T) {
			_, tx, _, _ := t3TxPrepare(t, true)
			if e := tx.record("previous"); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(tx.dir, "transaction.json")
			newer := []byte(`{"phase":"operator-intervened"}`)
			tx.publishFence = func(string) error {
				if writer == "replaced" {
					other := filepath.Join(tx.dir, "operator.json")
					if e := os.WriteFile(other, newer, 0600); e != nil {
						t.Fatal(e)
					}
					return os.Rename(other, path)
				}
				return os.WriteFile(path, newer, 0600)
			}
			if e := tx.record("next"); e == nil {
				t.Fatal("publication ignored a writer acting after the fence")
			}
			if got, _ := os.ReadFile(path); string(got) != string(newer) {
				t.Fatalf("newer journal overwritten: %s", got)
			}
		})
	}
}
