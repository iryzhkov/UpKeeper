package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/buildinfo"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/fleet"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/probe"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/publish"
	"github.com/iryzhkov/UpKeeper/internal/redact"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/report"
	"github.com/iryzhkov/UpKeeper/internal/runstore"
	"github.com/iryzhkov/UpKeeper/internal/safety"
	"github.com/iryzhkov/UpKeeper/internal/selfconv"
	"github.com/iryzhkov/UpKeeper/internal/units"
)

// Streams are the process's input and output, injected so that the whole
// command surface is testable without a subprocess.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Main runs one command and returns the process exit code.
//
// Three outcomes are distinguished on purpose. A malformed command line exits
// 2, an expected refusal exits 30 with a redacted message, and an unexpected
// failure exits 1 so that it can never be mistaken for a clean refusal.
func Main(program string, argv []string, streams Streams) int {
	program = filepath.Base(program)
	parsed, err := parse(argv, release.ComponentOrder)
	if requested, ok := err.(*helpRequest); ok {
		fmt.Fprint(streams.Out, helpText(program, requested.command))
		return 0
	}
	if err != nil {
		fmt.Fprintf(streams.Err, "usage: %s COMMAND [OPTIONS]\n", program)
		fmt.Fprintf(streams.Err, "%s: error: %s\n", program, err.Error())
		return exitcode.Usage
	}
	code, err := dispatch(parsed, streams)
	if err == nil {
		return code
	}
	message := redact.Text(err.Error())
	if !failure.IsExpected(err) {
		fmt.Fprintf(streams.Err, "%s: %s\n", program, message)
		return 1
	}
	if parsed.asJSON {
		_ = printJSON(streams.Out, map[string]any{
			"schema_version": exitcode.SchemaVersion,
			"command":        parsed.command,
			"result":         "invalid-input",
			"exit_code":      exitcode.Preflight,
			"error":          message,
		})
	} else {
		fmt.Fprintf(streams.Err, "%s: %s\n", program, message)
	}
	return exitcode.Preflight
}

func dispatch(parsed *arguments, streams Streams) (int, error) {
	switch parsed.command {
	case agentCommand:
		return runAgent(streams)
	case probeCommand:
		return runProbe(streams)
	case "version":
		fmt.Fprintln(streams.Out, buildinfo.Summary())
		return exitcode.Success, nil
	case "doctor", "status":
		return runObservation(parsed, streams)
	case "push":
		return runPush(parsed, streams)
	case "pull", "rollback":
		return runConvergence(parsed, streams)
	case "fleet-plan":
		return runFleetPlan(parsed, streams)
	case "self-pull":
		return runSelfPull(parsed, streams)
	case "scheduled-pull":
		return runScheduledPull(parsed, streams)
	case "hold":
		return runHold(parsed, streams)
	case "install-self-timer", "install-pull-timer":
		return runInstallTimer(parsed, streams)
	default:
		return exitcode.Preflight, failure.Validation("unknown command: %s", parsed.command)
	}
}

// runAgent is the target-side half of a convergence. It is invoked by a
// controller over SSH and speaks only JSON.
func runAgent(streams Streams) (int, error) {
	if err := agent.Run(streams.In, streams.Out, procx.Run); err != nil {
		return 1, err
	}
	return exitcode.Success, nil
}

// runProbe is the target-side half of doctor and status.
func runProbe(streams Streams) (int, error) {
	encoded, err := canonjson.MarshalSpaced(probe.Collect(procx.Run))
	if err != nil {
		return 1, err
	}
	fmt.Fprintln(streams.Out, string(encoded))
	return exitcode.Success, nil
}

