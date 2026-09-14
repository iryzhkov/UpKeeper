package selfconv

import (
	"os"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/procx"
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
