package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// These regressions come from the third independent review of the custody
// repair (F9, F10) and the narrowed journal concurrency contract that
// followed it: UpKeeper runs and the reconcile command hold the host's target
// lock while they publish, and any other writer of transaction.json is
// unsupported. Publication refuses such a writer without loss: the on-disk
// journal is left as that writer left it, the intended record is kept under a
// quarantine name beside it and no file is deleted.

// F9: a failed health check reports a password assignment whose value is
// longer than the tail Run keeps. Cutting the tail first removes the label
// and leaves a value fragment in the observation's check_error, which the
// readiness samples then persist. The post-restart samples and the
// restoration samples both quote that check.
func TestT3CustodyRedactsHealthCheckBeforeTail(t *testing.T) {
	const secret = "SYNTHETIC_CHECK_PAYLOAD"
	output := "password=" + strings.Repeat("A", 2300) + secret + strings.Repeat("B", 1750) + " end"
	// Subtest names avoid "restart": the fixture counts any argument naming it.
	for name, restoration := range map[string]bool{"activation-check": false, "restoration-check": true} {
		t.Run(name, func(t *testing.T) {
			h := newT3CustodyHost(t)
			// For the restoration samples the new native descendant exits after
			// the restart, so rollback restarts the previous service and its
			// health check fails.
			h.nativeGone = restoration
			prior := h.c.Runner
			injected := map[int]bool{}
			h.c.Runner = func(cmd procx.Command) procx.Result {
				failing := (!restoration && h.restarts == 1) || (restoration && h.restarts == 2)
				if failing && cmd.Argv[len(cmd.Argv)-1] == "check" {
					injected[h.restarts] = true
					return procx.Result{Code: 1, Stdout: output}
				}
				return prior(cmd)
			}
			_, e := h.apply(t)
			_, body, v := h.transaction(t)
			if e == nil || (!restoration && !injected[1]) || (restoration && !injected[2]) {
				t.Fatalf("failure not exercised: %v %v restarts=%d", injected, e, h.restarts)
			}
			stages := t3SampleStages(v)
			if len(stages["post-restart"]) == 0 || (restoration && len(stages["restoration"]) == 0) {
				t.Fatalf("failed check samples not retained: %v", stages)
			}
			if strings.Contains(e.Error(), secret) || strings.Contains(string(body), secret) {
				t.Fatal("health check assignment fragment persisted in failed_samples or the returned error")
			}
			if !strings.Contains(string(body), "check_error") {
				t.Fatal("redacted health check evidence not retained")
			}
		})
	}
}

// t3DirFiles lists every entry of a transaction directory with its bytes;
// directories have none.
func t3DirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	files := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			files[entry.Name()] = ""
			continue
		}
		b, e := os.ReadFile(filepath.Join(dir, entry.Name()))
		if e != nil {
			t.Fatal(e)
		}
		files[entry.Name()] = string(b)
	}
	return files
}

// t3Quarantined returns the quarantined journal files in a transaction
// directory, by name.
func t3Quarantined(files map[string]string) map[string]string {
	q := map[string]string{}
	for name, b := range files {
		if strings.HasPrefix(name, "transaction.json.quarantine-") {
			q[name] = b
		}
	}
	return q
}

func t3QuarantinedPhase(t *testing.T, files map[string]string, phase string) string {
	t.Helper()
	for name, b := range t3Quarantined(files) {
		var v map[string]any
		if json.Unmarshal([]byte(b), &v) == nil && v["phase"] == phase {
			return name
		}
	}
	return ""
}

