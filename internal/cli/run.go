package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/buildinfo"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/probe"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/publish"
	"github.com/iryzhkov/UpKeeper/internal/redact"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/report"
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
	case "self-pull":
		return runSelfPull(parsed, streams)
	case "scheduled-pull":
		return runScheduledPull(parsed, streams)
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
		value, commit, stored, err := release.LoadHistorical(root, revision, procx.Run)
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
		value, commit, stored, err := release.LoadHistorical(root, parsed.to, procx.Run)
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
		return reportScheduledFailure(streams, "self-schedule", err)
	}
	config, err := inventory.Load(root)
	if err != nil {
		return reportScheduledFailure(streams, "self-schedule", err)
	}
	// Exactly one local inventory identity, established before any network.
	if _, err := selfconv.LocalHost(config); err != nil {
		return reportScheduledFailure(streams, "self-schedule", err)
	}
	if !parsed.dryRun && !parsed.convergeOnly {
		if _, err := selfconv.Refresh(root, procx.Run); err != nil {
			return reportScheduledFailure(streams, "self-schedule", err)
		}
		// Run the gated code, never an old process against a new manifest.
		// Re-executing this binary's own path picks up a replacement that the
		// refresh installed, and costs nothing when there was none.
		if err := reexecConverge(); err != nil {
			return reportScheduledFailure(streams, "self-schedule", err)
		}
	}
	document, code, err := selfconv.ConvergeSelf(root, parsed.dryRun, procx.Run)
	if err != nil {
		return reportScheduledFailure(streams, "self-schedule", err)
	}
	if err := printJSON(streams.Out, document); err != nil {
		return 1, err
	}
	return code, nil
}

func runScheduledPull(parsed *arguments, streams Streams) (int, error) {
	root, err := CheckoutRoot()
	if err != nil {
		return reportScheduledFailure(streams, "scheduled-pull", err)
	}
	document, code, err := selfconv.ConvergeFleet(root, parsed.dryRun, procx.Run)
	if err != nil {
		return reportScheduledFailure(streams, "scheduled-pull", err)
	}
	if err := printJSON(streams.Out, document); err != nil {
		return 1, err
	}
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
// timer has nobody to read a traceback, so a failure is a report too.
func reportScheduledFailure(streams Streams, command string, err error) (int, error) {
	_ = printJSON(streams.Out, map[string]any{
		"command":   command,
		"error":     redact.Text(err.Error()),
		"exit_code": exitcode.Preflight,
	})
	return exitcode.Preflight, nil
}

func reexecConverge() error {
	executable, err := os.Executable()
	if err != nil {
		return failure.Value("cannot locate this binary to re-execute it: %v", err)
	}
	return syscall.Exec(
		executable, []string{executable, "self-pull", "--converge"}, os.Environ())
}

func trimNewline(value string) string {
	for len(value) > 0 &&
		(value[len(value)-1] == '\n' || value[len(value)-1] == '\r' ||
			value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}
	return value
}
