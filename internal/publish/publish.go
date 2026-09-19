// Package publish implements `upkeeper push`: capture this machine as the
// release and publish it.
//
// Publishing means committing desired/release.json plus the stored agent files
// to this repository and pushing the current branch to the named remotes.
// Targets never read the remote themselves; the controller carries the manifest
// to them with `upkeeper pull`. Publishing exists so that the release is
// durable, reviewable and recoverable from another controller.
package publish

import (
	"io"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// releasePaths limits every Git operation to the release itself, so that a
// capture can never commit unrelated work that happens to be in the tree.
var releasePaths = []string{"--", release.File, release.AgentConfigDir}

// Options are the choices `upkeeper push` offers.
type Options struct {
	Controller string
	Remotes    []string
	// Commit is false for a manifest-only run: the candidate manifest is
	// written and nothing else happens. It implies no push.
	Commit bool
	// Push is false for a run that commits locally without publishing.
	Push       bool
	Home       string
	Components []string
	Runner     procx.Runner
	Now        time.Time
	// PublishDivergedInstructions is the operator's explicit permission to
	// publish resident agent instructions that disagree with their source. It
	// is asked for by name; no general --force reaches it.
	PublishDivergedInstructions bool
	// Notice is where an override prints the paths it is promoting.
	Notice io.Writer
}

// ConfiguredRemotes lists the Git remotes of a checkout.
func ConfiguredRemotes(root string, runner procx.Runner) ([]string, error) {
	output, err := git(runner, root, 60*time.Second, "remote")
	if err != nil {
		return nil, err
	}
	remotes := []string{}
	for _, line := range strings.Split(output, "\n") {
		if line != "" {
			remotes = append(remotes, line)
		}
	}
	return remotes, nil
}

// Release captures this machine, writes the manifest, and commits and pushes it
// unless the caller asked for less.
func Release(root string, options Options) (map[string]any, error) {
	runner := options.Runner
	if runner == nil {
		runner = procx.Run
	}
	// The remote names are checked before anything is captured or written.
	// The Python implementation only checked them on a committing run, so a
	// typo in a remote name stayed hidden until the run that would have
	// published it. Checking first cannot mutate anything and it fails closed.
	known, err := ConfiguredRemotes(root, runner)
	if err != nil {
		return nil, err
	}
	if unknown := missing(options.Remotes, known); len(unknown) > 0 {
		return nil, failure.Push("unknown git remote(s): %s", strings.Join(unknown, ", "))
	}

	// The resident-instruction gate lives inside Capture, so it runs before the
	// candidate manifest is written and before any Git index is touched: a
	// refusal leaves no partial manifest, no commit to amend and no branch
	// behind. --no-commit is gated too, because writing the candidate manifest
	// is already a write.
	captured, err := release.Capture(
		root, options.Controller, options.Home, runner, options.Now, options.Components,
		release.ResidentPolicy{
			PublishDiverged: options.PublishDivergedInstructions,
			Notice:          options.Notice,
		})
	if err != nil {
		return nil, err
	}
	if _, err := release.Write(root, captured); err != nil {
		return nil, err
	}
	digest, err := captured.SHA256()
	if err != nil {
		return nil, err
	}
	summary := release.Describe(captured)
	report := map[string]any{
		"schema_version": exitcode.SchemaVersion,
		"command":        "push",
		"controller":     options.Controller,
		"release_sha256": digest,
		"summary":        summary,
		"committed":      nil,
		"pushed":         []string{},
	}
	// A manifest-only run writes the candidate manifest and stops. It must
	// neither commit nor push, and it deliberately omits "head" so that the
	// text report says nothing about commits that were never considered.
	if !options.Commit {
		return report, nil
	}

	if _, err := git(runner, root, 60*time.Second, append([]string{"add"}, releasePaths...)...); err != nil {
		return nil, err
	}
	staged, err := git(runner, root, 60*time.Second,
		append([]string{"diff", "--cached", "--name-only"}, releasePaths...)...)
	if err != nil {
		return nil, err
	}
	if staged != "" {
		message := "Capture the fleet release from " + options.Controller + "\n\n"
		for _, line := range summary {
			message += "- " + line + "\n"
		}
		arguments := append(
			[]string{"commit", "--quiet", "--only", "-m", message}, releasePaths...)
		if _, err := git(runner, root, 60*time.Second, arguments...); err != nil {
			return nil, err
		}
		head, err := git(runner, root, 60*time.Second, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		report["committed"] = head
	}
	head, err := git(runner, root, 60*time.Second, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	report["head"] = head
	if !options.Push {
		return report, nil
	}
	branch, err := git(runner, root, 60*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, err
	}
	if branch == "HEAD" {
		return nil, failure.Push("cannot push a detached HEAD; check out a branch first")
	}
	pushed := []string{}
	for _, remote := range options.Remotes {
		if _, err := git(runner, root, 120*time.Second,
			"push", "--quiet", remote, "HEAD:refs/heads/"+branch); err != nil {
			report["pushed"] = pushed
			return nil, err
		}
		pushed = append(pushed, remote+" "+branch)
	}
	report["pushed"] = pushed
	return report, nil
}

func git(runner procx.Runner, root string, timeout time.Duration, arguments ...string) (string, error) {
	if runner == nil {
		runner = procx.Run
	}
	result := runner(procx.Command{
		Argv:    append([]string{"git", "-C", root}, arguments...),
		Timeout: timeout,
	})
	if result.Err != nil {
		return "", failure.Push("git %s failed: %v", strings.Join(arguments, " "), result.Err)
	}
	if result.Code != 0 {
		stderr := strings.TrimSpace(result.Stderr)
		if len(stderr) > 400 {
			stderr = stderr[len(stderr)-400:]
		}
		return "", failure.Push("git %s failed: %s", strings.Join(arguments, " "), stderr)
	}
	return strings.TrimSpace(result.Stdout), nil
}

func missing(wanted, known []string) []string {
	present := make(map[string]bool, len(known))
	for _, name := range known {
		present[name] = true
	}
	unknown := []string{}
	for _, name := range wanted {
		if !present[name] {
			unknown = append(unknown, name)
		}
	}
	return unknown
}
