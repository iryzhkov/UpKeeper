package author_test

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

const steward = "0.11.0-rc.95"

// fakeGenerator stands in for the checkout's agents-instructions-gen. It
// derives one reference file from a skill, the way the real one does, writes
// the managed Codex block, and records the home it was given so that a test
// can prove the scratch home, not the test runner's, was the one written to.
const fakeGenerator = `#!/usr/bin/env bash
set -euo pipefail
[ "$1" = --root ] || exit 64
root=$2
mkdir -p "$HOME/.config/agents" "$HOME/.codex"
cat "$root/config/claude/skills/alpha/SKILL.md" > "$HOME/.config/agents/alpha.md"
printf 'generated for %s\n' "$root" | sed 's|/.*/checkout|CHECKOUT|' > "$HOME/.config/agents/pointer.md"
printf '<!-- fleet:start -->\nshared\n<!-- fleet:end -->\n' > "$HOME/.codex/AGENTS.md"
printf '%s\n' "$HOME" > "$HOME/.config/agents/home.txt"
`

type fixture struct {
	root      string
	setup     string
	temporary string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	previous := release.EnvironmentValidator
	release.EnvironmentValidator = agentenv.ValidateEnvironment
	t.Cleanup(func() { release.EnvironmentValidator = previous })
	for key, value := range map[string]string{
		"GIT_AUTHOR_NAME": "test", "GIT_AUTHOR_EMAIL": "test@example.invalid",
		"GIT_COMMITTER_NAME": "test", "GIT_COMMITTER_EMAIL": "test@example.invalid",
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
	} {
		t.Setenv(key, value)
	}
	base := t.TempDir()
	subject := &fixture{root: filepath.Join(base, "upkeeper"), setup: filepath.Join(base, "omarchy-setup")}
	// A temporary directory of its own shows whether the run cleaned up.
	subject.temporary = filepath.Join(base, "tmp")
	if err := os.MkdirAll(subject.temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", subject.temporary)

	// The previously pinned commit has no skills yet, and the release pins it.
	write(t, filepath.Join(subject.setup, "config/claude/CLAUDE.md"), "# Global\n", 0o644)
	write(t, filepath.Join(subject.setup, author.Generator), fakeGenerator, 0o755)
	gitIn(t, subject.setup, "init", "--quiet", "--initial-branch=main")
	pinned := subject.commit(t, subject.setup, "setup")
	// origin is the release's source spelled the scp way, and Git is told to
	// fetch it from the clone itself, so that fetching needs no network.
	gitIn(t, subject.setup, "remote", "add", "origin", fixtureOrigin)
	gitIn(t, subject.setup, "config", "url."+subject.setup+".insteadOf", fixtureOrigin)

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "release", "paired-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	manifest["components"].(map[string]any)["omarchy-setup"].(map[string]any)["commit"] = pinned
	encoded, err := canonjson.MarshalFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(subject.root, release.File), string(encoded), 0o644)
	write(t, filepath.Join(subject.root, "README.md"), "UpKeeper\n", 0o644)
	gitIn(t, subject.root, "init", "--quiet", "--initial-branch=main")
	subject.commit(t, subject.root, "fixture release")

	write(t, filepath.Join(subject.setup, "config/claude/skills/alpha/SKILL.md"), "alpha one\n", 0o644)
	write(t, filepath.Join(subject.setup, "config/claude/skills/alpha/references/notes.md"), "notes\n", 0o644)
	write(t, filepath.Join(subject.setup, "config/claude/skills/beta/SKILL.md"), "beta\n", 0o644)
	write(t, filepath.Join(subject.setup, "config/claude/skills/beta/scripts/run.sh"), "#!/bin/sh\n", 0o755)
	subject.commit(t, subject.setup, "skills")
	return subject
}

// fixtureOrigin is the omarchy-setup source paired-v1.json names, in the
// spelling an ssh clone of it has.
const fixtureOrigin = "git@github.com:iryzhkov/omarchy-setup.git"

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (f *fixture) commit(t *testing.T, directory, message string) string {
	t.Helper()
	gitIn(t, directory, "add", "-A")
	gitIn(t, directory, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "commit", "--quiet", "-m", message)
	return gitIn(t, directory, "rev-parse", "HEAD")
}

