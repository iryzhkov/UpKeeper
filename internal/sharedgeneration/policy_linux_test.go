//go:build linux

package sharedgeneration

import (
	"bytes"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"os"
	"path/filepath"
	"testing"
)

// Synthetic generation only: exercises versioned inert storage and physical
// closure. It makes no claim about owning parser/catalog or generator parity.
func installSyntheticV2(t *testing.T, s *Store) Inspection {
	return installSyntheticV2Payload(t, s, nil)
}
func installSyntheticV2Payload(t *testing.T, s *Store, mutate func(*Receipt, map[string][]byte)) Inspection {
	t.Helper()
	r, payload := syntheticV2Payload(t, mutate)
	rb, e := canonjson.MarshalFile(r)
	if e != nil {
		t.Fatal(e)
	}
	id := digest(rb)
	root := filepath.Join(s.root, "generations", id)
	if e = os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	for _, f := range r.Files {
		p := filepath.Join(root, f.Path)
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(p, payload[f.Path], os.FileMode(f.Mode)); e != nil {
			t.Fatal(e)
		}
	}
	if e = os.WriteFile(filepath.Join(root, "receipt.json"), rb, 0600); e != nil {
		t.Fatal(e)
	}
	return Inspection{id, &r, "trusted-generated-source"}
}
func syntheticV2Payload(t *testing.T, mutate func(*Receipt, map[string][]byte)) (Receipt, map[string][]byte) {
	t.Helper()
	r := receiptFixture()
	payload := map[string][]byte{}
	for n := range r.Files {
		f := &r.Files[n]
		b := []byte(f.Path + "\n")
		if f.Kind == "policy-input" {
			var e error
			b, e = canonjson.MarshalFile(map[string]any{})
			if e != nil {
				t.Fatal(e)
			}
		}
		f.SHA256 = digest(b)
		payload[f.Path] = b
		if f.Kind == "skill" {
			r.SourceSHA256["config/claude/"+f.Path] = f.SHA256
		}
		if f.Kind == "policy-input" {
			r.Policy.CanonicalPolicySHA256 = f.SHA256
		}
	}
	if mutate != nil {
		mutate(&r, payload)
	}
	for n := range r.Files {
		f := &r.Files[n]
		f.SHA256 = digest(payload[f.Path])
		if f.Kind == "skill" {
			r.SourceSHA256["config/claude/"+f.Path] = f.SHA256
		}
		if f.Kind == "policy-input" {
			r.Policy.CanonicalPolicySHA256 = f.SHA256
		}
	}
	if e := validateReceipt(r); e != nil {
		t.Fatal(e)
	}
	return r, payload
}
func TestMixedV1V2InspectSelectRollbackImmutable(t *testing.T) {
	repo, commit := fixture(t, "mixed")
	s := mustOpen(t)
	old := mustStage(t, s, repo, commit)
	oldPath := filepath.Join(s.root, "generations", old.GenerationID, "receipt.json")
	before, e := os.ReadFile(oldPath)
	if e != nil {
		t.Fatal(e)
	}
	prior := installSyntheticV2Payload(t, s, func(r *Receipt, _ map[string][]byte) {
		r.Policy.UpstreamCommit = "33cb805b5482d511071be7b344140e2ccb8c87da"
		r.Policy.UpstreamTree = "3439bd874573cb88bbad852c5b66771f2ab80eeb"
	})
	next := installSyntheticV2(t, s)
	if prior.GenerationID == next.GenerationID {
		t.Fatal("provenance identities collapsed")
	}
	retained := map[string][]byte{}
	for _, id := range []string{old.GenerationID, prior.GenerationID, next.GenerationID} {
		b, err := os.ReadFile(filepath.Join(s.root, "generations", id, "receipt.json"))
		if err != nil {
			t.Fatal(err)
		}
		retained[id] = b
	}
	// Reading and rollback require no consumer, current source or live catalog.
	t.Setenv("PATH", t.TempDir())
	for _, id := range []string{next.GenerationID, prior.GenerationID, old.GenerationID, prior.GenerationID, next.GenerationID} {
		i, e := s.SelectPrior(id)
		if e != nil {
			t.Fatal(e)
		}
		label := "trusted-generated-source"
		if i.Receipt.SchemaVersion == 2 {
			label += "; derivation-only"
		}
		if i.GenerationID != id || i.TrustLabel != label {
			t.Fatal("selection identity")
		}
		observed, e := s.Inspect()
		if e != nil {
			t.Fatal(e)
		}
		if observed.GenerationID != id {
			t.Fatal("inspect selection")
		}
		if observed.Receipt.SchemaVersion == 2 && (observed.Receipt.Policy.CurrentAuthorization != "unknown" || observed.Receipt.Policy.Readiness != "unassessed") {
			t.Fatal("inspection claimed authority")
		}
	}
	after, e := os.ReadFile(oldPath)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("v1 rewritten")
	}
	for id, before := range retained {
		after, err := os.ReadFile(filepath.Join(s.root, "generations", id, "receipt.json"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("immutable receipt rewritten", id, err)
		}
	}
}

