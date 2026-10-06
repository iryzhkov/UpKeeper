package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// These regressions come from the independent review of the custody repair
// (findings F1-F4). Each drives the real apply, rollback or reconciliation
// path with synthetic runner results; no live service is touched.

const t3SyntheticKeyPayload = "SYNTHETIC_PRIVATE_KEY_BYTES"

// F1: a complete private key block in failed restart output straddles the
// custody bound. Redaction must see the complete value before any bound.
func TestT3CustodyRedactsBeforeBound(t *testing.T) {
	for name, output := range map[string]string{
		// The footer falls past the custody bound once the error label is added.
		"custody-bound": strings.Repeat("x", 300) + "\n-----BEGIN RSA PRIVATE KEY-----\n" + t3SyntheticKeyPayload + "\n-----END RSA PRIVATE KEY-----",
		// The header falls before the command-output tail Must keeps.
		"command-tail": "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("A", 380) + t3SyntheticKeyPayload + "\n-----END RSA PRIVATE KEY-----",
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
				t.Fatalf("restart failure not exercised: injected=%v error=%v", injected, e)
			}
			for _, text := range append([]string{e.Error(), string(body)}, h.journals["restoration-required"]...) {
				if strings.Contains(text, t3SyntheticKeyPayload) {
					t.Fatal("private key payload in restart output leaked into custody evidence")
				}
			}
		})
	}
}

// F2: a dependent whose activity cannot be queried is unknown, not inactive,
// and refuses the restart before any effect.
func TestT3CustodyDependentActivityUnknownRefuses(t *testing.T) {
	h := newT3CustodyHost(t)
	h.dependents["t3-steward.service"] = "invocation-1"
	prior := h.c.Runner
	h.c.Runner = func(cmd procx.Command) procx.Result {
		if adapterTestMatches(cmd.Argv, "is-active") && cmd.Argv[len(cmd.Argv)-1] == "t3-steward.service" {
			return procx.Result{Code: 1, Stderr: "Failed to connect to bus"}
		}
		return prior(cmd)
	}
	got, e := h.apply(t)
	if h.restarts > 0 || e == nil {
		t.Fatalf("unknown dependent activity permitted a restart: result=%q error=%v restarts=%d", got, e, h.restarts)
	}
}

// F3: a dependent active before the transaction stays a restoration
// obligation even when it stopped after the failed restart.
func TestT3CustodyRollbackRetainsOriginalDependents(t *testing.T) {
	h := newT3CustodyHost(t)
	h.dependents["t3-steward.service"] = "invocation-1"
	prior := h.c.Runner
	h.c.Runner = func(cmd procx.Command) procx.Result {
		if adapterTestMatches(cmd.Argv, "is-active") && cmd.Argv[len(cmd.Argv)-1] == "t3-steward.service" && h.restarts > 0 {
			return procx.Result{Code: 3, Stdout: "inactive"}
		}
		return prior(cmd)
	}
	got, e := h.apply(t)
	_, body, _ := h.transaction(t)
	var v map[string]any
	_ = json.Unmarshal(body, &v)
	var rolled *RolledBack
	if errors.As(e, &rolled) || v["phase"] == "restored" {
		t.Fatalf("original active dependent remains inactive but restoration accepted: result=%q phase=%v error=%v restarts=%d", got, v["phase"], e, h.restarts)
	}
}

