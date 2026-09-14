package publish_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/publish"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

const movedCommit = "5555555555555555555555555555555555555555"

// authorFleetIntent puts a reviewed fleet document into the release this
// checkout publishes, the way an operator would: by editing the manifest and
// committing it.
func authorFleetIntent(t *testing.T, root string) []byte {
	t.Helper()
	intent, err := os.ReadFile(
		filepath.Join("..", "..", "testdata", "fleet", "fleet-intent.json"))
	if err != nil {
		t.Fatalf("intent fixture: %v", err)
	}
	document, err := canonjson.DecodeObject(intent)
	if err != nil {
		t.Fatalf("decode the intent: %v", err)
	}
	path := filepath.Join(root, release.File)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("published manifest: %v", err)
	}
	manifest, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	components, _ := manifest["components"].(map[string]any)
	components[fleet.Component] = document
	encoded, err := canonjson.MarshalFile(manifest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return intent
}

// captureOnly writes the candidate manifest and stops. These tests are about
// what a capture puts in the manifest, so nothing is committed and the
// fixture's Git history stays where it started.
func captureOnly(subject *fixture) publish.Options {
	options := subject.options()
	options.Commit, options.Push = false, false
	return options
}

func capturedComponent(t *testing.T, root, name string) []byte {
	t.Helper()
	value, err := release.Load(root)
	if err != nil {
		t.Fatalf("the captured manifest does not load back: %v", err)
	}
	encoded, err := canonjson.MarshalFile(value.Component(name))
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	return encoded
}

// Capture reads the controller's own machine, and for the fleet intent that is
// exactly what it must not do: one host's local state would become fleet-wide
// authorization. This is the regression test the ADR asks for, because capture
// has already moved a reviewed pin to the controller's newer checkout once.
func TestCaptureNeverOverwritesAuthoredFleetIntent(t *testing.T) {
	subject := newFixture(t)
	if _, err := publish.Release(subject.root, captureOnly(subject)); err != nil {
		t.Fatalf("first capture: %v", err)
	}
	authored := authorFleetIntent(t, subject.root)

	if _, err := publish.Release(subject.root, captureOnly(subject)); err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if captured := capturedComponent(t, subject.root, fleet.Component); string(captured) != string(authored) {
		t.Errorf("capture rewrote the authored fleet intent:\nauthored:\n%s\ncaptured:\n%s",
			authored, captured)
	}

	// It also survives a capture that names other components.
	options := captureOnly(subject)
	options.Components = []string{"t3-steward"}
	if _, err := publish.Release(subject.root, options); err != nil {
		t.Fatalf("selective capture: %v", err)
	}
	if captured := capturedComponent(t, subject.root, fleet.Component); string(captured) != string(authored) {
		t.Errorf("a selective capture rewrote the authored fleet intent:\n%s", captured)
	}
}

// Asking to capture the fleet intent is refused rather than quietly doing
// nothing. A selection that exits 0 without capturing anything reads as
// success, and the operator who ran it believes their host was consulted.
func TestCapturingTheFleetIntentIsRefused(t *testing.T) {
	subject := newFixture(t)
	if _, err := publish.Release(subject.root, captureOnly(subject)); err != nil {
		t.Fatalf("first capture: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(subject.root, release.File))
	if err != nil {
		t.Fatalf("published manifest: %v", err)
	}

	options := captureOnly(subject)
	options.Components = []string{fleet.Component}
	_, err = publish.Release(subject.root, options)
	if err == nil {
		t.Fatal("capturing authored fleet intent was accepted")
	}
	if !strings.Contains(err.Error(), "authored, not captured") ||
		!strings.Contains(err.Error(), release.File) {
		t.Errorf("the refusal does not say where the edit belongs: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(subject.root, release.File))
	if err != nil || string(after) != string(before) {
		t.Error("a refused capture still rewrote the manifest")
	}
}

// A release captured before the fleet component existed keeps being captured
// without one: adding a component may not start writing it into releases that
// never asked for it.
func TestCaptureDoesNotInventFleetIntent(t *testing.T) {
	subject := newFixture(t)
	if _, err := publish.Release(subject.root, captureOnly(subject)); err != nil {
		t.Fatalf("capture: %v", err)
	}
	value, err := release.Load(subject.root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if value.Component(fleet.Component) != nil {
		t.Error("capture invented a fleet intent nobody authored")
	}
	if _, present, err := value.FleetIntent(); present || err != nil {
		t.Errorf("FleetIntent reports present=%v, error=%v", present, err)
	}
}

// --components is documented as capturing only what it names. This pins the
// other half of that promise: every component it does not name keeps exactly
// what the release already published, even when the controller's own checkout
// has moved on.
func TestComponentsSelectionKeepsEveryComponentItDoesNotName(t *testing.T) {
	subject := newFixture(t)
	if _, err := publish.Release(subject.root, captureOnly(subject)); err != nil {
		t.Fatalf("first capture: %v", err)
	}
	published := capturedComponent(t, subject.root, "huyang")

	// The controller's Huyang checkout moves ahead of the release.
	subject.huyangHead = movedCommit
	options := captureOnly(subject)
	options.Components = []string{"t3-steward"}
	if _, err := publish.Release(subject.root, options); err != nil {
		t.Fatalf("selective capture: %v", err)
	}
	if captured := capturedComponent(t, subject.root, "huyang"); string(captured) != string(published) {
		t.Errorf("a selective capture carried an unnamed component along:\n%s", captured)
	}

	// Naming it does capture it, so the preservation above is a choice rather
	// than an inability to observe.
	options.Components = []string{"huyang"}
	if _, err := publish.Release(subject.root, options); err != nil {
		t.Fatalf("selective capture: %v", err)
	}
	value, err := release.Load(subject.root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if value.Component("huyang")["commit"] != movedCommit {
		t.Errorf("a named component was not captured: %v", value.Component("huyang"))
	}
}
