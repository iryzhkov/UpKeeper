package selfconv_test

import (
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
	if len(calls) != 2 || !strings.Contains(calls[1], "diff --quiet") {
		t.Fatalf("unexpected commands: %v", calls)
	}
	for _, path := range selfconv.ProgramPaths {
		if !strings.Contains(calls[1], path) {
			t.Errorf("the comparison does not cover %q: %s", path, calls[1])
		}
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
		for _, argument := range command.Argv {
			if argument == "diff" {
				return procx.Result{Code: 1}
			}
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
	if err := selfconv.VerifyProgram("/checkout", strings.Repeat("a", 40),
		strings.Repeat("b", 40), runner); err == nil {
		t.Fatal("a commit absent from the checkout was accepted")
	}
}

// A binary that cannot name a clean commit cannot prove anything about itself.
func TestAnUnidentifiedBinaryIsRefused(t *testing.T) {
	for _, value := range []string{"", "unknown", strings.Repeat("a", 40) + "-dirty"} {
		if err := selfconv.VerifyProgram("/checkout", value,
			strings.Repeat("b", 40), func(procx.Command) procx.Result {
				t.Error("ran git for a binary that names no commit")
				return procx.Result{}
			}); err == nil {
			t.Errorf("%q was accepted as a source commit", value)
		}
	}
}
