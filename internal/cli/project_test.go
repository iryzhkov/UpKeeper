package cli_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/cli"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// projectCheckout is fleetConfigRoot turned into a Git checkout with the
// manifest committed, because `project add` diffs against the release at HEAD
// and refuses to work outside a checkout.
func projectCheckout(t *testing.T) string {
	t.Helper()
	root := fleetConfigRoot(t, true)
	git := func(arguments ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
		command.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	git("init", "-q")
	git("add", ".")
	// Background maintenance after the commit would create and remove lock
	// files under .git while a test walks the tree, so it is switched off for
	// the fixture.
	git("-c", "gc.auto=0", "-c", "maintenance.auto=false", "commit", "-q", "-m", "fixture release")
	return root
}

func realReleaseDigest(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", release.File))
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func TestProjectAddEditsTheManifestAndReportsThePlan(t *testing.T) {
	real := realReleaseDigest(t)
	root := projectCheckout(t)
	t.Setenv(cli.ConfigRootEnvironment, root)
	var out, errs bytes.Buffer
	code := cli.Main("upkeeper", []string{"project", "add", "home-assistant-config",
		"--repository", "ssh://git@git.ryzhkov.dev/igor/home-assistant-config.git",
		"--default-ref", "main", "--workers", "homelab,omarchy-pc", "--json"},
		cli.Streams{Out: &out, Err: &errs})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, errs.String())
	}
	report, err := canonjson.DecodeObject(out.Bytes())
	if err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out.String())
	}
	if report["command"] != "project" || report["action"] != "add" || report["project"] != "home-assistant-config" {
		t.Fatalf("report header: %v", report)
	}
	plan, _ := report["fleet_plan"].(map[string]any)
	enrollment, _ := plan["enrollment_plan"].(map[string]any)
	changes, _ := enrollment["authorization_changes"].([]any)
	text := ""
	for _, change := range changes {
		text += change.(string) + "\n"
	}
	for _, expected := range []string{
		"project home-assistant-config is added at ssh://git@git.ryzhkov.dev/igor/home-assistant-config.git",
		"default local bindings",
		"t3-steward worker enroll homelab --current-catalog",
		"t3-steward worker enroll omarchy-pc --current-catalog",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("the plan does not say %q:\n%s", expected, text)
		}
	}
	if enrollment["applied"] != false {
		t.Error("the plan claims to have applied something")
	}
	value, err := release.Load(root)
	if err != nil {
		t.Fatalf("the edited manifest does not load: %v", err)
	}
	document, err := fleet.Parse(value.Component(fleet.Component))
	if err != nil {
		t.Fatalf("fleet.Parse refuses the edited component: %v", err)
	}
	project, ok := document.Projects["home-assistant-config"]
	if !ok || project.SetupProfile != "git-status" || strings.Join(project.EligibleWorkers, ",") != "homelab,omarchy-pc" {
		t.Fatalf("project = %+v, present=%v", project, ok)
	}
	if report["release_sha256"] != value.Digest() {
		t.Errorf("reported digest %v is not the written release's %s", report["release_sha256"], value.Digest())
	}
	if realReleaseDigest(t) != real {
		t.Fatal("the repository's own desired/release.json changed")
	}

	// The text form says the same and names the file that changed.
	out.Reset()
	if code := cli.Main("upkeeper", []string{"project", "remove", "home-assistant-config"},
		cli.Streams{Out: &out, Err: &errs}); code != 0 {
		t.Fatalf("remove: exit %d, stderr=%q", code, errs.String())
	}
	for _, expected := range []string{"removed project home-assistant-config from " + release.File, "not enrolled"} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("remove output does not say %q:\n%s", expected, out.String())
		}
	}
	value, err = release.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	document, err = fleet.Parse(value.Component(fleet.Component))
	if err != nil {
		t.Fatal(err)
	}
	if _, still := document.Projects["home-assistant-config"]; still {
		t.Fatal("the project survived removal")
	}
}

func TestProjectAddAuthorsAFreshWorkspaceWithoutGitFields(t *testing.T) {
	root := projectCheckout(t)
	t.Setenv(cli.ConfigRootEnvironment, root)
	var out, errs bytes.Buffer
	code := cli.Main("upkeeper", []string{"project", "add", "scratch",
		"--type", "fresh", "--workers", "homelab", "--json"},
		cli.Streams{Out: &out, Err: &errs})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, errs.String())
	}
	value, err := release.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	component := value.Component(fleet.Component)
	entry := component["projects"].(map[string]any)["scratch"].(map[string]any)
	if entry["type"] != "fresh" {
		t.Fatalf("type = %#v", entry["type"])
	}
	for _, key := range []string{"repository", "default_ref", "setup_profile"} {
		if _, present := entry[key]; present {
			t.Errorf("fresh manifest unexpectedly carries %s: %#v", key, entry[key])
		}
	}
	document, err := fleet.Parse(component)
	if err != nil {
		t.Fatalf("fleet.Parse refuses fresh project: %v", err)
	}
	if document.Projects["scratch"].SetupProfile != "" {
		t.Fatalf("implicit setup profile was materialized: %+v", document.Projects["scratch"])
	}
}

func TestProjectAddRefusesAnUnknownWorkerWithoutWriting(t *testing.T) {
	root := projectCheckout(t)
	t.Setenv(cli.ConfigRootEnvironment, root)
	before := snapshot(t, root)
	var out, errs bytes.Buffer
	code := cli.Main("upkeeper", []string{"project", "add", "scratch",
		"--repository", "https://example.invalid/scratch.git", "--default-ref", "main", "--workers", "laptop"},
		cli.Streams{Out: &out, Err: &errs})
	if code != 30 {
		t.Fatalf("exit %d, want 30; stderr=%q", code, errs.String())
	}
	if !strings.Contains(errs.String(), "laptop") {
		t.Errorf("the refusal does not name the worker: %q", errs.String())
	}
	after := snapshot(t, root)
	for path, content := range before {
		if after[path] != content {
			t.Errorf("a refused add rewrote %s", path)
		}
	}
}

func TestProjectUsageErrors(t *testing.T) {
	for _, argv := range [][]string{
		{"project"},
		{"project", "add"},
		{"project", "add", "scratch"},
		{"project", "add", "scratch", "--repository", "https://example.invalid/x.git", "--default-ref", "main"},
		{"project", "add", "scratch", "--type", "archive", "--workers", "homelab"},
		{"project", "add", "scratch", "--type", "fresh", "--repository", "https://example.invalid/x.git", "--workers", "homelab"},
		{"project", "remove"},
		{"project", "rename", "scratch"},
		{"project", "remove", "scratch", "--workers", "homelab"},
	} {
		var out, errs bytes.Buffer
		if code := cli.Main("upkeeper", argv, cli.Streams{Out: &out, Err: &errs}); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr=%q", argv, code, errs.String())
		}
	}
	var out, errs bytes.Buffer
	if code := cli.Main("upkeeper", []string{"project", "--help"}, cli.Streams{Out: &out, Err: &errs}); code != 0 {
		t.Fatalf("help: exit %d", code)
	}
	for _, expected := range []string{"usage: upkeeper project", "--workers", "--setup-profile", "pushes nothing"} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("help lacks %q:\n%s", expected, out.String())
		}
	}
}
