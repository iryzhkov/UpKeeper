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
	checkEnv []string
	// generatorInstalled is whether this host has the command that decides
	// whether it is converged. A host without it cannot be checked at all.
	generatorInstalled bool
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
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(subject.root, "README.md"), []byte("UpKeeper\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	subject.installGenerator(t, currentGenerator)

	previousValidator := release.EnvironmentValidator
	previousCapturer := release.EnvironmentCapturer
	release.EnvironmentValidator = func(map[string]any) error { return nil }
	release.EnvironmentCapturer = func(
		home string, provenance release.InstructionRef,
	) (map[string]any, error) {
		if subject.envErr != nil {
			return nil, subject.envErr
		}
		if provenance.Commit != rootCommit || provenance.Path != "README.md" {
			t.Errorf("the environment was captured with the wrong provenance: %#v", provenance)
		}
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
	case strings.HasSuffix(joined, "t3-steward version"):
		return procx.Result{Stdout: "t3-steward v0.10.1\ntested with T3 1.2.0..1.4.0\n"}
	case strings.HasPrefix(joined, "npm ls"):
		return procx.Result{Stdout: `{"dependencies":{"t3":{"version":"1.3.0"}}}`}
	case strings.HasSuffix(joined, "remote"):
		return procx.Result{Stdout: "origin\nforgejo\n"}
	case strings.Contains(joined, "diff --cached"):
		return procx.Result{Stdout: release.File + "\n"}
	case strings.HasSuffix(joined, "rev-parse --abbrev-ref HEAD"):
		return procx.Result{Stdout: "main\n"}
	case strings.HasSuffix(joined, "rev-parse HEAD"):
		if f.committed() {
			return procx.Result{Stdout: newCommit + "\n"}
		}
		return procx.Result{Stdout: rootCommit + "\n"}
	case strings.Contains(joined, " add "),
		strings.Contains(joined, " commit "),
		strings.Contains(joined, " push "):
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
