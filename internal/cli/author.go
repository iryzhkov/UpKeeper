package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/iryzhkov/UpKeeper/internal/author"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// authorSetupRepositoryDefault is where the omarchy-setup clone an operator
// authors releases from lives, relative to their home directory.
const authorSetupRepositoryDefault = "Work/omarchy-setup"

// runAuthor writes an authored release into the checkout's manifest and
// reports what changed.
//
// It replaces a helper script that lived outside this repository and had to
// copy a test file into it to reach the spec builder capture uses. Like
// project and provider, it only edits desired/release.json; publication stays
// with `push --authored` and its two fences.
func runAuthor(parsed *arguments, streams Streams) (int, error) {
	root, err := CheckoutRoot()
	if err != nil {
		return exitcode.Preflight, err
	}
	repository := parsed.authorSetupRepository
	if repository == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return exitcode.Preflight, failure.Validation("cannot resolve the home directory: %v", err)
		}
		repository = filepath.Join(home, authorSetupRepositoryDefault)
	}
	result, err := author.Run(root, author.Options{
		Steward:         parsed.authorSteward,
		SetupCommit:     parsed.authorSetupCommit,
		SetupRepository: repository,
		DryRun:          parsed.dryRun,
	})
	if err != nil {
		return exitcode.Preflight, err
	}
	pins := make([]any, 0, len(result.Pins))
	for _, pin := range result.Pins {
		pins = append(pins, map[string]any{"name": pin.Name, "from": pin.From, "to": pin.To})
	}
	files := make([]any, 0, len(result.Files))
	for _, file := range result.Files {
		entry := map[string]any{"path": file.Path, "action": file.Action}
		if file.From != "" {
			entry["from_sha256"] = file.From
		}
		if file.To != "" {
			entry["to_sha256"] = file.To
		}
		files = append(files, entry)
	}
	notes := make([]any, 0, len(result.Notes))
	for _, note := range result.Notes {
		notes = append(notes, note)
	}
	next := "nothing to publish: the release already pins these inputs"
	if result.Changed {
		next = "review `git diff " + release.File + "`, commit it, then publish with " +
			"upkeeper push --authored --expect-head <that commit> --expect-release-sha256 " + result.ReleaseSHA256
		if parsed.dryRun {
			next = "run again without --dry-run to write " + release.File
		}
	}
	report := map[string]any{
		"schema_version":          exitcode.SchemaVersion,
		"command":                 "author",
		"file":                    release.File,
		"dry_run":                 parsed.dryRun,
		"head":                    result.Head,
		"omarchy_setup_commit":    result.SetupCommit,
		"steward_version":         parsed.authorSteward,
		"pins":                    pins,
		"files":                   files,
		"notes":                   notes,
		"changed":                 result.Changed,
		"written":                 result.Written,
		"previous_release_sha256": result.PreviousReleaseSHA256,
		"release_sha256":          result.ReleaseSHA256,
		"next":                    next,
	}
	if parsed.asJSON {
		if err := printJSON(streams.Out, report); err != nil {
			return 1, err
		}
		return exitcode.Success, nil
	}
	switch {
	case !result.Changed:
		fmt.Fprintf(streams.Out, "no changes: %s already pins t3-steward %s and omarchy-setup %s (release %s)\n",
			release.File, parsed.authorSteward, shorten(result.SetupCommit, 12), shorten(result.ReleaseSHA256, 12))
	case result.Written:
		fmt.Fprintf(streams.Out, "authored %s (release %s, was %s)\n",
			release.File, shorten(result.ReleaseSHA256, 12), shorten(result.PreviousReleaseSHA256, 12))
	default:
		fmt.Fprintf(streams.Out, "dry run: %s would become release %s (now %s); nothing written\n",
			release.File, shorten(result.ReleaseSHA256, 12), shorten(result.PreviousReleaseSHA256, 12))
	}
	for _, pin := range result.Pins {
		fmt.Fprintf(streams.Out, "  pin %s: %s -> %s\n", pin.Name, pin.From, pin.To)
	}
	for _, file := range result.Files {
		switch file.Action {
		case "added":
			fmt.Fprintf(streams.Out, "  added %s (%s)\n", file.Path, shorten(file.To, 12))
		case "retired":
			fmt.Fprintf(streams.Out, "  retired %s (was %s)\n", file.Path, shorten(file.From, 12))
		default:
			fmt.Fprintf(streams.Out, "  changed %s: %s -> %s\n", file.Path, shorten(file.From, 12), shorten(file.To, 12))
		}
	}
	for _, note := range result.Notes {
		fmt.Fprintf(streams.Out, "note: %s\n", note)
	}
	fmt.Fprintln(streams.Out, next)
	return exitcode.Success, nil
}
