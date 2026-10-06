package selfconv

import (
	"os"
	"path/filepath"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/redact"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// HoldDirectory is where the durable scheduled markers live.
func HoldDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", failure.Validation("cannot resolve the home directory: %v", err)
	}
	return filepath.Join(home, ".local", "state", "dev-fleet", "scheduled-holds"), nil
}

// ConvergeSelf brings this machine onto the release its checkout names.
func ConvergeSelf(
	root string, dryRun bool, runner procx.Runner,
) (map[string]any, int, error) {
	return convergeSelf(root, dryRun, runner, nil)
}

// ConvergeSelfAfterUpdate is ConvergeSelf run by a freshly installed binary,
// which records what the self-update did in the report and the run record.
func ConvergeSelfAfterUpdate(
	root string, runner procx.Runner, selfUpdate map[string]any,
) (map[string]any, int, error) {
	return convergeSelf(root, false, runner, selfUpdate)
}

func convergeSelf(
	root string, dryRun bool, runner procx.Runner, selfUpdate map[string]any,
) (map[string]any, int, error) {
	revision, err := CheckoutRevision(root, runner)
	if err != nil {
		return nil, 0, err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return nil, 0, err
	}
	host, err := LocalHost(config)
	if err != nil {
		return nil, 0, err
	}
	value, commit, stored, err := release.LoadHistorical(root, revision, "", runner)
	if err != nil {
		return nil, 0, err
	}
	current, err := CheckoutRevision(root, runner)
	if err != nil {
		return nil, 0, err
	}
	if current != commit {
		return nil, 0, failure.Value("checkout changed during self validation")
	}
	hostRunner, err := scheduledRunner(dryRun, runner)
	if err != nil {
		return nil, 0, err
	}
	return converge.Fleet(config, value, converge.Options{
		Hosts:          []string{host},
		Command:        "self-pull",
		SelfOnly:       true,
		DryRun:         dryRun,
		Timeout:        DefaultTimeout,
		ManifestCommit: &commit,
		StoredFiles:    stored,
		Runner:         hostRunner,
		Process:        runner,
		SelfUpdate:     selfUpdate,
	})
}

// ConvergeFleet is the controller-side daily convergence that belongs on
// normandy and nowhere else.
func ConvergeFleet(
	root string, dryRun bool, runner procx.Runner,
) (map[string]any, int, error) {
	revision, err := CheckoutRevision(root, runner)
	if err != nil {
		return nil, 0, err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return nil, 0, err
	}
	host, err := LocalHost(config)
	if err != nil {
		return nil, 0, err
	}
	if host != "normandy" {
		return nil, 0, failure.Value("the fleet convergence timer belongs on normandy only")
	}
	value, commit, stored, err := release.LoadHistorical(root, revision, "", runner)
	if err != nil {
		return nil, 0, err
	}
	current, err := CheckoutRevision(root, runner)
	if err != nil {
		return nil, 0, err
	}
	if current != commit {
		return nil, 0, failure.Value("checkout changed during scheduled validation")
	}
	hostRunner, err := scheduledRunner(dryRun, runner)
	if err != nil {
		return nil, 0, err
	}
	return converge.Fleet(config, value, converge.Options{
		Hosts:           config.HostNames(),
		Command:         "scheduled-pull",
		DryRun:          dryRun,
		Timeout:         DefaultTimeout,
		ManifestCommit:  &commit,
		StoredFiles:     stored,
		SkipUnreachable: true,
		Runner:          hostRunner,
		Process:         runner,
		// Remote hosts must run the program the release pins.
		ExpectedProgramCommit: converge.ProgramFence(value, "scheduled-pull"),
	})
}

// DefaultTimeout is the per-host bound a scheduled convergence uses. It matches
// the default of the interactive command.
const DefaultTimeout = 1800 * time.Second

// scheduledRunner adds the durable hold marker to real runs. A dry run leaves
// no marker because it changes nothing that would need observing.
func scheduledRunner(dryRun bool, runner procx.Runner) (converge.HostRunner, error) {
	if dryRun {
		return converge.NewRunner(runner), nil
	}
	directory, err := HoldDirectory()
	if err != nil {
		return nil, err
	}
	hold := &HoldRunner{Directory: directory, Inner: converge.NewRunner(runner)}
	return hold.Run, nil
}

// Notification decides whether a scheduled convergence is worth telling anyone
// about. A quiet run that changed nothing is not news.
func Notification(report map[string]any) (severity, title string, wanted bool) {
	hosts, _ := report["hosts"].([]any)
	changed := false
	for _, item := range hosts {
		host, _ := item.(map[string]any)
		if host["result"] == "updated" {
			changed = true
		}
		if legacy, ok := host["legacy_updater"].(map[string]any); ok &&
			legacy["state"] == "updated" {
			changed = true
		}
		components, _ := host["components"].([]any)
		for _, entry := range components {
			component, _ := entry.(map[string]any)
			if component["state"] == "updated" {
				changed = true
			}
		}
	}
	failed := exitCodeOf(report) != 0
	if !changed && !failed {
		return "", "", false
	}
	if failed {
		return "fail", "UpKeeper scheduled convergence needs attention", true
	}
	return "info", "UpKeeper scheduled convergence changed hosts", true
}

// PostReport tells the fleet feed what a scheduled convergence did.
//
// Exactly one attempt is made. A lost feed response does not justify a
// duplicate post, because the feed is the fleet's shared history and a
// duplicated event is a lie about what happened.
func PostReport(report map[string]any, runner procx.Runner) error {
	severity, title, wanted := Notification(report)
	if !wanted {
		return nil
	}
	if runner == nil {
		runner = procx.Run
	}
	summary := map[string]any{}
	for _, key := range []string{
		"run_id", "controller", "desired_manifest_commit", "exit_code", "error",
	} {
		if value, present := report[key]; present {
			summary[key] = value
		}
	}
	hosts, _ := report["hosts"].([]any)
	entries := make([]any, 0, len(hosts))
	for _, item := range hosts {
		host, _ := item.(map[string]any)
		components, _ := host["components"].([]any)
		interesting := make([]any, 0, len(components))
		for _, entry := range components {
			component, _ := entry.(map[string]any)
			if component["state"] == "converged" {
				continue
			}
			interesting = append(interesting, map[string]any{
				"name": component["name"], "state": component["state"],
				"error": component["error"],
			})
		}
		entries = append(entries, map[string]any{
			"host": host["host"], "result": host["result"],
			"error": host["error"], "components": interesting,
		})
	}
	summary["hosts"] = entries
	body, err := canonjson.Marshal(redact.Value(summary))
	if err != nil {
		return err
	}
	result := runner(procx.Command{
		Argv: []string{
			"feed", "post", title, "--source", "upkeeper-pull",
			"--tag", "citadel upkeeper", "--severity", severity, "--body-file", "-",
		},
		Input:   string(body),
		Timeout: 30 * time.Second,
	})
	if result.Err != nil || result.Code != 0 {
		return failure.Value("fleet feed post failed; inspect the journal and run record")
	}
	return nil
}

func exitCodeOf(report map[string]any) int {
	switch value := report["exit_code"].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return exitcode.Success
	}
}