func TestV2MixedAndArbitraryProvenanceRefusesOfflineSelection(t *testing.T) {
	for _, pair := range [][2]string{
		{"33cb805b5482d511071be7b344140e2ccb8c87da", "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52"},
		{"b32c0b6f48e12b7d74a60f8a010f84bab73d299f", "3439bd874573cb88bbad852c5b66771f2ab80eeb"},
		{"0000000000000000000000000000000000000000", "fc0e7ad2bad7a7df96030f71696125b5a7d5bd52"},
		{"b32c0b6f48e12b7d74a60f8a010f84bab73d299f", "0000000000000000000000000000000000000000"},
	} {
		t.Run(pair[0]+"/"+pair[1], func(t *testing.T) {
			s := mustOpen(t)
			good := installSyntheticV2(t, s)
			if _, err := s.SelectPrior(good.GenerationID); err != nil {
				t.Fatal(err)
			}
			bad := installSyntheticV2Payload(t, s, func(r *Receipt, payload map[string][]byte) {
				// Distinct valid fixture before forging only provenance.
				payload[r.Files[1].Path] = []byte("distinct fixture\n")
			})
			bad.Receipt.Policy.UpstreamCommit, bad.Receipt.Policy.UpstreamTree = pair[0], pair[1]
			if err := validateReceipt(*bad.Receipt); err == nil {
				t.Fatal("invalid provenance accepted")
			}
			raw, err := canonjson.MarshalFile(bad.Receipt)
			if err != nil {
				t.Fatal(err)
			}
			oldRoot := filepath.Join(s.root, "generations", bad.GenerationID)
			if err := os.WriteFile(filepath.Join(oldRoot, "receipt.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			id := digest(raw)
			if err := os.Rename(oldRoot, filepath.Join(s.root, "generations", id)); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", t.TempDir())
			if _, err := s.SelectPrior(id); err == nil {
				t.Fatal("forged provenance selected")
			}
			// Corruption anywhere in retained evidence fences inspection.
			if _, err := s.Inspect(); err == nil {
				t.Fatal("corrupt retained provenance inspected")
			}
			target, err := os.Readlink(filepath.Join(s.root, "candidate-current"))
			if err != nil || target != "generations/"+good.GenerationID {
				t.Fatal("refusal changed selector", target, err)
			}
		})
	}
}

func TestV2PolicyPhysicalClosureRefusals(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "hardlink", "extra-input", "noncanonical"} {
		t.Run(kind, func(t *testing.T) {
			s := mustOpen(t)
			i := installSyntheticV2(t, s)
			root := filepath.Join(s.root, "generations", i.GenerationID)
			p := filepath.Join(root, "inputs/route-policy.json")
			var e error
			switch kind {
			case "mode":
				e = os.Chmod(p, 0644)
			case "symlink":
				e = os.Remove(p)
				if e == nil {
					e = os.Symlink("/dev/null", p)
				}
			case "hardlink":
				e = os.Link(p, filepath.Join(t.TempDir(), "link"))
			case "extra-input":
				e = os.WriteFile(filepath.Join(root, "inputs/manifest.json"), []byte("{}\n"), 0600)
			case "noncanonical":
				e = os.WriteFile(p, []byte("{}"), 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.Inspect(); e == nil {
				t.Fatal("unsafe v2 policy closure accepted")
			}
		})
	}
}
