package release_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// projectRoot is a scratch checkout holding the frozen paired manifest with the
// shared fleet intent fixture added, so that every project edit under test
// runs against a copy and never against the repository's own release.
func projectRoot(t *testing.T) string {
	t.Helper()
	acceptAnyEnvironment(t)
	root := t.TempDir()
	manifest := pairedManifestFixture(t)
	intent, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fleet", "fleet-intent.json"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := canonjson.DecodeObject(intent)
	if err != nil {
		t.Fatal(err)
	}
	components, _ := manifest["components"].(map[string]any)
	components[fleet.Component] = document
	encoded, err := canonjson.MarshalFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, release.File), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func manifestBytes(t *testing.T, root string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, release.File))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func realManifestDigest(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", release.File))
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func TestAddProjectWritesACanonicalManifestTheFleetParserAccepts(t *testing.T) {
	real := realManifestDigest(t)
	root := projectRoot(t)
	value, err := release.AddProject(root, release.ProjectSpec{
		Name: "home-assistant-config", Repository: "ssh://git@git.ryzhkov.dev/igor/home-assistant-config.git",
		DefaultRef: "main", SetupProfile: "git-status", EligibleWorkers: []string{"omarchy-pc", "homelab"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	document, present, err := value.FleetIntent()
	if err != nil || !present {
		t.Fatalf("returned release has no parseable fleet intent: %v", err)
	}
	project, ok := document.Projects["home-assistant-config"]
	if !ok {
		t.Fatal("the project is absent from the returned release")
	}
	if strings.Join(project.EligibleWorkers, ",") != "homelab,omarchy-pc" {
		t.Fatalf("eligible workers are not sorted and unique: %v", project.EligibleWorkers)
	}
	// What was written is what was returned, and it is canonical.
	raw := manifestBytes(t, root)
	loaded, err := release.Load(root)
	if err != nil {
		t.Fatalf("the written manifest does not load: %v", err)
	}
	if _, err := fleet.Parse(loaded.Component(fleet.Component)); err != nil {
		t.Fatalf("fleet.Parse refuses the written component: %v", err)
	}
	decoded, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := canonjson.MarshalFile(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, raw) {
		t.Fatal("the written manifest is not canonical")
	}
	if loaded.Digest() != value.Digest() {
		t.Fatal("the returned release differs from the written one")
	}
	if _, err := os.Stat(filepath.Join(root, "desired", ".release.json.tmp")); err == nil {
		t.Fatal("the temporary file was left behind")
	}
	if realManifestDigest(t) != real {
		t.Fatal("the repository's own desired/release.json changed")
	}
}

func TestAddProjectRefusesAndLeavesTheManifestUntouched(t *testing.T) {
	cases := []struct {
		name     string
		spec     release.ProjectSpec
		expected string
	}{
		{"an unknown worker", release.ProjectSpec{Name: "scratch", Repository: "https://example.invalid/scratch.git", DefaultRef: "main", SetupProfile: "git-status", EligibleWorkers: []string{"laptop"}}, "laptop"},
		{"no workers", release.ProjectSpec{Name: "scratch", Repository: "https://example.invalid/scratch.git", DefaultRef: "main", SetupProfile: "git-status"}, "eligible worker"},
		{"an existing project", release.ProjectSpec{Name: "upkeeper", Repository: "https://example.invalid/scratch.git", DefaultRef: "main", SetupProfile: "git-status", EligibleWorkers: []string{"homelab"}}, "already"},
		{"a bad repository", release.ProjectSpec{Name: "scratch", Repository: "scratch; rm -rf /", DefaultRef: "main", SetupProfile: "git-status", EligibleWorkers: []string{"homelab"}}, "repository"},
		{"a bad name", release.ProjectSpec{Name: "Scratch Project", Repository: "https://example.invalid/scratch.git", DefaultRef: "main", SetupProfile: "git-status", EligibleWorkers: []string{"homelab"}}, "name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := projectRoot(t)
			before := manifestBytes(t, root)
			_, err := release.AddProject(root, tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.expected) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.expected)
			}
			if !bytes.Equal(before, manifestBytes(t, root)) {
				t.Fatal("a refused add changed the manifest")
			}
		})
	}
}

func TestRemoveProjectRemovesItAndRefusesAnUnknownOne(t *testing.T) {
	root := projectRoot(t)
	value, err := release.RemoveProject(root, "upkeeper")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	document, _, err := value.FleetIntent()
	if err != nil {
		t.Fatal(err)
	}
	if _, still := document.Projects["upkeeper"]; still {
		t.Fatal("the project is still in the returned release")
	}
	loaded, err := release.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest() != value.Digest() {
		t.Fatal("the written manifest differs from the returned release")
	}
	before := manifestBytes(t, root)
	if _, err := release.RemoveProject(root, "upkeeper"); err == nil || !strings.Contains(err.Error(), "upkeeper") {
		t.Fatalf("removing an absent project: err = %v", err)
	}
	if !bytes.Equal(before, manifestBytes(t, root)) {
		t.Fatal("a refused remove changed the manifest")
	}
}

func TestProjectEditsRefuseAReleaseWithoutFleetIntent(t *testing.T) {
	acceptAnyEnvironment(t)
	root := t.TempDir()
	encoded, err := canonjson.MarshalFile(pairedManifestFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, release.File), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = release.AddProject(root, release.ProjectSpec{Name: "scratch", Repository: "https://example.invalid/scratch.git", DefaultRef: "main", SetupProfile: "git-status", EligibleWorkers: []string{"homelab"}})
	if err == nil || !strings.Contains(err.Error(), fleet.Component) {
		t.Fatalf("err = %v", err)
	}
}
