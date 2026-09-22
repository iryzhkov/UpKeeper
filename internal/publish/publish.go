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
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
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
	// Authored publishes the already-reviewed release bytes without recapture.
	// Both fences are mandatory and are checked before any Git mutation.
	Authored              bool
	ExpectedHead          string
	ExpectedReleaseSHA256 string
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

	var captured release.Release
	var previous release.Release
	var historicalErr error
	if options.Authored {
		if options.ExpectedHead == "" || options.ExpectedReleaseSHA256 == "" {
			return nil, failure.Push("authored publication requires --expect-head and --expect-release-sha256")
		}
		if len(options.Components) != 0 {
			return nil, failure.Push("--components is not valid with authored publication; the reviewed release is published as a whole")
		}
		head, headErr := git(runner, root, 60*time.Second, "rev-parse", "HEAD")
		if headErr != nil {
			return nil, headErr
		}
		if head != options.ExpectedHead {
			return nil, failure.Push("authored publication HEAD changed: got %s, want %s", head, options.ExpectedHead)
		}
		captured, err = release.Load(root)
		if err != nil {
			return nil, err
		}
		previous, _, _, historicalErr = release.LoadHistorical(root, options.ExpectedHead, "--expect-head", runner)
		if historicalErr != nil {
			return nil, historicalErr
		}
		digest, digestErr := captured.SHA256()
		if digestErr != nil {
			return nil, digestErr
		}
		if digest != options.ExpectedReleaseSHA256 {
			return nil, failure.Push("authored release digest changed: got %s, want %s", digest, options.ExpectedReleaseSHA256)
		}
		config, inventoryErr := inventory.Load(root)
		if inventoryErr != nil {
			return nil, inventoryErr
		}
		if document, present, fleetErr := captured.FleetIntent(); fleetErr != nil {
			return nil, fleetErr
		} else if present {
			if fleetErr = document.CheckInventory(config.KnownHosts()); fleetErr != nil {
				return nil, fleetErr
			}
		}
	} else {
		// The resident-instruction gate lives inside Capture, so it runs before
		// the candidate is written or the Git index is touched.
		captured, err = release.Capture(
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

	paths := releasePaths
	if options.Authored {
		paths = authoredReleasePaths(captured, previous)
	}
	var head string
	if options.Authored {
		head, err = commitAuthored(root, options, runner, captured, paths, summary)
		if err != nil {
			return nil, err
		}
		if head != options.ExpectedHead {
			report["committed"] = head
		}
	} else {
		if _, err = git(runner, root, 60*time.Second, append([]string{"add"}, paths...)...); err != nil {
			return nil, err
		}
		staged, stageErr := git(runner, root, 60*time.Second,
			append([]string{"diff", "--cached", "--name-only"}, paths...)...)
		if stageErr != nil {
			return nil, stageErr
		}
		if staged != "" {
			message := releaseCommitMessage(options.Controller, summary)
			arguments := append([]string{"commit", "--quiet", "--only", "-m", message}, paths...)
			if _, err = git(runner, root, 60*time.Second, arguments...); err != nil {
				return nil, err
			}
		}
		head, err = git(runner, root, 60*time.Second, "rev-parse", "HEAD")
		if staged != "" {
			report["committed"] = head
		}
	}
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
	pushSource := "HEAD"
	if options.Authored {
		pushSource = head
	}
	pushed := []string{}
	receipts := make([]map[string]any, 0, len(options.Remotes))
	for index, remote := range options.Remotes {
		if _, err := git(runner, root, 120*time.Second,
			"push", "--quiet", remote, pushSource+":refs/heads/"+branch); err != nil {
			receipts = append(receipts, map[string]any{"remote": remote, "branch": branch, "status": "failed", "error": err.Error()})
			for _, pending := range options.Remotes[index+1:] {
				receipts = append(receipts, map[string]any{"remote": pending, "branch": branch, "status": "not-attempted"})
			}
			report["pushed"] = pushed
			report["push_receipts"] = receipts
			report["result"] = "partial-failure"
			return report, err
		}
		pushed = append(pushed, remote+" "+branch)
		receipts = append(receipts, map[string]any{"remote": remote, "branch": branch, "status": "pushed", "head": head})
	}
	report["pushed"] = pushed
	report["push_receipts"] = receipts
	return report, nil
}

func releaseCommitMessage(controller string, summary []string) string {
	message := "Capture the fleet release from " + controller + "\n\n"
	for _, line := range summary {
		message += "- " + line + "\n"
	}
	return message
}

// commitAuthored builds and validates the exact commit in a private index, then
// advances the checked-out branch only if its parent is still the reviewed HEAD.
// The caller's index is never read or changed.
func commitAuthored(root string, options Options, runner procx.Runner, captured release.Release, paths, summary []string) (string, error) {
	index, err := os.CreateTemp("", "upkeeper-authored-index-*")
	if err != nil {
		return "", err
	}
	indexPath := index.Name()
	if err = index.Close(); err != nil {
		return "", err
	}
	if err = os.Remove(indexPath); err != nil {
		return "", err
	}
	defer os.Remove(indexPath)
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	run := func(input string, arguments ...string) (string, error) {
		result := runner(procx.Command{
			Argv: append([]string{"git", "-C", root}, arguments...),
			Env:  env, Input: input, Timeout: 60 * time.Second,
		})
		if result.Err != nil {
			return "", result.Err
		}
		if result.Code != 0 {
			return "", failure.Push("git %s failed: %s", strings.Join(arguments, " "), result.Output())
		}
		return result.Stdout, nil
	}
	if _, err = run("", "read-tree", options.ExpectedHead); err != nil {
		return "", err
	}
	if _, err = run("", append([]string{"add"}, paths...)...); err != nil {
		return "", err
	}
	staged, err := run("", append([]string{"diff", "--cached", "--name-only", options.ExpectedHead}, paths...)...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(staged) == "" {
		return options.ExpectedHead, nil
	}
	expectedManifest, err := captured.CanonicalBytes()
	if err != nil {
		return "", err
	}
	indexManifest, err := run("", "show", ":"+release.File)
	if err != nil {
		return "", err
	}
	if indexManifest != string(expectedManifest) {
		return "", failure.Push("authored release changed while staging")
	}
	for _, entry := range captured.Files() {
		store, _ := entry["store"].(string)
		expected, _ := entry["sha256"].(string)
		payload, showErr := run("", "show", ":"+filepath.ToSlash(filepath.Join(release.AgentConfigDir, store)))
		if showErr != nil {
			return "", showErr
		}
		sum := sha256.Sum256([]byte(payload))
		if hex.EncodeToString(sum[:]) != expected {
			return "", failure.Push("authored companion file changed while staging: %s", store)
		}
	}
	current, err := git(runner, root, 60*time.Second, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if current != options.ExpectedHead {
		return "", failure.Push("authored publication HEAD changed: got %s, want %s", current, options.ExpectedHead)
	}
	branch, err := git(runner, root, 60*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if branch == "HEAD" {
		return "", failure.Push("cannot publish authored release from a detached HEAD")
	}
	tree, err := run("", "write-tree")
	tree = strings.TrimSpace(tree)
	if err != nil {
		return "", err
	}
	commit, err := run(releaseCommitMessage(options.Controller, summary), "commit-tree", tree, "-p", options.ExpectedHead)
	commit = strings.TrimSpace(commit)
	if err != nil {
		return "", err
	}
	if _, err = run("", "update-ref", "refs/heads/"+branch, commit, options.ExpectedHead); err != nil {
		return "", failure.Push("authored branch changed before commit: %v", err)
	}
	return commit, nil
}
func authoredReleasePaths(values ...release.Release) []string {
	paths := []string{"--", release.File}
	seen := map[string]bool{}
	for _, value := range values {
		for _, entry := range value.Files() {
			store, _ := entry["store"].(string)
			if store == "" || seen[store] {
				continue
			}
			seen[store] = true
			paths = append(paths, filepath.Join(release.AgentConfigDir, filepath.FromSlash(store)))
		}
	}
	sort.Strings(paths[2:])
	return paths
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
