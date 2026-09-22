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

const (
	setupCommit  = "1111111111111111111111111111111111111111"
	huyangCommit = "2222222222222222222222222222222222222222"
	rootCommit   = "3333333333333333333333333333333333333333"
	newCommit    = "4444444444444444444444444444444444444444"
)

// fixture is a controller whose machine is in exactly the state a release
// capture expects, with every external command answered from a table so that
// the test never touches Git, npm or the network.
type fixture struct {
	root string
	home string
	// huyangHead is what the controller's own Huyang checkout is on. It is a
	// field rather than a constant so that a test can move the controller
	// ahead of the release and watch what a capture does with that.
	huyangHead string
	calls      []string
	envErr     error
	// checkCode, checkStdout and checkErr are what the installed
	// agents-instructions-gen --check answers. The default is a converged host,
	// because that is the only state from which a release may be captured.
	checkCode   int
	checkStdout string
	checkErr    error
	// checkEnv is the environment the check was actually run with. The answer
	// depends on it: a check run against the wrong home reports on the wrong
	// host, and one that inherits OMARCHY_SETUP_ROOT reports against whatever
	// tree the caller's shell happened to name.
	checkEnv                 []string
	capturedRepositorySkills string
	// generatorInstalled is whether this host has the command that decides
	// whether it is converged. A host without it cannot be checked at all.
	generatorInstalled bool
	failRemote         string
	historicalManifest string
	onAdd              func()
	headChanged        bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	subject := &fixture{
		root:               filepath.Join(base, "checkout"),
		home:               filepath.Join(base, "home"),
		huyangHead:         huyangCommit,
		checkStdout:        convergedCheckOutput,
		generatorInstalled: true,
	}
	for _, directory := range []string{
		filepath.Join(subject.home, ".local/share/omarchy-setup/.git"),
		filepath.Join(subject.home, ".local/share/huyang/.git"),
		filepath.Join(subject.home, ".local/bin"),
		subject.root,
		filepath.Join(subject.root, ".git"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(subject.root, "README.md"), []byte("UpKeeper\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subject.root, ".git/index"), []byte("fixture-index"), 0o600); err != nil {
		t.Fatalf("WriteFile index: %v", err)
	}
	subject.installGenerator(t, currentGenerator)

	previousValidator := release.EnvironmentValidator
	previousCapturer := release.EnvironmentCapturer
	release.EnvironmentValidator = func(map[string]any) error { return nil }
	release.EnvironmentCapturer = func(
		home, repositorySkills string, provenance release.InstructionRef, previous map[string]any,
	) (map[string]any, error) {
		if subject.envErr != nil {
			return nil, subject.envErr
		}
		if provenance.Commit != rootCommit || provenance.Path != "README.md" {
			t.Errorf("the environment was captured with the wrong provenance: %#v", provenance)
		}
		subject.capturedRepositorySkills = repositorySkills
		return map[string]any{"kind": "agent-environment", "files": []any{}}, nil
	}
	t.Cleanup(func() {
		release.EnvironmentValidator = previousValidator
		release.EnvironmentCapturer = previousCapturer
	})
	return subject
}

// convergedCheckOutput is what agents-instructions-gen --check prints on a host
// that still holds what its omarchy-setup checkout generates.
const convergedCheckOutput = "agents-instructions-gen: the installed agent " +
	"instructions match /home/igor/.local/share/omarchy-setup\n"

// currentGenerator stands in for an installed agents-instructions-gen that
// understands being asked a question. It is never executed: every command in
// these tests is answered from the table in run. What it has to carry is the
// evidence the gate looks for before it runs anything, because the copy that
// predates the check ignores its arguments and rewrites the host's instruction
// files instead of reporting on them.
const currentGenerator = "#!/usr/bin/env bash\n" +
	"# Usage: agents-instructions-gen [--root DIR] [--check]\n" +
	"exit 0\n"

// legacyGenerator is the copy that predates the check: it takes no options and
// writes ~/.config/agents unconditionally.
const legacyGenerator = "#!/usr/bin/env bash\n" +
	"out=\"$HOME/.config/agents\"\nmkdir -p \"$out\"\n"

// installGenerator puts an executable agents-instructions-gen on the fixture
// host. A host without one is a host whose instructions cannot be checked.
func (f *fixture) installGenerator(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(f.home, ".local/bin/agents-instructions-gen")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f.generatorInstalled = true
}

// removeGenerator takes the check away, which is the degraded case: the capture
// cannot find out whether this host agrees with its source.
func (f *fixture) removeGenerator(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.home, ".local/bin/agents-instructions-gen")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	f.generatorInstalled = false
}

// run answers every command a capture and a publication issue.
func (f *fixture) run(command procx.Command) procx.Result {
	joined := strings.Join(command.Argv, " ")
	f.calls = append(f.calls, joined)
	switch {
	case strings.Contains(joined, "agents-instructions-gen"):
		f.checkEnv = command.Env
		if !f.generatorInstalled {
			f.calls = append(f.calls, "UNEXPECTED "+joined)
			return procx.Result{Code: 1, Stderr: "the gate ran a command that is not installed"}
		}
		return procx.Result{Code: f.checkCode, Stdout: f.checkStdout, Err: f.checkErr}
	case strings.HasSuffix(joined, "status --porcelain"):
		return procx.Result{}
	case strings.Contains(joined, "omarchy-setup rev-parse HEAD"):
		return procx.Result{Stdout: setupCommit + "\n"}
	case strings.Contains(joined, "huyang rev-parse HEAD"):
		return procx.Result{Stdout: f.huyangHead + "\n"}
	case strings.HasSuffix(joined, "fetch --quiet origin main"),
		strings.Contains(joined, "merge-base --is-ancestor"):
		return procx.Result{}
	case strings.Contains(joined, "rev-parse --verify --end-of-options"):
		return procx.Result{Stdout: rootCommit + "\n"}
	case strings.Contains(joined, " read-tree "), strings.Contains(joined, " update-ref "):
		return procx.Result{}
	case strings.Contains(joined, " write-tree"):
		return procx.Result{Stdout: strings.Repeat("5", 40) + "\n"}
	case strings.Contains(joined, " commit-tree "):
		return procx.Result{Stdout: newCommit + "\n"}
	case strings.Contains(joined, " show :") && strings.HasSuffix(joined, release.File):
		return procx.Result{Stdout: f.historicalManifest}
	case strings.Contains(joined, " show ") && strings.HasSuffix(joined, ":"+release.File):
		return procx.Result{Stdout: f.historicalManifest}
	case strings.HasSuffix(joined, "t3-steward version"):
		return procx.Result{Stdout: "t3-steward v0.10.1\ntested with T3 1.2.0..1.4.0\n"}
	case strings.HasPrefix(joined, "npm ls"):
		return procx.Result{Stdout: `{"dependencies":{"t3":{"version":"1.3.0"}}}`}
	case strings.HasSuffix(joined, "remote"):
		return procx.Result{Stdout: "origin\nforgejo\nbackup\n"}
	case strings.Contains(joined, "diff --cached"):
		if command.Env == nil && strings.Contains(joined, rootCommit) {
			return procx.Result{}
		}
		return procx.Result{Stdout: release.File + "\n"}
	case strings.HasSuffix(joined, "symbolic-ref --quiet HEAD"):
		return procx.Result{Stdout: "refs/heads/main\n"}
	case strings.HasSuffix(joined, "rev-parse --git-path index"):
		return procx.Result{Stdout: ".git/index\n"}
	case strings.Contains(joined, " ls-files --stage -- "):
		return procx.Result{Stdout: "100644 " + strings.Repeat("6", 40) + " 0\tfile\n"}
	case strings.Contains(joined, " reset --quiet "):
		return procx.Result{}
	case strings.HasSuffix(joined, "rev-parse --abbrev-ref HEAD"):
		return procx.Result{Stdout: "main\n"}
	case strings.HasSuffix(joined, "rev-parse HEAD"):
		if f.headChanged || f.committed() {
			return procx.Result{Stdout: newCommit + "\n"}
		}
		return procx.Result{Stdout: rootCommit + "\n"}
	case strings.Contains(joined, " push "):
		if f.failRemote != "" && strings.Contains(joined, " "+f.failRemote+" ") {
			return procx.Result{Code: 1, Stderr: "remote refused"}
		}
		return procx.Result{}
	case strings.Contains(joined, " add "):
		if f.onAdd != nil {
			f.onAdd()
			f.onAdd = nil
		}
		return procx.Result{}
	case strings.Contains(joined, " commit "):
		return procx.Result{}
	default:
		f.calls = append(f.calls, "UNEXPECTED "+joined)
		return procx.Result{Code: 1, Stderr: "unexpected command: " + joined}
	}
}

func (f *fixture) committed() bool { return f.did(" commit ") }

func (f *fixture) did(fragment string) bool {
	for _, call := range f.calls {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

func (f *fixture) options() publish.Options {
	return publish.Options{
		Controller: "normandy",
		Remotes:    []string{"origin"},
		Commit:     true,
		Push:       true,
		Home:       f.home,
		Runner:     f.run,
	}
}

// This is the regression test the migration plan asks for: --no-commit is
// documented as "write the manifest only", so it must write the candidate
// manifest and touch neither the index, the history nor any remote.
func TestManifestOnlyRunNeitherCommitsNorPushes(t *testing.T) {
	subject := newFixture(t)
	options := subject.options()
	options.Commit = false
	options.Push = false

	report, err := publish.Release(subject.root, options)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	for _, forbidden := range []string{" add ", " commit ", " push "} {
		if subject.did(forbidden) {
			t.Errorf("a manifest-only run ran git%s", forbidden)
		}
	}
	if _, err := os.Stat(filepath.Join(subject.root, release.File)); err != nil {
		t.Errorf("the candidate manifest was not written: %v", err)
	}
	if report["committed"] != nil {
		t.Errorf("committed is %v, expected nothing", report["committed"])
	}
	if pushed, _ := report["pushed"].([]string); len(pushed) != 0 {
		t.Errorf("pushed is %v, expected nothing", pushed)
	}
	// The text report says nothing about commits that were never considered,
	// which is why "head" stays out of a manifest-only report.
	if _, present := report["head"]; present {
		t.Error("a manifest-only report must not claim a head commit")
	}
	if report["release_sha256"] == "" {
		t.Error("the candidate release has no identity")
	}
}

func TestNoPushCommitsWithoutPublishing(t *testing.T) {
	subject := newFixture(t)
	options := subject.options()
	options.Push = false

	report, err := publish.Release(subject.root, options)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !subject.did(" commit ") {
		t.Error("--no-push must still commit")
	}
	if subject.did(" push ") {
		t.Error("--no-push pushed anyway")
	}
	if report["committed"] != newCommit {
		t.Errorf("committed is %v", report["committed"])
	}
}

func TestAFullRunCommitsAndPushesEveryNamedRemote(t *testing.T) {
	subject := newFixture(t)
	options := subject.options()
	options.Remotes = []string{"origin", "forgejo"}

	report, err := publish.Release(subject.root, options)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	pushed, _ := report["pushed"].([]string)
	if len(pushed) != 2 || pushed[0] != "origin main" || pushed[1] != "forgejo main" {
		t.Errorf("pushed is %v", pushed)
	}
	if !subject.did("push --quiet origin HEAD:refs/heads/main") {
		t.Error("the branch was not pushed by its full ref")
	}
}

// A typo in a remote name used to be discovered only on the run that would have
// published, because the check came after the capture. It is now a preflight,
// so it costs nothing and cannot be reached by a manifest-only run either.
func TestAnUnknownRemoteFailsBeforeAnythingIsCaptured(t *testing.T) {
	for _, name := range []string{"full run", "manifest only"} {
		t.Run(name, func(t *testing.T) {
			subject := newFixture(t)
			options := subject.options()
			options.Remotes = []string{"origin", "typo"}
			if name == "manifest only" {
				options.Commit, options.Push = false, false
			}
			_, err := publish.Release(subject.root, options)
			if err == nil || err.Error() != "unknown git remote(s): typo" {
				t.Fatalf("produced %v", err)
			}
			if _, statErr := os.Stat(
				filepath.Join(subject.root, release.File)); statErr == nil {
				t.Error("a refused publication still wrote a manifest")
			}
		})
	}
}

func TestNothingNewToCommitLeavesTheReportHonest(t *testing.T) {
	subject := newFixture(t)
	subject.calls = nil
	options := subject.options()
	options.Push = false
	// An empty staged diff means the capture proposed exactly what is already
	// published, so there is nothing to record.
	runner := func(command procx.Command) procx.Result {
		if strings.Contains(strings.Join(command.Argv, " "), "diff --cached") {
			subject.calls = append(subject.calls, "diff --cached")
			return procx.Result{}
		}
		return subject.run(command)
	}
	options.Runner = runner
	report, err := publish.Release(subject.root, options)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if report["committed"] != nil {
		t.Errorf("committed is %v, expected nothing", report["committed"])
	}
	if report["head"] != rootCommit {
		t.Errorf("head is %v", report["head"])
	}
}

func TestADirtyReleaseRepositoryIsRefused(t *testing.T) {
	subject := newFixture(t)
	options := subject.options()
	options.Runner = func(command procx.Command) procx.Result {
		joined := strings.Join(command.Argv, " ")
		if joined == "git -C "+subject.root+" status --porcelain" {
			return procx.Result{Stdout: " M desired/release.json\n"}
		}
		return subject.run(command)
	}
	_, err := publish.Release(subject.root, options)
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("produced %v", err)
	}
}

func prepareAuthoredFixture(t *testing.T) (*fixture, string, []byte) {
	t.Helper()
	f := newFixture(t)
	for _, path := range []string{"inventory/hosts.yml", "desired/components.yml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(f.root, path)
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(target, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	options := f.options()
	options.Commit = false
	options.Push = false
	if _, err := publish.Release(f.root, options); err != nil {
		t.Fatal(err)
	}
	value, err := release.Load(f.root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.root, release.File))
	if err != nil {
		t.Fatal(err)
	}
	f.historicalManifest = string(raw)
	f.calls = nil
	return f, value.Digest(), raw
}

func TestAuthoredPublicationKeepsReviewedBytesAndRequiresBothFences(t *testing.T) {
	f, digest, before := prepareAuthoredFixture(t)
	extra := filepath.Join(f.root, release.AgentConfigDir, "unreferenced.txt")
	if err := os.MkdirAll(filepath.Dir(extra), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extra, []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := f.options()
	options.Authored = true
	options.ExpectedHead = rootCommit
	options.ExpectedReleaseSHA256 = digest
	report, err := publish.Release(f.root, options)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(f.root, release.File))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("authored publication rewrote reviewed release bytes or captured_at")
	}
	if report["release_sha256"] != digest {
		t.Fatalf("release digest = %#v", report["release_sha256"])
	}
	for _, call := range f.calls {
		if strings.Contains(call, "agents-instructions-gen") || strings.Contains(call, "omarchy-setup rev-parse") {
			t.Fatalf("authored publication recaptured host state: %s", call)
		}
		if strings.Contains(call, " add ") && (strings.Contains(call, "unreferenced.txt") || strings.HasSuffix(call, " "+release.AgentConfigDir)) {
			t.Fatalf("authored publication swept an unreferenced asset: %s", call)
		}
	}
	for _, mutate := range []publish.Options{
		func() publish.Options { bad := options; bad.ExpectedHead = newCommit; return bad }(),
		func() publish.Options {
			bad := options
			bad.ExpectedReleaseSHA256 = strings.Repeat("0", 64)
			return bad
		}(),
		func() publish.Options { bad := options; bad.Components = []string{"t3-steward"}; return bad }(),
	} {
		f.calls = nil
		if _, err = publish.Release(f.root, mutate); err == nil {
			t.Fatal("invalid authored fence was accepted")
		}
		if f.did(" add ") || f.did(" commit ") || f.did(" push ") {
			t.Fatalf("refused authored publication mutated Git: %v", f.calls)
		}
	}
}

func TestAuthoredPublicationRefusesChangedReviewedReleaseBeforeGitMutation(t *testing.T) {
	f, digest, _ := prepareAuthoredFixture(t)
	value, err := release.Load(f.root)
	if err != nil {
		t.Fatal(err)
	}
	value.Manifest["captured_from"] = "changed-after-review"
	if _, err = release.Write(f.root, value); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	options := f.options()
	options.Authored = true
	options.ExpectedHead = rootCommit
	options.ExpectedReleaseSHA256 = digest
	if _, err = publish.Release(f.root, options); err == nil {
		t.Fatal("changed reviewed release was accepted")
	}
	if f.did(" add ") || f.did(" commit ") || f.did(" push ") {
		t.Fatalf("digest refusal mutated Git: %v", f.calls)
	}
}

func TestAuthoredPublicationRefusesBytesChangedAtStagingBoundary(t *testing.T) {
	f, digest, _ := prepareAuthoredFixture(t)
	options := f.options()
	options.Authored = true
	options.ExpectedHead = rootCommit
	options.ExpectedReleaseSHA256 = digest
	f.onAdd = func() {
		value, err := release.Load(f.root)
		if err != nil {
			t.Fatal(err)
		}
		value.Manifest["captured_from"] = "changed-during-add"
		raw, err := value.CanonicalBytes()
		if err != nil {
			t.Fatal(err)
		}
		f.historicalManifest = string(raw)
	}
	if _, err := publish.Release(f.root, options); err == nil ||
		!strings.Contains(err.Error(), "changed while staging") {
		t.Fatalf("staging race produced %v", err)
	}
	if f.did(" update-ref ") || f.did(" push ") {
		t.Fatalf("staging race advanced or pushed a ref: %v", f.calls)
	}
}

func TestAuthoredPublicationRefusesHeadChangedBeforeCAS(t *testing.T) {
	f, digest, _ := prepareAuthoredFixture(t)
	options := f.options()
	options.Authored = true
	options.ExpectedHead = rootCommit
	options.ExpectedReleaseSHA256 = digest
	f.onAdd = func() { f.headChanged = true }
	if _, err := publish.Release(f.root, options); err == nil ||
		!strings.Contains(err.Error(), "HEAD changed") {
		t.Fatalf("HEAD race produced %v", err)
	}
	if f.did(" update-ref ") || f.did(" push ") {
		t.Fatalf("HEAD race advanced or pushed a ref: %v", f.calls)
	}
}

func TestAuthoredPublicationRealGitReconcilesOwnedIndexOnly(t *testing.T) {
	f, _, _ := prepareAuthoredFixture(t)
	if err := os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
		t.Fatal(err)
	}
	runGit := func(arguments ...string) string {
		t.Helper()
		result := procx.Run(procx.Command{Argv: append([]string{"git", "-C", f.root}, arguments...)})
		if !result.OK() {
			t.Fatalf("git %v: %s", arguments, result.Output())
		}
		return strings.TrimSpace(result.Stdout)
	}
	runGit("init", "--quiet")
	runGit("config", "user.name", "UpKeeper Test")
	runGit("config", "user.email", "upkeeper@example.invalid")
	if err := os.WriteFile(filepath.Join(f.root, "unrelated"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "--quiet", "-m", "baseline")
	head := runGit("rev-parse", "HEAD")

	value, err := release.Load(f.root)
	if err != nil {
		t.Fatal(err)
	}
	value.Manifest["captured_from"] = "reviewed-controller"
	if _, err = release.Write(f.root, value); err != nil {
		t.Fatal(err)
	}
	digest := value.Digest()
	if err = os.WriteFile(filepath.Join(f.root, "unrelated"), []byte("staged-user-work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "unrelated")

	runner := func(command procx.Command) procx.Result {
		if len(command.Argv) > 0 && command.Argv[0] == "git" {
			return procx.Run(command)
		}
		return f.run(command)
	}
	options := f.options()
	options.Runner = runner
	options.Remotes = nil
	options.Push = false
	options.Authored = true
	options.ExpectedHead = head
	options.ExpectedReleaseSHA256 = digest
	if _, err = publish.Release(f.root, options); err != nil {
		t.Fatal(err)
	}
	if got := runGit("diff", "--cached", "--name-only"); got != "unrelated" {
		t.Fatalf("staged paths after publish = %q", got)
	}
	if got := runGit("diff", "--name-only"); got != "" {
		t.Fatalf("working paths after publish = %q", got)
	}
	if got := runGit("show", "HEAD:"+release.File); got != strings.TrimSpace(string(mustRead(t, filepath.Join(f.root, release.File)))) {
		t.Fatal("committed release differs from reviewed working bytes")
	}

	secondHead := runGit("rev-parse", "HEAD")
	options.ExpectedHead = secondHead
	if _, err = publish.Release(f.root, options); err != nil {
		t.Fatalf("no-change authored publication: %v", err)
	}
	if got := runGit("rev-parse", "HEAD"); got != secondHead {
		t.Fatal("no-change publication moved HEAD")
	}
	if got := runGit("diff", "--cached", "--name-only"); got != "unrelated" {
		t.Fatalf("no-change publication disturbed staging: %q", got)
	}

	value, err = release.Load(f.root)
	if err != nil {
		t.Fatal(err)
	}
	value.Manifest["captured_from"] = "pre-staged-owned-path"
	if _, err = release.Write(f.root, value); err != nil {
		t.Fatal(err)
	}
	options.ExpectedReleaseSHA256 = value.Digest()
	runGit("add", release.File)
	if _, err = publish.Release(f.root, options); err == nil ||
		!strings.Contains(err.Error(), "already have staged changes") {
		t.Fatalf("pre-staged owned path produced %v", err)
	}
	if got := runGit("rev-parse", "HEAD"); got != secondHead {
		t.Fatal("pre-staged refusal moved HEAD")
	}
	staged := strings.Split(runGit("diff", "--cached", "--name-only"), "\n")
	if len(staged) != 2 || staged[0] != release.File || staged[1] != "unrelated" {
		t.Fatalf("pre-staged refusal changed index: %#v", staged)
	}

	runGit("reset", "--quiet", "HEAD", "--", release.File)
	target := filepath.Join(f.root, "desired", "release-target.json")
	reviewed := mustRead(t, filepath.Join(f.root, release.File))
	if err = os.WriteFile(target, reviewed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(f.root, release.File)); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("release-target.json", filepath.Join(f.root, release.File)); err != nil {
		t.Fatal(err)
	}
	if _, err = publish.Release(f.root, options); err == nil ||
		!strings.Contains(err.Error(), "requires a regular 100644 file") {
		t.Fatalf("symlinked reviewed manifest produced %v", err)
	}
	if got := runGit("rev-parse", "HEAD"); got != secondHead {
		t.Fatal("mode refusal moved HEAD")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAuthoredPublicationRealGitNoChangeRefusesUnsafeMode(t *testing.T) {
	f, _, _ := prepareAuthoredFixture(t)
	value, err := release.Load(f.root)
	if err != nil {
		t.Fatal(err)
	}
	components := value.Manifest["components"].(map[string]any)
	agentConfig := components["agent-config"].(map[string]any)
	agentConfig["files"] = []any{map[string]any{
		"path": ".config/agents/link", "store": "link",
		"sha256": "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
	}}
	delete(components, "agent-environment")
	delete(components, "t3")
	delete(components, "worker-configuration")
	components["t3-steward"].(map[string]any)["installer"] = "t3-update"
	delete(components["omarchy-setup"].(map[string]any), "modules")
	if _, err = release.Write(f.root, value); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(f.root, release.AgentConfigDir)
	if err = os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(storeDir, "foo"), []byte("foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("foo", filepath.Join(storeDir, "link")); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
		t.Fatal(err)
	}
	runGit := func(arguments ...string) string {
		t.Helper()
		result := procx.Run(procx.Command{Argv: append([]string{"git", "-C", f.root}, arguments...)})
		if !result.OK() {
			t.Fatalf("git %v: %s", arguments, result.Output())
		}
		return strings.TrimSpace(result.Stdout)
	}
	runGit("init", "--quiet")
	runGit("config", "user.name", "UpKeeper Test")
	runGit("config", "user.email", "upkeeper@example.invalid")
	runGit("add", ".")
	runGit("commit", "--quiet", "-m", "unsafe baseline")
	head := runGit("rev-parse", "HEAD")
	runner := func(command procx.Command) procx.Result {
		if len(command.Argv) > 0 && command.Argv[0] == "git" {
			if len(command.Argv) > 3 && command.Argv[3] == "push" {
				t.Fatal("unsafe no-change release attempted a push")
			}
			return procx.Run(command)
		}
		return f.run(command)
	}
	options := f.options()
	options.Runner = runner
	options.Remotes = nil
	options.Authored = true
	options.ExpectedHead = head
	options.ExpectedReleaseSHA256 = value.Digest()
	if _, err = publish.Release(f.root, options); err == nil ||
		!strings.Contains(err.Error(), "requires a regular 100644 file") {
		t.Fatalf("unsafe no-change release produced %v", err)
	}
	if got := runGit("rev-parse", "HEAD"); got != head {
		t.Fatal("unsafe no-change refusal moved HEAD")
	}
}

func TestPartialPushFailureReturnsReceipts(t *testing.T) {
	f := newFixture(t)
	options := f.options()
	options.Remotes = []string{"origin", "forgejo", "backup"}
	f.failRemote = "forgejo"
	report, err := publish.Release(f.root, options)
	if err == nil || report == nil {
		t.Fatal("partial failure discarded its receipt")
	}
	pushed := report["pushed"].([]string)
	if len(pushed) != 1 || pushed[0] != "origin main" {
		t.Fatalf("successful pushes = %#v", pushed)
	}
	receipts := report["push_receipts"].([]map[string]any)
	if len(receipts) != 3 || receipts[0]["status"] != "pushed" || receipts[1]["status"] != "failed" || receipts[2]["status"] != "not-attempted" {
		t.Fatalf("receipts = %#v", receipts)
	}
}

func TestCapturedManifestNamesTheControllerAndThePair(t *testing.T) {
	subject := newFixture(t)
	if _, err := publish.Release(subject.root, subject.options()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	value, err := release.Load(subject.root)
	if err != nil {
		t.Fatalf("the captured manifest does not load back: %v", err)
	}
	if value.Manifest["captured_from"] != "normandy" {
		t.Errorf("captured_from is %v", value.Manifest["captured_from"])
	}
	if value.Component("huyang")["commit"] != huyangCommit {
		t.Errorf("the huyang pin was not captured: %v", value.Component("huyang"))
	}
	if value.Component("t3")["version"] != "1.3.0" {
		t.Errorf("the T3 pin was not derived from the installed steward: %v",
			value.Component("t3"))
	}
	if value.Component("t3-steward")["installer"] != "upkeeper" {
		t.Error("a paired release must be installed by UpKeeper itself")
	}
	modules, _ := value.Component("omarchy-setup")["modules"].([]any)
	if len(modules) != 1 || modules[0] != "28-scripts" {
		t.Errorf("the retired agents module was not dropped: %v", modules)
	}
}