func runObservation(parsed *arguments, streams Streams) (int, error) {
	if parsed.command == "status" {
		return runReleaseStatus(parsed, streams)
	}
	root, err := ConfigRoot()
	if err != nil {
		return exitcode.Preflight, err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	selected, err := safety.ParseHostSelector(parsed.limit, config.KnownHosts())
	if err != nil {
		return exitcode.Preflight, err
	}
	observations, err := probe.Fleet(
		config, selected, time.Duration(parsed.probeTimeout)*time.Second)
	if err != nil {
		return exitcode.Preflight, err
	}
	var document map[string]any
	var code int
	printer := printDoctor
	if parsed.command == "doctor" {
		document, code = report.Doctor(config, observations, selected)
	} else {
		document, code = report.Status(config, observations, selected)
		printer = printStatus
	}
	if parsed.asJSON {
		if err := printJSON(streams.Out, document); err != nil {
			return 1, err
		}
	} else {
		printer(streams.Out, document)
	}
	return code, nil
}

func runPush(parsed *arguments, streams Streams) (int, error) {
	root, err := ConfigRoot()
	if err != nil {
		return exitcode.Preflight, err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	remotes := parsed.remotes
	if len(remotes) == 0 {
		remotes, err = publish.ConfiguredRemotes(root, procx.Run)
		if err != nil {
			return exitcode.Preflight, err
		}
	}
	document, err := publish.Release(root, publish.Options{
		Controller: converge.Controller(config),
		Remotes:    remotes,
		// A manifest-only run writes the candidate manifest and stops: it
		// neither commits nor pushes, which is what --no-commit documents.
		Commit:     !parsed.noCommit,
		Push:       !parsed.noCommit && !parsed.noPush,
		Components: parsed.components,
		Runner:     procx.Run,
	})
	if err != nil {
		return exitcode.Preflight, err
	}
	if parsed.asJSON {
		if err := printJSON(streams.Out, document); err != nil {
			return 1, err
		}
	} else {
		printPush(streams.Out, document)
	}
	return exitcode.Success, nil
}

// runFleetPlan reports the Steward fleet intent and the enrollment it proposes.
//
// It reads the release and the inventory, and that is all it does: no host is
// reached, no file is written and nothing is enrolled. A plan is the document
// somebody reads before deciding, which is only useful if producing it cannot
// itself change anything.
func runFleetPlan(parsed *arguments, streams Streams) (int, error) {
	root, err := ConfigRoot()
	if err != nil {
		return exitcode.Preflight, err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	value, err := release.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	document, present, err := value.FleetIntent()
	if err != nil {
		return exitcode.Preflight, err
	}
	if !present {
		return exitcode.Preflight, failure.Validation(
			"this release carries no %s component", fleet.Component)
	}
	if err := document.CheckInventory(config.KnownHosts()); err != nil {
		return exitcode.Preflight, err
	}
	previous, err := previousFleetIntent(parsed)
	if err != nil {
		return exitcode.Preflight, err
	}
	documentReport, err := document.Report()
	if err != nil {
		return exitcode.Preflight, err
	}
	plan, err := document.EnrollmentPlan(previous)
	if err != nil {
		return exitcode.Preflight, err
	}
	documentReport["schema_version"] = exitcode.SchemaVersion
	documentReport["command"] = "fleet-plan"
	documentReport["release_sha256"] = value.Digest()
	documentReport["enrollment_plan"] = plan
	if parsed.asJSON {
		if err := printJSON(streams.Out, documentReport); err != nil {
			return 1, err
		}
	} else {
		printFleetPlan(streams.Out, documentReport)
	}
	return exitcode.Success, nil
}

// previousFleetIntent reads the release a plan is compared against out of Git
// history, so that the diff is against what was committed rather than against
// whatever the working tree currently holds.
func previousFleetIntent(parsed *arguments) (*fleet.Document, error) {
	if parsed.previousRelease == "" {
		return nil, nil
	}
	root, err := CheckoutRoot()
	if err != nil {
		return nil, err
	}
	value, _, _, err := release.LoadHistorical(root, parsed.previousRelease, "--previous", procx.Run)
	if err != nil {
		return nil, err
	}
	document, present, err := value.FleetIntent()
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, failure.Validation(
			"the release at %s carries no %s component",
			parsed.previousRelease, fleet.Component)
	}
	return document, nil
}

func runConvergence(parsed *arguments, streams Streams) (int, error) {
	root, err := convergenceRoot(parsed)
	if err != nil {
		return exitcode.Preflight, err
	}
	config, err := inventory.Load(root)
	if err != nil {
		return exitcode.Preflight, err
	}
	value, manifestCommit, stored, err := releaseFor(parsed, root)
	if err != nil {
		return exitcode.Preflight, err
	}
	hosts, err := selectHosts(parsed, config)
	if err != nil {
		return exitcode.Preflight, err
	}
	if parsed.pullTimeout < 60 || parsed.pullTimeout > 7200 {
		return exitcode.Preflight, failure.Validation(
			"timeout must be between 60 and 7200 seconds")
	}
	document, code, err := converge.Fleet(config, value, converge.Options{
		Hosts:           hosts,
		Components:      parsed.components,
		DryRun:          parsed.dryRun,
		Timeout:         time.Duration(parsed.pullTimeout) * time.Second,
		ManifestCommit:  manifestCommit,
		StoredFiles:     stored,
		Command:         parsed.command,
		SelfOnly:        parsed.selfOnly,
		SkipUnreachable: parsed.skipUnreachable,
		Process:         procx.Run,
	})
	if err != nil {
		return exitcode.Preflight, err
	}
	if parsed.asJSON {
		if err := printJSON(streams.Out, document); err != nil {
			return 1, err
		}
	} else {
		printPull(streams.Out, document)
	}
	return code, nil
}

// convergenceRoot picks the tree a convergence reads its intent from. A --self
// run insists on the release checkout, because it reads release history.
func convergenceRoot(parsed *arguments) (string, error) {
	if parsed.selfOnly {
		return CheckoutRoot()
	}
	return ConfigRoot()
}

// releaseFor resolves which release a convergence applies.
//
// A --self run and a rollback both read immutable intent out of Git history
// rather than out of the working tree, so that what is applied is exactly what
// was reviewed and committed. A plain pull applies the manifest as published in
// the checkout.
func releaseFor(
	parsed *arguments, root string,
) (release.Release, *string, map[string]string, error) {
	if parsed.selfOnly {
		revision, err := selfconv.CheckoutRevision(root, procx.Run)
		if err != nil {
			return release.Release{}, nil, nil, err
		}
		value, commit, stored, err := release.LoadHistorical(root, revision, "", procx.Run)
		if err != nil {
			return release.Release{}, nil, nil, err
		}
		// The checkout must not have moved while it was being validated, or
		// the manifest just read no longer describes the tree on disk.
		current, err := selfconv.CheckoutRevision(root, procx.Run)
		if err != nil {
			return release.Release{}, nil, nil, err
		}
		if current != revision {
			return release.Release{}, nil, nil, failure.Validation(
				"self checkout changed during validation; inspect before retry")
		}
		return value, &commit, stored, nil
	}
	if parsed.command == "rollback" {
		value, commit, stored, err := release.LoadHistorical(root, parsed.to, "--to", procx.Run)
		if err != nil {
			return release.Release{}, nil, nil, err
		}
		return value, &commit, stored, nil
	}
	value, err := release.Load(root)
	if err != nil {
		return release.Release{}, nil, nil, err
	}
	return value, manifestCommit(root), nil, nil
}

func selectHosts(parsed *arguments, config *inventory.Config) ([]string, error) {
	switch {
	case parsed.selfOnly:
		host, err := selfconv.LocalHost(config)
		if err != nil {
			return nil, err
		}
		return []string{host}, nil
	case parsed.all:
		return config.HostNames(), nil
	}
	known := config.KnownHosts()
	hosts := make([]string, 0, len(parsed.hosts))
	seen := map[string]bool{}
	duplicated := false
	for _, raw := range parsed.hosts {
		name, err := safety.ValidateHostName(raw, known)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			duplicated = true
		}
		seen[name] = true
		hosts = append(hosts, name)
	}
	if duplicated {
		return nil, failure.Validation("host list contains duplicates")
	}
	return hosts, nil
}

// manifestCommit records which commit the working tree's manifest came from,
// or nothing when the tree is not a checkout. It is reporting context, so a
// failure to determine it is never fatal.
func manifestCommit(root string) *string {
	result := procx.Run(procx.Command{
		Argv:    []string{"git", "-C", root, "rev-parse", "HEAD"},
		Timeout: 3 * time.Second,
	})
	if result.Err != nil || result.Code != 0 {
		return nil
	}
	value := trimNewline(result.Stdout)
	if len(value) != 40 {
		return nil
	}
	return &value
}

func runSelfPull(parsed *arguments, streams Streams) (int, error) {
	root, err := CheckoutRoot()
	if err != nil {
		return reportScheduledFailure(streams, parsed.command, "self-schedule", err)
	}
	config, err := inventory.Load(root)
	if err != nil {
		return reportScheduledFailure(streams, parsed.command, "self-schedule", err)
	}
	// Exactly one local inventory identity, established before any network.
	if _, err := selfconv.LocalHost(config); err != nil {
		return reportScheduledFailure(streams, parsed.command, "self-schedule", err)
	}
	if !parsed.dryRun && !parsed.convergeOnly {
		refreshed, err := selfconv.Refresh(root, procx.Run)
		if err != nil {
			return reportScheduledFailure(streams, parsed.command, "self-schedule", err)
		}
		// Run the gated code, never an old program against a new manifest.
		// The Python launcher was the checkout, so re-executing it after the
		// refresh satisfied that by construction; a binary is not the checkout,
		// so the identity is checked instead.
		if err := selfconv.VerifyProgram(
			root, buildinfo.SourceCommit(), refreshed, procx.Run); err != nil {
			return reportScheduledFailure(streams, parsed.command, "self-schedule", err)
		}
	}
	document, code, err := selfconv.ConvergeSelf(root, parsed.dryRun, procx.Run)
	if err != nil {
		return reportScheduledFailure(streams, parsed.command, "self-schedule", err)
	}
	if err := printJSON(streams.Out, document); err != nil {
		return 1, err
	}
	printScheduledSummary(streams.Err, parsed.command, document,
		scheduledRecordPath(parsed, document), code)
	return code, nil
}

func runScheduledPull(parsed *arguments, streams Streams) (int, error) {
	root, err := CheckoutRoot()
	if err != nil {
		return reportScheduledFailure(streams, parsed.command, "scheduled-pull", err)
	}
	document, code, err := selfconv.ConvergeFleet(root, parsed.dryRun, procx.Run)
	if err != nil {
		return reportScheduledFailure(streams, parsed.command, "scheduled-pull", err)
	}
	if err := printJSON(streams.Out, document); err != nil {
		return 1, err
	}
	printScheduledSummary(streams.Err, parsed.command, document,
		scheduledRecordPath(parsed, document), code)
	if parsed.dryRun {
		return code, nil
	}
	if err := selfconv.PostReport(document, procx.Run); err != nil {
		_ = printJSON(streams.Out, map[string]any{"feed_error": err.Error()})
		if code == exitcode.Success {
			return exitcode.Partial, nil
		}
		return code, nil
	}
	return code, nil
}

// runInstallTimer installs a systemd user timer from the unit text embedded in
// this binary, so that a host with only the released binary and its release
// checkout can schedule itself.
func runInstallTimer(parsed *arguments, streams Streams) (int, error) {
	root, err := CheckoutRoot()
	if err != nil {
		return exitcode.Preflight, err
	}
	install := units.InstallSelfTimer
	if parsed.command == "install-pull-timer" {
		install = units.InstallFleetTimer
	}
	written, err := install(root, procx.Run)
	if err != nil {
		return exitcode.Preflight, err
	}
	for _, path := range written {
		fmt.Fprintf(streams.Out, "installed %s\n", path)
	}
	return exitcode.Success, nil
}

// reportScheduledFailure keeps an unattended run's output machine readable: a
// timer has nobody to read a traceback, so a failure is a report too. The
// same failure is repeated as one line on stderr, where the journal shows it
// without anyone having to read the document.
func reportScheduledFailure(
	streams Streams, verb, command string, err error,
) (int, error) {
	message := redact.Text(err.Error())
	_ = printJSON(streams.Out, map[string]any{
		"command":   command,
		"error":     message,
		"exit_code": exitcode.Preflight,
	})
	fmt.Fprintf(streams.Err, "upkeeper %s: failed before dispatch: %s; exit %d\n",
		verb, firstLineOf(message), exitcode.Preflight)
	return exitcode.Preflight, nil
}

// scheduledRecordPath is where the run record of a scheduled convergence
// was written, or "" for a dry run, which writes none.
func scheduledRecordPath(parsed *arguments, document map[string]any) string {
	if parsed.dryRun {
		return ""
	}
	store, err := runstore.Default()
	if err != nil {
		return ""
	}
	return store.Path(text(document["run_id"]))
}

// scheduledFailureStates are the component and host outcomes worth a line on
// stderr. A deferral is not a failure: the run said why it waited.
var scheduledFailureStates = map[string]bool{
	"failed": true, "rolled-back": true, "rollback-failed": true, "lock-contention": true,
}

// printScheduledSummary writes the failure summary of a scheduled run to
// stderr: one line per failed component, or per failed host when no component
// got as far as failing, and a final line naming the run record and the exit
// code. The JSON document on stdout is untouched; this is the part a person
// reads in the journal. A clean run writes nothing.
func printScheduledSummary(
	err io.Writer, command string, document map[string]any, recordPath string, code int,
) {
	lines := []string{}
	for _, item := range list(document["hosts"]) {
		host, _ := item.(map[string]any)
		name := text(host["host"])
		componentFailed := false
		for _, entry := range list(host["components"]) {
			component, _ := entry.(map[string]any)
			state := text(component["state"])
			if !scheduledFailureStates[state] {
				continue
			}
			componentFailed = true
			lines = append(lines, fmt.Sprintf("upkeeper %s: %s %s %s: %s",
				command, name, text(component["name"]), state, firstLineOf(text(component["error"]))))
		}
		if componentFailed || !scheduledFailureStates[text(host["result"])] {
			continue
		}
		lines = append(lines, fmt.Sprintf("upkeeper %s: %s %s: %s",
			command, name, text(host["result"]), firstLineOf(errorText(host))))
	}
	if len(lines) == 0 && code == exitcode.Success {
		return
	}
	for _, line := range lines {
		fmt.Fprintln(err, line)
	}
	if recordPath == "" {
		fmt.Fprintf(err, "upkeeper %s: run %s not recorded (dry run); exit %d\n",
			command, text(document["run_id"]), code)
		return
	}
	fmt.Fprintf(err, "upkeeper %s: run %s recorded at %s; exit %d\n",
		command, text(document["run_id"]), recordPath, code)
}

// firstLineOf keeps the first line of a message whole: the line is what the
// journal shows, and a refusal's remediation lives on it.
func firstLineOf(value string) string {
	trimmed := strings.TrimSpace(value)
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
		return strings.TrimSpace(trimmed[:index])
	}
	return trimmed
}

func trimNewline(value string) string {
	for len(value) > 0 &&
		(value[len(value)-1] == '\n' || value[len(value)-1] == '\r' ||
			value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}
	return value
}
