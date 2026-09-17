package release_test

import (
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

const historicalTestCommit = "3333333333333333333333333333333333333333"

// historicalGit answers the plumbing LoadHistorical runs for a checkout whose
// HEAD is one paired release with no companion files.
func historicalGit(t *testing.T, calls *[]string) procx.Runner {
	t.Helper()
	manifest := string(mustEncode(t, pairedManifestFixture(t)))
	return func(command procx.Command) procx.Result {
		joined := strings.Join(command.Argv, " ")
		*calls = append(*calls, joined)
		switch {
		case strings.Contains(joined, " rev-parse --verify "):
			if strings.HasSuffix(joined, " HEAD^{commit}") ||
				strings.HasSuffix(joined, " v0.1.8^{commit}") {
				return procx.Result{Stdout: historicalTestCommit + "\n"}
			}
			return procx.Result{Code: 128, Stderr: "fatal: Needed a single revision\n"}
		case strings.Contains(joined, " merge-base --is-ancestor "):
			return procx.Result{}
		case strings.HasSuffix(joined, " show "+historicalTestCommit+":"+release.File):
			return procx.Result{Stdout: manifest}
		default:
			return procx.Result{Code: 1, Stderr: "unscripted: " + joined}
		}
	}
}

// A release is named by anything Git resolves to a commit, and a refusal
// names the flag the operator actually typed.
func TestLoadHistoricalResolvesSymbolicRefsAndNamesTheFlag(t *testing.T) {
	acceptAnyEnvironment(t)
	calls := []string{}
	runner := historicalGit(t, &calls)

	for _, revision := range []string{"HEAD", "v0.1.8"} {
		_, resolved, _, err := release.LoadHistorical(t.TempDir(), revision, "--previous", runner)
		if err != nil {
			t.Fatalf("%s was refused: %v", revision, err)
		}
		if resolved != historicalTestCommit {
			t.Errorf("%s resolved to %q", revision, resolved)
		}
	}

	_, _, _, err := release.LoadHistorical(t.TempDir(), "nope", "--previous", runner)
	if err == nil || err.Error() != "--previous nope does not resolve to a commit in this checkout" {
		t.Fatalf("an unknown revision produced %v", err)
	}

	calls = nil
	_, _, _, err = release.LoadHistorical(t.TempDir(), "--help", "--to", runner)
	if err == nil || err.Error() != "--to must name a commit: a hash, tag, branch or symbolic ref such as HEAD" {
		t.Fatalf("an option-shaped revision produced %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("an option-shaped revision reached git: %v", calls)
	}
}

func TestLoadHistoricalRefusesACommitOutsideTheCheckoutHistory(t *testing.T) {
	acceptAnyEnvironment(t)
	calls := []string{}
	inner := historicalGit(t, &calls)
	runner := func(command procx.Command) procx.Result {
		if strings.Contains(strings.Join(command.Argv, " "), " merge-base --is-ancestor ") {
			return procx.Result{Code: 1}
		}
		return inner(command)
	}
	_, _, _, err := release.LoadHistorical(t.TempDir(), "HEAD", "--to", runner)
	want := "--to HEAD (" + historicalTestCommit + ") is not in this checkout's history"
	if err == nil || err.Error() != want {
		t.Fatalf("produced %v, expected %q", err, want)
	}
}
