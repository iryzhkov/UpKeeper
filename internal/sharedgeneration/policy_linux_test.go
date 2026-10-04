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
	next := installSyntheticV2(t, s)
	for _, id := range []string{next.GenerationID, old.GenerationID, next.GenerationID} {
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