// F3 and F4 in reconciliation: a dependent the transaction found active must
// be active again, and a journal replaced with identical bytes is still a
// different record. Both refuse with the journal unchanged.
func TestT3ReconcileUpholdsDependentsAndJournalIdentity(t *testing.T) {
	for _, mode := range []string{"dependent-active", "dependent-inactive", "dependent-unknown", "journal-replaced", "journal-changed-at-publish"} {
		t.Run(mode, func(t *testing.T) {
			h := newT3CustodyHost(t)
			h.dependents["t3-steward.service"] = "invocation-1"
			_, name := h.filesRestored(t)
			dir, original, v := h.transaction(t)
			if journaled, _ := v["dependents"].(map[string]any); journaled["t3-steward.service"] != "invocation-1" {
				t.Fatalf("original dependent census not journaled: %v", v["dependents"])
			}
			prior := h.c.Runner
			h.c.Runner = func(cmd procx.Command) procx.Result {
				if adapterTestMatches(cmd.Argv, "is-active") && cmd.Argv[len(cmd.Argv)-1] == "t3-steward.service" {
					switch mode {
					case "dependent-inactive":
						return procx.Result{Code: 3, Stdout: "inactive"}
					case "dependent-unknown":
						return procx.Result{Code: 1, Stderr: "Failed to connect to bus"}
					}
				}
				return prior(cmd)
			}
			if mode == "journal-replaced" {
				journal := filepath.Join(dir, "transaction.json")
				if e := os.WriteFile(journal+".new", original, 0600); e != nil {
					t.Fatal(e)
				}
				checks := 0
				base := h.c.Runner
				h.c.Runner = func(cmd procx.Command) procx.Result {
					if cmd.Argv[len(cmd.Argv)-1] == "check" {
						// Checks 1 and 2 precede the journal read; 3 is the readiness sample.
						if checks++; checks == 3 {
							if e := os.Rename(journal+".new", journal); e != nil {
								t.Fatal(e)
							}
						}
					}
					return base(cmd)
				}
			}
			if mode == "journal-changed-at-publish" {
				// Another writer changes the record after the last fence, while the
				// replacement journal is being written.
				original = []byte(strings.Replace(string(original), `"phase":"files-restored"`, `"phase":"operator-intervened"`, 1))
				create := h.c.t3JournalFiles.create
				h.c.t3JournalFiles.create = func(dir, pattern string) (t3JournalFile, error) {
					if e := os.WriteFile(filepath.Join(dir, "transaction.json"), original, 0600); e != nil {
						t.Fatal(e)
					}
					return create(dir, pattern)
				}
			}
			_, got, e := h.reconcile(t, name)
			_, body, after := h.transaction(t)
			if mode == "dependent-active" {
				if e != nil || got != "updated" || after["phase"] != "restored" {
					t.Fatalf("active journaled dependent refused: %s %v", got, e)
				}
				return
			}
			want := map[string]string{"dependent-inactive": "dependent t3-steward.service", "dependent-unknown": "dependent t3-steward.service", "journal-replaced": "journal was replaced", "journal-changed-at-publish": "journal changed"}[mode]
			if e == nil || got != "" || !strings.Contains(e.Error(), want) {
				t.Fatalf("%s accepted or refused for another reason: %s %v", mode, got, e)
			}
			if string(body) != string(original) {
				t.Fatal("refusal changed the journal")
			}
		})
	}
}

// F4: a journal changed by another writer while reconciliation probes is
// refused and left byte-identical.
func TestT3ReconcileRefusesJournalDrift(t *testing.T) {
	h := newT3CustodyHost(t)
	_, name := h.filesRestored(t)
	dir, original, _ := h.transaction(t)
	changed := strings.Replace(string(original), `"phase":"files-restored"`, `"phase":"operator-intervened"`, 1)
	if changed == string(original) {
		t.Fatal("fixture phase replacement failed")
	}
	prior := h.c.Runner
	injected := false
	checks := 0
	h.c.Runner = func(cmd procx.Command) procx.Result {
		if cmd.Argv[len(cmd.Argv)-1] == "check" {
			checks++
		}
		if !injected && checks == 2 {
			injected = true
			if e := os.WriteFile(filepath.Join(dir, "transaction.json"), []byte(changed), 0600); e != nil {
				t.Fatal(e)
			}
		}
		return prior(cmd)
	}
	actions := []string{}
	got, e := t3ReconcileRetained(h.c, h.spec, name, &actions)
	body, _ := os.ReadFile(filepath.Join(dir, "transaction.json"))
	if !injected || e == nil || got != "" || string(body) != changed {
		t.Fatalf("journal drift accepted or overwritten: injected=%v result=%q error=%v changed_journal_preserved=%v", injected, got, e, string(body) == changed)
	}
}