// F10 and the earlier journal-concurrency findings (F4, F7), re-expressed
// under the narrowed contract. An unsupported writer acts at each point of a
// publication. Every case refuses, leaves transaction.json exactly as that
// writer left it, keeps the intended record (or, when the writer already
// overwrote it in place, the superseded one) under a quarantine name, leaves
// no temporary name behind and deletes no file.
func TestT3JournalUnsupportedWriterRefusedWithoutLoss(t *testing.T) {
	type writerCase struct {
		previous bool // a record exists before the publication
		// fence acts after the snapshot, before the re-check; durability acts
		// after the rename, and the following directory sync fails.
		fence, durability func(t *testing.T, path string)
		want              string // the writer's record; "" means removed
	}
	newer := `{"phase":"operator-intervened"}`
	inPlace := func(t *testing.T, path string) {
		if e := os.WriteFile(path, []byte(newer), 0600); e != nil {
			t.Fatal(e)
		}
	}
	replace := func(t *testing.T, path string) {
		other := path + ".operator"
		if e := os.WriteFile(other, []byte(newer), 0600); e != nil {
			t.Fatal(e)
		}
		if e := os.Rename(other, path); e != nil {
			t.Fatal(e)
		}
	}
	remove := func(t *testing.T, path string) {
		if e := os.Remove(path); e != nil {
			t.Fatal(e)
		}
	}
	for name, c := range map[string]writerCase{
		"changed-in-place-before-publication":    {previous: true, fence: inPlace, want: newer},
		"replaced-before-publication":            {previous: true, fence: replace, want: newer},
		"removed-before-publication":             {previous: true, fence: remove},
		"appeared-before-first-record":           {fence: inPlace, want: newer},
		"changed-in-place-before-durability":     {previous: true, durability: inPlace, want: newer},
		"replaced-before-durability":             {previous: true, durability: replace, want: newer},
		"changed-first-record-before-durability": {durability: inPlace, want: newer},
	} {
		t.Run(name, func(t *testing.T) {
			c0, tx, _, _ := t3TxPrepare(t, true)
			path := filepath.Join(tx.dir, "transaction.json")
			superseded := ""
			if c.previous {
				if e := tx.record("previous"); e != nil {
					t.Fatal(e)
				}
				b, _ := os.ReadFile(path)
				superseded = string(b)
			}
			before := t3DirFiles(t, tx.dir)
			if c.fence != nil {
				tx.publishFence = func(string) error { c.fence(t, path); return nil }
			}
			if c.durability != nil {
				dirs := 0
				c0.t3JournalFiles = &t3JournalFiles{directory: func(p string) (t3JournalFile, error) {
					dirs++
					f, e := os.Open(p)
					if dirs == 2 {
						c.durability(t, path)
						return &t3SyncFailureDirectory{f}, e
					}
					return f, e
				}}
			}
			e := tx.record("next")
			if e == nil {
				t.Fatal("publication accepted an unsupported writer's change")
			}
			after := t3DirFiles(t, tx.dir)
			got, present := after["transaction.json"]
			if got != c.want || present != (c.want != "") {
				t.Fatalf("on-disk journal not left as the writer left it: %q present=%v", got, present)
			}
			for entry, b := range before {
				if entry == "transaction.json" {
					continue
				}
				if now, ok := after[entry]; !ok || now != b {
					t.Fatalf("refusal deleted or changed %s", entry)
				}
			}
			for entry := range after {
				if strings.HasPrefix(entry, ".journal-") {
					t.Fatalf("temporary name %s left behind instead of a quarantine name", entry)
				}
			}
			if !strings.Contains(e.Error(), path) {
				t.Fatalf("refusal does not report the journal path: %v", e)
			}
			kept := ""
			if c.fence != nil {
				// The intended record never reached the journal.
				kept = t3QuarantinedPhase(t, after, "next")
			} else if c.previous {
				// The writer overwrote the intended record after it was published;
				// the superseded record recovery needs is kept.
				for q, b := range t3Quarantined(after) {
					if b == superseded {
						kept = q
					}
				}
			} else {
				kept = "none"
			}
			if kept == "" {
				t.Fatalf("record not kept under a quarantine name: %v", after)
			}
			if kept != "none" && !strings.Contains(e.Error(), filepath.Join(tx.dir, kept)) {
				t.Fatalf("refusal does not report the quarantine path %s: %v", kept, e)
			}
		})
	}
}

// F10: a second unsupported writer acts after a first one forced a refusal.
// The round-2 exchange undo returned the earlier writer's record to the
// journal and deleted the later one. Now nothing is undone: each refusal
// leaves the journal as the latest writer left it, and every intended record
// is quarantined.
func TestT3JournalLaterWriterAfterRefusalKept(t *testing.T) {
	_, tx, _, _ := t3TxPrepare(t, true)
	if e := tx.record("previous"); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(tx.dir, "transaction.json")
	second := []byte(`{"phase":"operator-second"}`)
	third := []byte(`{"phase":"operator-third"}`)
	tx.publishFence = func(string) error { return os.WriteFile(path, second, 0600) }
	if e := tx.record("next"); e == nil {
		t.Fatal("conflict not refused")
	}
	tx.publishFence = nil
	if e := os.WriteFile(path, third, 0600); e != nil {
		t.Fatal(e)
	}
	if e := tx.record("again"); e == nil {
		t.Fatal("publication over the later writer accepted")
	}
	files := t3DirFiles(t, tx.dir)
	if files["transaction.json"] != string(third) {
		t.Fatalf("later writer record lost: %q", files["transaction.json"])
	}
	if t3QuarantinedPhase(t, files, "next") == "" || t3QuarantinedPhase(t, files, "again") == "" {
		t.Fatalf("intended records not quarantined: %v", t3Quarantined(files))
	}
}
