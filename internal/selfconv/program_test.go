package selfconv_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
)

// A binary older than the checkout is the steady state: main advances on every
// release capture without the Go program changing. Refusing that would stop
// every host from converging.
func TestAProgramThatDidNotChangeIsAccepted(t *testing.T) {
	calls := []string{}
	runner := func(command procx.Command) procx.Result {
		calls = append(calls, strings.Join(command.Argv, " "))
		return procx.Result{Code: 0}
	}
	if err := selfconv.VerifyProgram("/checkout", strings.Repeat("a", 40),
		strings.Repeat("b", 40), runner); err != nil {
		t.Fatalf("refused an unchanged program: %v", err)
	}
	if len(calls) != 2 || !strings.Contains(calls[1], " diff ") {
		t.Fatalf("unexpected commands: %v", calls)
	}
	// A compiled input can live anywhere, so the comparison names no paths.
	if !strings.HasSuffix(calls[1], " --") {
		t.Errorf("the comparison does not cover the whole repository: %s", calls[1])
	}
}

// The same commit needs no comparison at all.
func TestTheSameCommitAsksGitNothing(t *testing.T) {
	commit := strings.Repeat("c", 40)
	called := false
	runner := func(procx.Command) procx.Result {
		called = true
		return procx.Result{Code: 0}
	}
	if err := selfconv.VerifyProgram("/checkout", commit, commit, runner); err != nil {
		t.Fatalf("refused its own commit: %v", err)
	}
	if called {
		t.Error("ran git for a commit that is already the checkout's")
	}
}

// This is the invariant the Python launcher got for free by being the
// checkout: the program that applies a release must be the program that was
// gated with it.
func TestAChangedProgramIsRefusedAndNamesTheRemedy(t *testing.T) {
	checkout := strings.Repeat("b", 40)
	runner := func(command procx.Command) procx.Result {
		if slices.Contains(command.Argv, "--name-only") {
			return procx.Result{Code: 1, Stdout: "internal/a.go\x00"}
		}
		return procx.Result{Code: 0}
	}
	err := selfconv.VerifyProgram("/checkout", strings.Repeat("a", 40), checkout, runner)
	if err == nil {
		t.Fatal("a changed program was accepted")
	}
	if !strings.Contains(err.Error(), checkout) {
		t.Errorf("the refusal does not name the commit to install: %v", err)
	}
	if !errors.Is(err, selfconv.ErrProgramDiffers) {
		t.Errorf("a changed program is not marked as one a self-update resolves: %v", err)
	}
}

// A dependency-only change rebuilds a different binary, so go.sum is part of
// the program.
func TestTheProgramIncludesTheDependencySums(t *testing.T) {
	for _, path := range []string{"go.mod", "go.sum", "cmd", "internal"} {
		if !slices.Contains(selfconv.ProgramPaths, path) {
			t.Errorf("ProgramPaths omits %s: %v", path, selfconv.ProgramPaths)
		}
	}
}

// Tests and their fixtures are not build inputs, so the comparison ignores
// them: a test-only commit must not make hosts refuse to converge. A Go file
// a program imports from a testdata directory, or a fixture it embeds, is
// compiled into the binary and so changes the program.
func TestTheProgramComparisonIgnoresOnlyWhatIsNotCompiled(t *testing.T) {
	root, commits := programRepository(t,
		map[string]string{
			"go.mod": "module x\n", "go.sum": "",
			"cmd/main.go":                        "package main\n\nimport _ \"x/internal/testdata/helper\"\n",
			"internal/testdata/helper/helper.go": "package helper\n\nconst Value = 1\n",
			"internal/a/a.go":                    "package a\n\nimport _ \"embed\"\n\n//go:embed testdata/embedded.txt\nvar Data string\n",
			"internal/a/a_test.go":               "package a\n",
			"internal/a/testdata/embedded.txt":   "one\n",
			"internal/a/testdata/fixture.json":   "{}\n",
		},
		map[string]string{"internal/a/a_test.go": "package a // changed\n"},
		map[string]string{"internal/a/testdata/fixture.json": "{\"changed\": true}\n"},
		map[string]string{"internal/testdata/helper/helper.go": "package helper\n\nconst Value = 2\n"},
		map[string]string{"internal/a/testdata/embedded.txt": "two\n"},
	)
	for index, what := range []string{"a changed test", "a changed test fixture"} {
		if err := selfconv.VerifyProgram(root, commits[index], commits[index+1], nil); err != nil {
			t.Errorf("%s changed the program: %v", what, err)
		}
	}
	for index, what := range []string{"an imported helper under testdata", "an embedded fixture"} {
		err := selfconv.VerifyProgram(root, commits[index+2], commits[index+3], nil)
		if !errors.Is(err, selfconv.ErrProgramDiffers) {
			t.Errorf("%s kept the program: %v", what, err)
		}
	}
}

// programRepository builds a throwaway repository with one commit per change
// and returns its root and the commits, oldest first.
func programRepository(t *testing.T, changes ...map[string]string) (string, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	git := func(arguments ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{
			"-C", root, "-c", "gc.auto=0", "-c", "maintenance.auto=false",
			"-c", "commit.gpgsign=false", "-c", "user.name=test", "-c", "user.email=test@invalid",
		}, arguments...)...)
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet", "--initial-branch=main")
	commits := []string{}
	for _, change := range changes {
		for name, content := range change {
			path := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		git("add", "--all")
		git("commit", "--quiet", "--allow-empty", "-m", "change")
		commits = append(commits, git("rev-parse", "HEAD"))
	}
	return root, commits
}

// A comparison git could not perform proves nothing, and in particular does
// not prove that the program changed: only a changed program may trigger a
// self-update.
func TestAFailedComparisonIsNotAChangedProgram(t *testing.T) {
	runner := func(command procx.Command) procx.Result {
		if slices.Contains(command.Argv, "diff") {
			return procx.Result{Code: 128}
		}
		return procx.Result{Code: 0}
	}
	err := selfconv.VerifyProgram("/checkout", strings.Repeat("a", 40), strings.Repeat("b", 40), runner)
	if err == nil || errors.Is(err, selfconv.ErrProgramDiffers) {
		t.Fatalf("a failed comparison produced %v", err)
	}
}

// A commit the checkout has never heard of cannot be compared, so it cannot be
// trusted either.
func TestAnUnknownBinaryCommitIsRefused(t *testing.T) {
	runner := func(command procx.Command) procx.Result {
		for _, argument := range command.Argv {
			if argument == "cat-file" {
				return procx.Result{Code: 128}
			}
		}
		return procx.Result{Code: 0}
	}
	err := selfconv.VerifyProgram("/checkout", strings.Repeat("a", 40), strings.Repeat("b", 40), runner)
	if err == nil {
		t.Fatal("a commit absent from the checkout was accepted")
	}
	if errors.Is(err, selfconv.ErrProgramDiffers) {
		t.Error("a binary the checkout cannot compare is marked as a changed program")
	}
}

// A binary that cannot name a clean commit cannot prove anything about itself.
func TestAnUnidentifiedBinaryIsRefused(t *testing.T) {
	for _, value := range []string{"", "unknown", strings.Repeat("a", 40) + "-dirty"} {
		if err := selfconv.VerifyProgram("/checkout", value,
			strings.Repeat("b", 40), func(procx.Command) procx.Result {
				t.Error("ran git for a binary that names no commit")
				return procx.Result{}
			}); err == nil || errors.Is(err, selfconv.ErrProgramDiffers) {
			t.Errorf("%q was accepted as a source commit, or as one to self-update: %v", value, err)
		}
	}
}
