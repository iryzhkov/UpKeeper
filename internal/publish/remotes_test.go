package publish_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/publish"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

const laggingCommit = "7777777777777777777777777777777777777777"

// refusedBeforeAnything asserts a publication refused before it captured,
// wrote, staged, committed or pushed anything.
func refusedBeforeAnything(t *testing.T, f *fixture, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("the publication was not refused")
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q:\n%s", want, err)
		}
	}
	for _, forbidden := range []string{" add ", " commit ", " commit-tree ", " update-ref ", " push ", "agents-instructions-gen"} {
		if f.did(forbidden) {
			t.Errorf("a refused publication ran%s: %v", forbidden, f.calls)
		}
	}
	if _, statErr := os.Stat(filepath.Join(f.root, release.File)); statErr == nil {
		t.Error("a refused publication wrote a manifest")
	}
}

// A remote whose main lags is named with its commit, and so is the command
// that brings it level, before anything is captured.
func TestRemotesThatDisagreeOnMainRefuseThePublication(t *testing.T) {
	f := newFixture(t)
	f.remoteMain = map[string]string{"forgejo": laggingCommit}
	// origin and backup are at rootCommit, which contains the lagging commit.
	f.ancestors = map[string]bool{laggingCommit + " " + rootCommit: true}
	_, err := publish.Release(f.root, f.options())
	refusedBeforeAnything(t, f, err,
		"the remotes disagree on main", "origin at "+rootCommit, "forgejo at "+laggingCommit,
		"origin contains every other remote's main",
		"git push forgejo refs/remotes/origin/main:refs/heads/main")
}

func TestRemotesWhoseHistoriesDivergedNameNoResyncCommand(t *testing.T) {
	f := newFixture(t)
	f.remoteMain = map[string]string{"forgejo": laggingCommit}
	f.ancestors = map[string]bool{}
	_, err := publish.Release(f.root, f.options())
	refusedBeforeAnything(t, f, err, "the remotes disagree on main", "histories have diverged")
	if strings.Contains(err.Error(), "git push") {
		t.Errorf("a diverged history was given a push to run: %s", err)
	}
}

func TestARemoteWithoutMainIsADisagreement(t *testing.T) {
	f := newFixture(t)
	f.remoteMain = map[string]string{"backup": ""}
	_, err := publish.Release(f.root, f.options())
	refusedBeforeAnything(t, f, err, "backup at absent", "git push backup refs/remotes/origin/main:refs/heads/main")
}

func TestAMainThatHeadDoesNotContainIsRefused(t *testing.T) {
	f := newFixture(t)
	f.remoteMain = map[string]string{"origin": laggingCommit, "forgejo": laggingCommit, "backup": laggingCommit}
	f.ancestors = map[string]bool{}
	_, err := publish.Release(f.root, f.options())
	refusedBeforeAnything(t, f, err, "main on every remote is at "+laggingCommit,
		"git pull --ff-only origin main")
}

// A publication from a clone on another branch used to publish the release
// under that branch's name on every remote. It is refused instead, for a
// capture and for an authored release alike, and so is a detached HEAD.
func TestAPublicationFromAnythingButMainIsRefused(t *testing.T) {
	f := newFixture(t)
	f.branch = "rc119-release"
	_, err := publish.Release(f.root, f.options())
	refusedBeforeAnything(t, f, err, "the checkout is on branch rc119-release",
		"never the branch it was run from", "git switch main")

	// The reviewed manifest of an authored release is the operator's and
	// stays; nothing else may happen.
	f, digest, _ := prepareAuthoredFixture(t)
	f.branch = "rc119-release"
	options := f.options()
	options.Authored = true
	options.ExpectedHead = rootCommit
	options.ExpectedReleaseSHA256 = digest
	_, err = publish.Release(f.root, options)
	if err == nil || !strings.Contains(err.Error(), "the checkout is on branch rc119-release") ||
		!strings.Contains(err.Error(), "git switch main") {
		t.Fatalf("authored from a branch: %v", err)
	}
	for _, forbidden := range []string{" add ", " commit-tree ", " update-ref ", " push "} {
		if f.did(forbidden) {
			t.Fatalf("authored refusal ran%s: %v", forbidden, f.calls)
		}
	}

	f = newFixture(t)
	f.branch = "detached"
	_, err = publish.Release(f.root, f.options())
	refusedBeforeAnything(t, f, err, "detached HEAD", "git switch main")
}

// Committing without publishing touches no remote, so it may happen on any
// branch; only publication is held to main.
func TestACommitWithoutPushIsNotHeldToMain(t *testing.T) {
	f := newFixture(t)
	f.branch = "scratch"
	options := f.options()
	options.Push = false
	if _, err := publish.Release(f.root, options); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !f.did(" commit ") || f.did(" push ") || f.did(" ls-remote ") {
		t.Errorf("calls = %v", f.calls)
	}
}

