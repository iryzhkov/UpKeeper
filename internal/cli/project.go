package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// runProject edits one project in the release manifest and reports the fleet
// plan the edit produces.
//
// It is the one project-add flow the Home Assistant campaign was missing: the
// manifest is edited through the release model instead of by hand in a
// two-thousand-line file, validated the way fleet-plan validates it, and the
// authorization diff against the committed release is printed so the operator
// reads what changed before committing it. Nothing is committed, pushed,
// pulled or enrolled here; the only file that changes is desired/release.json
// in the checkout.
func runProject(parsed *arguments, streams Streams) (int, error) {
	// The diff is against the release at HEAD, so this needs the checkout,
	// not just its configuration files.
	root, err := CheckoutRoot()
	if err != nil {
		return exitcode.Preflight, err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	previous, err := committedFleetIntent(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	var value release.Release
	switch parsed.projectAction {
	case "add":
		value, err = release.AddProject(root, release.ProjectSpec{
			Name:            parsed.projectName,
			Repository:      parsed.projectRepository,
			DefaultRef:      parsed.projectDefaultRef,
			SetupProfile:    parsed.projectSetupProfile,
			EligibleWorkers: parsed.projectWorkers,
		})
	case "remove":
		value, err = release.RemoveProject(root, parsed.projectName)
	default:
		return exitcode.Preflight, failure.Validation("unknown project action: %s", parsed.projectAction)
	}
	if err != nil {
		return exitcode.Preflight, err
	}
	document, _, err := value.FleetIntent()
	if err != nil {
		return exitcode.Preflight, err
	}
	if err := document.CheckInventory(config.KnownHosts()); err != nil {
		return exitcode.Preflight, err
	}
	// The plan is composed from the same pieces runFleetPlan uses, against the
	// same previous document, so what this prints is exactly what
	// `fleet-plan --previous HEAD` would print for the edited checkout.
	planReport, err := document.Report()
	if err != nil {
		return exitcode.Preflight, err
	}
	plan, err := document.EnrollmentPlan(previous)
	if err != nil {
		return exitcode.Preflight, err
	}
	planReport["release_sha256"] = value.Digest()
	planReport["enrollment_plan"] = plan
	report := map[string]any{
		"schema_version": exitcode.SchemaVersion,
		"command":        "project",
		"action":         parsed.projectAction,
		"project":        parsed.projectName,
		"file":           release.File,
		"release_sha256": value.Digest(),
		"previous":       "HEAD",
		"fleet_plan":     planReport,
		"next":           "review `git diff " + release.File + "`, commit, then publish with upkeeper push",
	}
	if parsed.asJSON {
		if err := printJSON(streams.Out, report); err != nil {
			return 1, err
		}
		return exitcode.Success, nil
	}
	verb := "added"
	if parsed.projectAction == "remove" {
		verb = "removed"
	}
	preposition := "to"
	if parsed.projectAction == "remove" {
		preposition = "from"
	}
	fmt.Fprintf(streams.Out, "%s project %s %s %s (release %s)\n",
		verb, parsed.projectName, preposition, release.File, shorten(value.Digest(), 12))
	fmt.Fprintln(streams.Out, "plan against the release committed at HEAD:")
	printFleetPlan(streams.Out, planReport)
	fmt.Fprintf(streams.Out, "%s\n", text(report["next"]))
	return exitcode.Success, nil
}

// committedFleetIntent reads the fleet intent of the release committed at HEAD
// out of Git history, or nil when that release carries none, so that the diff
// is against what was reviewed and not against the working tree before the
// edit. HEAD is resolved to its hash first because the historical loader
// accepts only a commit hash.
func committedFleetIntent(root string) (*fleet.Document, error) {
	result := procx.Run(procx.Command{
		Argv:    []string{"git", "-C", root, "rev-parse", "--verify", "HEAD^{commit}"},
		Timeout: 30 * time.Second,
	})
	if !result.OK() {
		return nil, failure.Validation(
			"the checkout at %s has no commit at HEAD to diff against", root)
	}
	value, _, _, err := release.LoadHistorical(root, strings.TrimSpace(result.Stdout), "", procx.Run)
	if err != nil {
		return nil, err
	}
	document, present, err := value.FleetIntent()
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	return document, nil
}
