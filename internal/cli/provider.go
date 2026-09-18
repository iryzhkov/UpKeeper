package cli

import (
	"fmt"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// runProvider registers or removes one provider instance in the release
// manifest and reports the fleet plan the edit produces.
//
// It is the provider half of what runProject is for projects: one reviewed
// edit of desired/release.json carries the instance, its models, its pool and
// its quota binding, so that registering a provider is a release edit rather
// than an edit of the manifest plus an edit of the coordinator's own
// configuration on another host. Nothing is committed, pushed, pulled or
// enrolled here.
func runProvider(parsed *arguments, streams Streams) (int, error) {
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
	var edit release.ProviderEdit
	switch parsed.providerAction {
	case "add":
		value, edit, err = release.AddProvider(root, release.ProviderSpec{
			Instance:  parsed.providerInstance,
			QuotaPool: parsed.providerPool,
			Models:    parsed.providerModels,
			Hosts:     parsed.providerHosts,
		})
	case "remove":
		value, edit, err = release.RemoveProvider(root, parsed.providerInstance, parsed.providerHosts)
	default:
		return exitcode.Preflight, failure.Validation("unknown provider action: %s", parsed.providerAction)
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
		"command":        "provider",
		"action":         parsed.providerAction,
		"instance":       parsed.providerInstance,
		"quota_pool":     nullable(parsed.providerPool),
		"models":         asAnyList(parsed.providerModels),
		"hosts":          asAnyList(parsed.providerHosts),
		"edited": map[string]any{
			"profiles": asAnyList(edit.Profiles),
			"hosts":    asAnyList(edit.Hosts),
		},
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
	verb, preposition := "added", "to"
	if parsed.providerAction == "remove" {
		verb, preposition = "removed", "from"
	}
	fmt.Fprintf(streams.Out, "%s provider %s %s %s (release %s) on %s\n",
		verb, parsed.providerInstance, preposition, release.File,
		shorten(value.Digest(), 12), describeProviderEdit(edit))
	fmt.Fprintln(streams.Out, "plan against the release committed at HEAD:")
	printFleetPlan(streams.Out, planReport)
	fmt.Fprintf(streams.Out, "%s\n", text(report["next"]))
	return exitcode.Success, nil
}

// describeProviderEdit says where the edit landed, in words: the profiles
// edited as a class and the hosts edited through their exceptions.
func describeProviderEdit(edit release.ProviderEdit) string {
	parts := []string{}
	for _, profile := range edit.Profiles {
		parts = append(parts, "profile "+profile)
	}
	for _, host := range edit.Hosts {
		parts = append(parts, "host "+host+" (exceptions)")
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

func asAnyList(values []string) []any {
	converted := make([]any, 0, len(values))
	for _, value := range values {
		converted = append(converted, value)
	}
	return converted
}
