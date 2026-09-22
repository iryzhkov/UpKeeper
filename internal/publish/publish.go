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
	if _, err := git(runner, root, 60*time.Second, append([]string{"add"}, paths...)...); err != nil {
		return nil, err
	}
	staged, err := git(runner, root, 60*time.Second,
		append([]string{"diff", "--cached", "--name-only"}, paths...)...)
	if err != nil {
		return nil, err
	}
	if staged != "" {
		message := "Capture the fleet release from " + options.Controller + "\n\n"
		for _, line := range summary {
			message += "- " + line + "\n"
		}
		arguments := append(
			[]string{"commit", "--quiet", "--only", "-m", message}, paths...)
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
	receipts := make([]map[string]any, 0, len(options.Remotes))
	for index, remote := range options.Remotes {
		if _, err := git(runner, root, 120*time.Second,
			"push", "--quiet", remote, "HEAD:refs/heads/"+branch); err != nil {
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