// Naming some remotes and not others would leave the others behind, which is
// how one remote's main came to lag the other's by a week.
func TestAPublicationMustNameEveryConfiguredRemote(t *testing.T) {
	f := newFixture(t)
	options := f.options()
	options.Remotes = []string{"origin"}
	_, err := publish.Release(f.root, options)
	refusedBeforeAnything(t, f, err, "on every configured remote", "forgejo, backup was not named")

	f = newFixture(t)
	report, err := publish.Release(f.root, f.options())
	if err != nil {
		t.Fatal(err)
	}
	pushed, _ := report["pushed"].([]string)
	if strings.Join(pushed, ",") != "origin main,forgejo main,backup main" {
		t.Errorf("pushed = %v", pushed)
	}
	for _, remote := range []string{"origin", "forgejo", "backup"} {
		if !f.did("push --quiet " + remote + " HEAD:refs/heads/main") {
			t.Errorf("%s was not published to main: %v", remote, f.calls)
		}
	}
}

// The whole loop with real Git and two bare remotes: a lagging remote refuses
// the authored publication, the command the refusal names resyncs it, the
// publication then lands on main on both, and a branch never reaches them.
func TestAuthoredPublicationRealGitResyncsAndPublishesMainEverywhere(t *testing.T) {
	f, _, _ := prepareAuthoredFixture(t)
	if err := os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
		t.Fatal(err)
	}
	git := func(directory string, arguments ...string) string {
		t.Helper()
		result := procx.Run(procx.Command{Argv: append([]string{"git", "-C", directory}, arguments...)})
		if !result.OK() {
			t.Fatalf("git %v: %s", arguments, result.Output())
		}
		return strings.TrimSpace(result.Stdout)
	}
	bare := map[string]string{}
	for _, name := range []string{"origin", "gitea"} {
		bare[name] = filepath.Join(t.TempDir(), name+".git")
		git(filepath.Dir(bare[name]), "init", "--quiet", "--bare", "--initial-branch=main", bare[name])
	}
	git(f.root, "init", "--quiet", "--initial-branch=main")
	git(f.root, "config", "user.name", "UpKeeper Test")
	git(f.root, "config", "user.email", "upkeeper@example.invalid")
	git(f.root, "add", ".")
	git(f.root, "commit", "--quiet", "-m", "baseline")
	base := git(f.root, "rev-parse", "HEAD")
	for _, name := range []string{"origin", "gitea"} {
		git(f.root, "remote", "add", name, bare[name])
		git(f.root, "push", "--quiet", name, "HEAD:refs/heads/main")
	}
	// origin moves on; gitea is left a commit behind, as it once was.
	if err := os.WriteFile(filepath.Join(f.root, "notes"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(f.root, "add", "notes")
	git(f.root, "commit", "--quiet", "-m", "later")
	head := git(f.root, "rev-parse", "HEAD")
	git(f.root, "push", "--quiet", "origin", "HEAD:refs/heads/main")

	value, err := release.Load(f.root)
	if err != nil {
		t.Fatal(err)
	}
	value.Manifest["captured_from"] = "reviewed-controller"
	if _, err = release.Write(f.root, value); err != nil {
		t.Fatal(err)
	}
	options := f.options()
	options.Runner = func(command procx.Command) procx.Result {
		if len(command.Argv) > 0 && command.Argv[0] == "git" {
			return procx.Run(command)
		}
		return f.run(command)
	}
	options.Authored = true
	options.ExpectedHead = head
	options.ExpectedReleaseSHA256 = value.Digest()

	_, err = publish.Release(f.root, options)
	resync := "git push gitea refs/remotes/origin/main:refs/heads/main"
	if err == nil || !strings.Contains(err.Error(), "gitea at "+base) || !strings.Contains(err.Error(), resync) {
		t.Fatalf("lagging remote produced %v", err)
	}
	if got := git(f.root, "rev-parse", "HEAD"); got != head {
		t.Fatal("the refusal committed")
	}

	git(f.root, strings.Fields(resync)[1:]...)
	report, err := publish.Release(f.root, options)
	if err != nil {
		t.Fatalf("after resync: %v", err)
	}
	published := report["head"].(string)
	if published == head {
		t.Fatal("the reviewed release was not committed")
	}
	for name, path := range bare {
		if got := git(path, "rev-parse", "refs/heads/main"); got != published {
			t.Errorf("%s main = %s, want %s", name, got, published)
		}
	}

	// From a branch, nothing reaches either remote under any name.
	git(f.root, "switch", "--quiet", "-c", "rc119-release")
	value.Manifest["captured_from"] = "reviewed-again"
	if _, err = release.Write(f.root, value); err != nil {
		t.Fatal(err)
	}
	options.ExpectedHead = published
	options.ExpectedReleaseSHA256 = value.Digest()
	if _, err = publish.Release(f.root, options); err == nil ||
		!strings.Contains(err.Error(), "on branch rc119-release") {
		t.Fatalf("a branch publication produced %v", err)
	}
	for name, path := range bare {
		if heads := git(path, "for-each-ref", "--format=%(refname)", "refs/heads"); heads != "refs/heads/main" {
			t.Errorf("%s gained branches: %q", name, heads)
		}
		if got := git(path, "rev-parse", "refs/heads/main"); got != published {
			t.Errorf("%s main moved to %s", name, got)
		}
	}
}
