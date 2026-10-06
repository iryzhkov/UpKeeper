package selfconv

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// Origins are the only remotes a self-refresh will pull release intent from.
var Origins = map[string]bool{
	"https://github.com/iryzhkov/UpKeeper.git": true,
	"git@github.com:iryzhkov/UpKeeper.git":     true,
}

// Refresh fast-forwards the local release checkout to the newest main commit
// that has already passed its published gate, and returns that commit.
//
// Every step is a fence. The branch, the remote and the ancestry are checked
// before anything moves; the candidate commit must have a *successful, already
// completed* CI run of its own; and the checkout must still be exactly where it
// was when the run started, otherwise somebody else is working in it and the
// refresh preserves their work instead of overwriting it.
func Refresh(root string, runner procx.Runner) (string, error) {
	if runner == nil {
		runner = procx.Run
	}
	// A binary converging after a self-update holds this lock, so that the
	// commit it checked stays HEAD until it has converged it.
	unlock, err := LockCheckout(root, 0)
	if err != nil {
		return "", err
	}
	defer unlock()
	before, err := CheckoutRevision(root, runner)
	if err != nil {
		return "", err
	}
	environment := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=yes",
	)
	command := func(argv []string) (string, error) {
		result := runner(procx.Command{Argv: argv, Env: environment, Timeout: 120 * time.Second})
		if result.Err != nil || result.Code != 0 {
			label := argv[0]
			if len(argv) > 1 {
				label += " " + argv[1]
			}
			return "", failure.Value("self refresh failed: %s", label)
		}
		return strings.TrimSpace(result.Stdout), nil
	}
	git := func(arguments ...string) (string, error) {
		return command(append([]string{"git", "-C", root}, arguments...))
	}
	branch, err := git("branch", "--show-current")
	if err != nil {
		return "", err
	}
	if branch != "main" {
		return "", failure.Value("self refresh requires main")
	}
	origin, err := git("remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	if !Origins[origin] {
		return "", failure.Value("self refresh requires the canonical UpKeeper GitHub origin")
	}
	if _, err := git("fetch", "--quiet", "--no-tags", "origin", "refs/heads/main"); err != nil {
		return "", err
	}
	candidate, err := git("rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", err
	}
	if _, err := git("merge-base", "--is-ancestor", before, candidate); err != nil {
		return "", err
	}
	gateOutput, err := command([]string{
		"gh", "run", "list", "--repo", "iryzhkov/UpKeeper", "--workflow", "ci.yml",
		"--event", "push", "--commit", candidate, "--limit", "1",
		"--json", "headSha,status,conclusion",
	})
	if err != nil {
		return "", err
	}
	if !gatePassed(gateOutput, candidate) {
		return "", failure.Value(
			"candidate main has no successful GitHub push gate; checkout unchanged")
	}
	current, err := CheckoutRevision(root, runner)
	if err != nil {
		return "", err
	}
	if current != before {
		return "", failure.Value("checkout changed during refresh; preserving it")
	}
	if _, err := git("merge", "--ff-only", "--quiet", candidate); err != nil {
		return "", err
	}
	verified, err := CheckoutRevision(root, runner)
	if err != nil {
		return "", err
	}
	if verified != candidate {
		return "", failure.Value("refreshed checkout did not verify")
	}
	return candidate, nil
}

// ProgramPaths are the paths a change to which changes the program itself,
// as opposed to the release intent the program applies. The release package
// owns the list, because release.CheckProgramPin checks a program pin against
// the same paths.
var ProgramPaths = release.ProgramPaths

// ErrProgramDiffers marks the one VerifyProgram refusal a self-update can
// resolve: an identified, clean binary whose program is not the gated one.
// An unidentified binary, or one the checkout cannot compare, is not it.
var ErrProgramDiffers = errors.New("program differs")

// VerifyProgram refuses to converge when the running binary is not built from
// the program the refreshed checkout now holds.
//
// The Python launcher was the checkout, so refreshing it and re-executing it
// guaranteed that the gated code ran. A compiled binary breaks that identity:
// a fast-forward moves the release intent and the source, and leaves the
// running program exactly as it was. Re-executing the same binary restores
// nothing, so the invariant has to be checked instead of assumed.
//
// A binary older than the checkout is normal and allowed: main advances on
// every release capture without the Go program changing at all. What is
// refused is a binary whose program differs from the one that was gated, and
// the refusal names the commit whose artifact the host needs.
func VerifyProgram(root, binaryCommit, checkoutCommit string, runner procx.Runner) error {
	if runner == nil {
		runner = procx.Run
	}
	if binaryCommit == "" || binaryCommit == "unknown" || strings.HasSuffix(binaryCommit, "-dirty") {
		return failure.Value(
			"this binary does not name a clean source commit (%s); a scheduled "+
				"convergence will not run an unidentified program", displayCommit(binaryCommit))
	}
	if binaryCommit == checkoutCommit {
		return nil
	}
	run := func(argv ...string) procx.Result {
		return runner(procx.Command{
			Argv: append([]string{"git", "-C", root}, argv...), Timeout: 60 * time.Second})
	}
	if result := run("cat-file", "-e", binaryCommit+"^{commit}"); result.Err != nil || result.Code != 0 {
		return failure.Value(
			"this binary was built from %s, which is not in this checkout; "+
				"install the released artifact for %s", binaryCommit, checkoutCommit)
	}
	diff := run(append([]string{"diff", "--quiet", binaryCommit, checkoutCommit, "--"},
		ProgramPaths...)...)
	// Only git's "differences found" may count as a changed program: that is
	// what a self-update acts on, and a failed comparison proves nothing.
	if diff.Err != nil || (diff.Code != 0 && diff.Code != 1) {
		return failure.Value("cannot compare this binary's source with the checkout")
	}
	if diff.Code != 0 {
		return failure.Wrap(ErrProgramDiffers, failure.KindValue,
			"this binary was built from %s and the gated checkout is now %s, whose "+
				"program differs; install the released artifact for %s before converging",
			binaryCommit, checkoutCommit, checkoutCommit)
	}
	return nil
}

func displayCommit(value string) string {
	if value == "" {
		return "unset"
	}
	return value
}

// gatePassed requires one completed, successful run for exactly this commit.
// An in-progress run, a different commit or an extra field all fail the gate.
func gatePassed(output, candidate string) bool {
	document, err := canonjson.Decode([]byte(output))
	if err != nil {
		return false
	}
	runs, ok := document.([]any)
	if !ok || len(runs) == 0 {
		return false
	}
	run, ok := runs[0].(map[string]any)
	if !ok || len(run) != 3 {
		return false
	}
	return run["headSha"] == candidate &&
		run["status"] == "completed" &&
		run["conclusion"] == "success"
}