func (f *fixture) author(t *testing.T, commit string, dryRun bool) (author.Result, error) {
	t.Helper()
	return author.Run(f.root, author.Options{
		Steward: steward, SetupCommit: commit, SetupRepository: f.setup, DryRun: dryRun,
	})
}

func (f *fixture) manifest(t *testing.T) (map[string]any, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.root, release.File))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := canonjson.DecodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, raw
}

func environmentFiles(t *testing.T, manifest map[string]any, key string) map[string]map[string]any {
	t.Helper()
	components := manifest["components"].(map[string]any)
	environment := components["agent-environment"].(map[string]any)
	listed, _ := environment[key].([]any)
	files := map[string]map[string]any{}
	for _, item := range listed {
		entry := item.(map[string]any)
		files[entry["path"].(string)] = entry
	}
	return files
}

func content(t *testing.T, entry map[string]any) string {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(entry["content"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return string(decoded)
}

func actions(result author.Result) map[string]string {
	found := map[string]string{}
	for _, file := range result.Files {
		found[file.Path] = file.Action
	}
	return found
}

func TestAuthorPinsAndProjectsTheCommit(t *testing.T) {
	subject := newFixture(t)
	before, original := subject.manifest(t)
	head := gitIn(t, subject.root, "rev-parse", "HEAD")
	commit := gitIn(t, subject.setup, "rev-parse", "HEAD")

	result, err := subject.author(t, commit[:12], false)
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if !result.Changed || !result.Written || result.SetupCommit != commit || result.Head != head {
		t.Fatalf("result = %+v", result)
	}
	pins := map[string]author.Pin{}
	for _, pin := range result.Pins {
		pins[pin.Name] = pin
	}
	if pins["t3-steward.version"].To != steward || pins["t3.compatibility.steward_version"].To != steward ||
		pins["omarchy-setup.commit"].To != commit || len(pins) != 3 {
		t.Fatalf("pins = %+v", result.Pins)
	}

	after, written := subject.manifest(t)
	if agentenv.Digest(written) != result.ReleaseSHA256 || agentenv.Digest(original) != result.PreviousReleaseSHA256 {
		t.Fatal("the reported digests are not those of the files")
	}
	if _, err := release.Load(subject.root); err != nil {
		t.Fatalf("the authored release does not load: %v", err)
	}
	components := after["components"].(map[string]any)
	if components["t3-steward"].(map[string]any)["version"] != steward ||
		components["t3"].(map[string]any)["compatibility"].(map[string]any)["steward_version"] != steward ||
		components["omarchy-setup"].(map[string]any)["commit"] != commit {
		t.Fatal("the pins were not written")
	}

	// Every byte outside the pins and the agent environment is what it was.
	previousComponents := before["components"].(map[string]any)
	for _, pinned := range []struct{ component, key string }{
		{"t3-steward", "version"}, {"omarchy-setup", "commit"},
	} {
		components[pinned.component].(map[string]any)[pinned.key] = previousComponents[pinned.component].(map[string]any)[pinned.key]
	}
	components["t3"].(map[string]any)["compatibility"].(map[string]any)["steward_version"] =
		previousComponents["t3"].(map[string]any)["compatibility"].(map[string]any)["steward_version"]
	for name, component := range previousComponents {
		if name == "agent-environment" {
			continue
		}
		want, _ := canonjson.Marshal(component)
		got, _ := canonjson.Marshal(components[name])
		if !bytes.Equal(want, got) {
			t.Errorf("component %s changed beyond its pins", name)
		}
	}
	for _, key := range []string{"captured_at", "captured_from", "schema_version"} {
		if after[key] != before[key] {
			t.Errorf("%s changed", key)
		}
	}

	files := environmentFiles(t, after, "files")
	for _, path := range []string{
		".claude/skills/alpha/SKILL.md", ".agents/skills/alpha/SKILL.md",
		".claude/skills/alpha/references/notes.md", ".agents/skills/beta/scripts/run.sh",
		".config/agents/alpha.md", ".config/agents/pointer.md",
	} {
		if files[path] == nil || actions(result)[path] != agentenv.RefreshAdded {
			t.Errorf("%s was not added (action %q)", path, actions(result)[path])
		}
	}
	if files[".agents/skills/beta/scripts/run.sh"]["permissions"].(interface{ String() string }).String() != "493" ||
		files[".claude/skills/alpha/SKILL.md"]["permissions"].(interface{ String() string }).String() != "420" {
		t.Error("skill files do not carry the modes Git records for them")
	}
	if content(t, files[".config/agents/alpha.md"]) != "alpha one\n" ||
		content(t, files[".claude/omarchy-setup/CLAUDE.md"]) != "# Global\n" ||
		content(t, files[".codex/AGENTS.md"]) != "<!-- fleet:start -->\nshared\n<!-- fleet:end -->\n" {
		t.Error("the derived instructions are not the checkout's")
	}
	if files[".config/agents/home.txt"] != nil {
		t.Error("a generated file capture would not publish was published")
	}
	// The generator wrote into its scratch home, which is gone again.
	if matches, _ := filepath.Glob(filepath.Join(subject.temporary, "*")); len(matches) != 0 {
		t.Errorf("scratch directories were left behind: %v", matches)
	}
	// Released instructions the generator no longer writes are retired, and a
	// file the release carried that nothing derives is left alone.
	retired := environmentFiles(t, after, "retired_files")
	if retired[".config/agents/huyang.md"] == nil || files[".config/agents/huyang.md"] != nil ||
		actions(result)[".config/agents/huyang.md"] != agentenv.RefreshRetired {
		t.Error("a stale generated instruction was not retired")
	}
	if files[".claude/skills/tdd/SKILL.md"] == nil || actions(result)[".claude/skills/tdd/SKILL.md"] != "" {
		t.Error("a skill the checkout does not own was touched")
	}
	spec := components["agent-environment"].(map[string]any)["spec"].(map[string]any)
	provenance := spec["project_instructions"].([]any)[0].(map[string]any)
	if provenance["repository_revision"] != head {
		t.Errorf("provenance revision = %v, want HEAD %s", provenance["repository_revision"], head)
	}
}

func TestAuthorAgainIsANoOp(t *testing.T) {
	subject := newFixture(t)
	commit := gitIn(t, subject.setup, "rev-parse", "HEAD")
	if _, err := subject.author(t, commit, false); err != nil {
		t.Fatal(err)
	}
	subject.commit(t, subject.root, "authored")
	_, first := subject.manifest(t)
	// A later HEAD alone is not a reason to rewrite the environment.
	write(t, filepath.Join(subject.root, "NOTES"), "unrelated\n", 0o644)
	subject.commit(t, subject.root, "unrelated")

	result, err := subject.author(t, commit, false)
	if err != nil {
		t.Fatal(err)
	}
	_, second := subject.manifest(t)
	if result.Changed || result.Written || len(result.Pins) != 0 || len(result.Files) != 0 {
		t.Fatalf("a repeated run reports changes: %+v", result)
	}
	if !bytes.Equal(first, second) || result.ReleaseSHA256 != agentenv.Digest(first) {
		t.Fatal("a repeated run changed the release bytes")
	}
}

func TestAuthorRefreshesChangedAndNewSkillFiles(t *testing.T) {
	subject := newFixture(t)
	first := gitIn(t, subject.setup, "rev-parse", "HEAD")
	if _, err := subject.author(t, first, false); err != nil {
		t.Fatal(err)
	}
	subject.commit(t, subject.root, "authored")
	before, _ := subject.manifest(t)

	write(t, filepath.Join(subject.setup, "config/claude/skills/alpha/SKILL.md"), "alpha two\n", 0o644)
	write(t, filepath.Join(subject.setup, "config/claude/skills/alpha/extra.md"), "extra\n", 0o644)
	if err := os.RemoveAll(filepath.Join(subject.setup, "config/claude/skills/beta")); err != nil {
		t.Fatal(err)
	}
	second := subject.commit(t, subject.setup, "change alpha, drop beta")

	// A dry run reports the same result and writes nothing.
	preview, err := subject.author(t, second, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, unchanged := subject.manifest(t); !preview.Changed || preview.Written ||
		agentenv.Digest(unchanged) != preview.PreviousReleaseSHA256 {
		t.Fatalf("dry run = %+v", preview)
	}
	result, err := subject.author(t, second, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReleaseSHA256 != preview.ReleaseSHA256 {
		t.Fatal("the dry run predicted different bytes")
	}
	if len(result.Pins) != 1 || result.Pins[0].Name != "omarchy-setup.commit" ||
		result.Pins[0].From != first || result.Pins[0].To != second {
		t.Fatalf("pins = %+v", result.Pins)
	}
	want := map[string]string{
		".claude/skills/alpha/SKILL.md":      agentenv.RefreshChanged,
		".agents/skills/alpha/SKILL.md":      agentenv.RefreshChanged,
		".config/agents/alpha.md":            agentenv.RefreshChanged,
		".claude/skills/alpha/extra.md":      agentenv.RefreshAdded,
		".agents/skills/alpha/extra.md":      agentenv.RefreshAdded,
		".claude/skills/beta/SKILL.md":       agentenv.RefreshRetired,
		".agents/skills/beta/SKILL.md":       agentenv.RefreshRetired,
		".claude/skills/beta/scripts/run.sh": agentenv.RefreshRetired,
		".agents/skills/beta/scripts/run.sh": agentenv.RefreshRetired,
	}
	got := actions(result)
	if len(got) != len(want) {
		t.Errorf("refreshed %v, want %v", got, want)
	}
	for path, action := range want {
		if got[path] != action {
			t.Errorf("%s: %q, want %q", path, got[path], action)
		}
	}
	after, _ := subject.manifest(t)
	files := environmentFiles(t, after, "files")
	retired := environmentFiles(t, after, "retired_files")
	if content(t, files[".agents/skills/alpha/SKILL.md"]) != "alpha two\n" ||
		content(t, files[".claude/skills/alpha/extra.md"]) != "extra\n" ||
		retired[".claude/skills/beta/SKILL.md"] == nil || files[".claude/skills/beta/SKILL.md"] != nil {
		t.Error("the refreshed component does not carry the new commit")
	}
	if _, err := release.Load(subject.root); err != nil {
		t.Fatalf("the authored release does not load: %v", err)
	}
	for name, component := range before["components"].(map[string]any) {
		if name == "agent-environment" || name == "omarchy-setup" {
			continue
		}
		want, _ := canonjson.Marshal(component)
		got, _ := canonjson.Marshal(after["components"].(map[string]any)[name])
		if !bytes.Equal(want, got) {
			t.Errorf("component %s changed", name)
		}
	}
}

func TestAuthorRefusesADirtyCheckout(t *testing.T) {
	subject := newFixture(t)
	commit := gitIn(t, subject.setup, "rev-parse", "HEAD")
	_, original := subject.manifest(t)
	write(t, filepath.Join(subject.root, "stray.txt"), "work in progress\n", 0o644)

	_, err := subject.author(t, commit, false)
	if err == nil || !strings.Contains(err.Error(), "stray.txt") {
		t.Fatalf("err = %v, want a refusal naming stray.txt", err)
	}
	if _, unchanged := subject.manifest(t); !bytes.Equal(original, unchanged) {
		t.Fatal("a refused run wrote the release")
	}

	// A change to the release itself is the one local change allowed, so that
	// an authored release can be recomputed before it is committed.
	if err := os.Remove(filepath.Join(subject.root, "stray.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := subject.author(t, commit, false); err != nil {
		t.Fatal(err)
	}
	if result, err := subject.author(t, commit, false); err != nil || result.Changed {
		t.Fatalf("rerun on an uncommitted release: %+v, %v", result, err)
	}
}

func TestAuthorRefusesAForeignOrigin(t *testing.T) {
	subject := newFixture(t)
	commit := gitIn(t, subject.setup, "rev-parse", "HEAD")
	gitIn(t, subject.setup, "remote", "set-url", "origin", "https://github.com/someone/omarchy-setup.git")
	gitIn(t, subject.setup, "config", "url."+subject.setup+".insteadOf", "https://github.com/someone/omarchy-setup.git")
	_, err := subject.author(t, commit, true)
	if err == nil || !strings.Contains(err.Error(), "but the release takes omarchy-setup from") {
		t.Fatalf("err = %v, want a refusal for a foreign origin", err)
	}
	// The https spelling of the same repository is the same repository.
	gitIn(t, subject.setup, "remote", "set-url", "origin", "https://GitHub.com/iryzhkov/omarchy-setup/")
	gitIn(t, subject.setup, "config", "url."+subject.setup+".insteadOf", "https://GitHub.com/iryzhkov/omarchy-setup/")
	if _, err := subject.author(t, commit, true); err != nil {
		t.Fatalf("the https spelling of the source was refused: %v", err)
	}
}

func TestAuthorRefusesWithoutThePreviousPin(t *testing.T) {
	subject := newFixture(t)
	commit := gitIn(t, subject.setup, "rev-parse", "HEAD")
	manifest, _ := subject.manifest(t)
	manifest["components"].(map[string]any)["omarchy-setup"].(map[string]any)["commit"] = strings.Repeat("a", 40)
	encoded, err := canonjson.MarshalFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(subject.root, release.File), string(encoded), 0o644)
	subject.commit(t, subject.root, "pin an unknown commit")
	_, err = subject.author(t, commit, true)
	if err == nil || !strings.Contains(err.Error(), "previously pinned commit aaaaaaaaaaaa is not in") {
		t.Fatalf("err = %v, want a refusal for a missing previous pin", err)
	}
}

func TestAuthorRefusesALinkOutOfTheCheckout(t *testing.T) {
	subject := newFixture(t)
	if err := os.Symlink("../../../../../etc", filepath.Join(subject.setup, "config/claude/skills/escape")); err != nil {
		t.Fatal(err)
	}
	commit := subject.commit(t, subject.setup, "link out")
	_, err := subject.author(t, commit, true)
	if err == nil || !strings.Contains(err.Error(), "outside the checkout") {
		t.Fatalf("err = %v, want a refusal for a link out of the checkout", err)
	}
}

func TestAuthorModesDoNotDependOnTheUmask(t *testing.T) {
	subject := newFixture(t)
	commit := gitIn(t, subject.setup, "rev-parse", "HEAD")
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)
	if _, err := subject.author(t, commit, false); err != nil {
		t.Fatal(err)
	}
	files := environmentFiles(t, func() map[string]any { manifest, _ := subject.manifest(t); return manifest }(), "files")
	for path, want := range map[string]string{
		".config/agents/alpha.md":            "420",
		".codex/AGENTS.md":                   "420",
		".claude/omarchy-setup/CLAUDE.md":    "420",
		".claude/skills/alpha/SKILL.md":      "420",
		".agents/skills/beta/scripts/run.sh": "493",
	} {
		if got := files[path]["permissions"].(interface{ String() string }).String(); got != want {
			t.Errorf("%s: permissions %s under umask 077, want %s", path, got, want)
		}
	}
}

func TestAuthorIgnoresALeftoverTemporaryRelease(t *testing.T) {
	subject := newFixture(t)
	commit := gitIn(t, subject.setup, "rev-parse", "HEAD")
	write(t, filepath.Join(subject.root, "desired/.release.json.tmp"), "torn\n", 0o644)
	if result, err := subject.author(t, commit, false); err != nil || !result.Written {
		t.Fatalf("a leftover temporary release blocked the run: %+v, %v", result, err)
	}
	if _, err := release.Load(subject.root); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorRefusesACommitTheRefDoesNotReach(t *testing.T) {
	subject := newFixture(t)
	gitIn(t, subject.setup, "checkout", "--quiet", "-b", "side")
	write(t, filepath.Join(subject.setup, "config/claude/skills/alpha/SKILL.md"), "unpublished\n", 0o644)
	side := subject.commit(t, subject.setup, "side")
	_, original := subject.manifest(t)

	_, err := subject.author(t, side, false)
	if err == nil || !strings.Contains(err.Error(), "is not on origin/main") {
		t.Fatalf("err = %v, want a refusal for an unpublished commit", err)
	}
	if _, unchanged := subject.manifest(t); !bytes.Equal(original, unchanged) {
		t.Fatal("a refused run wrote the release")
	}
	if _, err := subject.author(t, "main", false); err == nil {
		t.Fatal("a branch name was accepted as a commit")
	}
}
