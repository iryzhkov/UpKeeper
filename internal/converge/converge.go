// Package converge implements `upkeeper pull` and `upkeeper rollback`: bring
// selected hosts onto an exact release manifest.
//
// The controller never streams shell to a target. It sends one JSON payload,
// with the validated release embedded, to the target's own upkeeper binary, and
// reads one JSON result line back. On the target that program observes each
// component, changes only what differs, verifies the result, and rolls a failed
// build back to what it found. Every host is locked on both sides for the
// duration and the controller writes a redacted run record.
package converge

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/locks"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/redact"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/iryzhkov/UpKeeper/internal/runid"
	"github.com/iryzhkov/UpKeeper/internal/runstore"
	"github.com/iryzhkov/UpKeeper/internal/transport"
)

// HostRunner dispatches one payload to one host and returns that host's entry
// in the report. It never returns an error: a host that could not be reached is
// a documented result, not an exception.
type HostRunner func(
	spec inventory.HostSpec, payload agent.Payload, timeout time.Duration, localOnly bool,
) map[string]any

// Options are the choices a convergence offers.
type Options struct {
	Hosts      []string
	Components []string
	DryRun     bool
	Timeout    time.Duration
	// ManifestCommit is the Git commit the release intent was read from, or nil
	// when the manifest was read from the working tree instead of history.
	ManifestCommit  *string
	StoredFiles     map[string]string
	Command         string
	SelfOnly        bool
	SkipUnreachable bool
	LockStore       *locks.Store
	RecordStore     *runstore.Store
	Runner          HostRunner
	Reachability    func(spec inventory.HostSpec) map[string]any
	Process         procx.Runner
	RunID           string
}

// Controller names the inventory host this process is running on, or
// "unknown" when this machine is not in the inventory at all.
func Controller(config *inventory.Config) string {
	identity := transport.LocalMachineIDSHA256()
	if identity == "" {
		return "unknown"
	}
	for _, name := range config.HostNames() {
		if config.Hosts[name].MachineIDSHA256 == identity {
			return name
		}
	}
	return "unknown"
}

// Fleet converges the selected hosts and returns the report and its exit code.
func Fleet(
	config *inventory.Config, value release.Release, options Options,
) (map[string]any, int, error) {
	if options.SelfOnly {
		identity := transport.LocalMachineIDSHA256()
		if len(options.Hosts) != 1 ||
			config.Hosts[options.Hosts[0]].MachineIDSHA256 != identity {
			return nil, 0, failure.Pull("--self may select only this inventory machine")
		}
	}
	selected, err := selectComponents(value, options.Components)
	if err != nil {
		return nil, 0, err
	}
	runID := options.RunID
	if runID == "" {
		minted, err := runid.New()
		if err != nil {
			return nil, 0, err
		}
		runID = minted
	}
	controller := Controller(config)
	lockStore := options.LockStore
	if lockStore == nil {
		store, err := locks.Default()
		if err != nil {
			return nil, 0, err
		}
		lockStore = store
	}
	recordStore := options.RecordStore
	if recordStore == nil {
		store, err := runstore.Default()
		if err != nil {
			return nil, 0, err
		}
		recordStore = store
	}
	lockController := controller
	if lockController == "unknown" {
		lockController = "controller"
	}
	desiredCommit := value.Digest()
	if options.ManifestCommit != nil {
		desiredCommit = *options.ManifestCommit
	}
	metadata, err := locks.NewMetadata(
		runID, lockController, os.Getpid(), desiredCommit, selected, time.Time{})
	if err != nil {
		return nil, 0, err
	}
	record, err := runstore.New(
		runID, commandName(options.Command), controller,
		options.ManifestCommit, selected, options.Hosts)
	if err != nil {
		return nil, 0, err
	}
	if !options.DryRun {
		if _, err := recordStore.Write(record); err != nil {
			return nil, 0, err
		}
	}
	stored := options.StoredFiles
	if stored == nil {
		stored, err = release.StoredFiles(config.Root, value)
		if err != nil {
			return nil, 0, err
		}
	}
	runner := options.Runner
	if runner == nil {
		runner = NewRunner(options.Process)
	}
	reachability := options.Reachability
	if reachability == nil {
		reachability = func(spec inventory.HostSpec) map[string]any {
			return CheckReachable(spec, options.Process)
		}
	}

	results := make([]any, 0, len(options.Hosts))
	for _, name := range options.Hosts {
		spec := config.Hosts[name]
		// A self-managed host converges through its own timer. Reaching into
		// it from a controller would race with that timer's lock.
		if spec.SelfManaged && !options.SelfOnly {
			results = append(results, map[string]any{
				"host": name, "reachable": false, "result": "skipped",
				"self_managed": true, "error": "converges through its local timer",
			})
			continue
		}
		if options.SkipUnreachable {
			if preflight := reachability(spec); preflight != nil {
				results = append(results, preflight)
				continue
			}
		}
		payload := agent.Payload{
			RunID:          runID,
			RetireUpdater:  true,
			AllowDowngrade: options.Command == "rollback",
			DryRun:         options.DryRun,
			Host: agent.HostRef{
				Name: name, OSID: spec.OSID, SetupCheckout: spec.SetupCheckout,
			},
			Components:  selected,
			Manifest:    value.Manifest,
			StoredFiles: stored,
			Lock: map[string]any{
				"schema_version":          exitcode.SchemaVersion,
				"run_id":                  runID,
				"controller":              metadata.Controller,
				"controller_pid":          metadata.ControllerPID,
				"desired_manifest_commit": metadata.DesiredManifestCommit,
				"components":              metadata.Components,
				"started_at":              metadata.StartedAt,
			},
		}
		if options.DryRun {
			results = append(results, runner(spec, payload, options.Timeout, options.SelfOnly))
			continue
		}
		if _, err := lockStore.Acquire(name, metadata); err != nil {
			if failure.Is(err, failure.KindLock) {
				results = append(results, map[string]any{
					"host": name, "reachable": true,
					"result": "lock-contention", "error": err.Error(),
				})
				continue
			}
			return nil, 0, err
		}
		results = append(results, dispatchWithLock(lockStore, name, runID, func() map[string]any {
			return runner(spec, payload, options.Timeout, options.SelfOnly)
		}))
	}

	exitCode := ExitCodeFor(results)
	report := map[string]any{
		"schema_version":          exitcode.SchemaVersion,
		"command":                 commandName(options.Command),
		"run_id":                  runID,
		"controller":              controller,
		"dry_run":                 options.DryRun,
		"self":                    options.SelfOnly,
		"release_sha256":          value.Digest(),
		"desired_manifest_commit": nullableString(options.ManifestCommit),
		"exit_code":               exitCode,
		"hosts":                   results,
	}
	if !options.DryRun {
		final := *record
		final.Phase = "finished"
		final.Result = "success"
		if exitCode != exitcode.Success {
			final.Result = "exit-" + itoa(exitCode)
		}
		final.Details = map[string]any{"hosts": results}
		if _, err := recordStore.Write(&final); err != nil {
			return nil, 0, err
		}
	}
	return report, exitCode, nil
}

// dispatchWithLock guarantees the lock is released exactly once, including when
// the dispatch panics.
func dispatchWithLock(
	store *locks.Store, host, runID string, dispatch func() map[string]any,
) map[string]any {
	defer func() { _ = store.Release(host, runID) }()
	return dispatch()
}

// selectComponents keeps the release's convergence order and refuses a name the
// release does not know.
//
// The T3 pair is selected together: installing a steward without the T3 it was
// tested against, or the reverse, is exactly the transition the pair exists to
// prevent.
func selectComponents(value release.Release, requested []string) ([]string, error) {
	components := value.Components()
	wanted := map[string]bool{}
	for _, name := range requested {
		wanted[name] = true
	}
	known := map[string]bool{}
	for _, name := range release.ComponentOrder {
		known[name] = true
	}
	unknown := []string{}
	for _, name := range requested {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sortStrings(unknown)
		return nil, failure.Pull("unknown components: %s", strings.Join(unknown, ", "))
	}
	selected := map[string]bool{}
	for _, name := range release.ComponentOrder {
		if _, present := components[name]; !present {
			continue
		}
		if requested == nil || wanted[name] {
			selected[name] = true
		}
	}
	if _, pairPresent := components["t3"]; pairPresent && (selected["t3"] || selected["t3-steward"]) {
		selected["t3"] = true
		selected["t3-steward"] = true
	}
	ordered := []string{}
	for _, name := range release.ComponentOrder {
		if selected[name] {
			ordered = append(ordered, name)
		}
	}
	return ordered, nil
}

// ExitCodeFor aggregates host outcomes into the process exit code.
//
// The order is deliberate and is the contract operators and timers read: a
// failed rollback outranks a failure, which outranks lock contention, which
// outranks an offline host, which outranks mere drift.
func ExitCodeFor(hosts []any) int {
	states := map[string]bool{}
	offline := false
	for _, item := range hosts {
		host, _ := item.(map[string]any)
		if state, ok := host["result"].(string); ok {
			states[state] = true
		}
		if !boolField(host, "reachable") &&
			!boolField(host, "skipped_unreachable") &&
			!boolField(host, "self_managed") {
			offline = true
		}
	}
	switch {
	case states["rollback-failed"]:
		return exitcode.Rollback
	case states["failed"] || states["rolled-back"]:
		return exitcode.Deployment
	case states["lock-contention"]:
		return exitcode.LockContention
	case offline:
		return exitcode.Partial
	case states["ahead"] || states["deferred"]:
		return exitcode.Drift
	default:
		return exitcode.Success
	}
}

// NewRunner returns the production dispatcher.
func NewRunner(process procx.Runner) HostRunner {
	return func(
		spec inventory.HostSpec, payload agent.Payload, timeout time.Duration, localOnly bool,
	) map[string]any {
		return RunHost(spec, payload, timeout, localOnly, process)
	}
}

// RunHost converges one host.
//
// When the host is this machine the agent runs in this process: there is no
// subprocess, no interpreter and no network involved, which is what makes a
// self-converging host free of any runtime beyond this binary. Every other host
// is reached over strict SSH and runs its own copy of this binary.
func RunHost(
	spec inventory.HostSpec, payload agent.Payload,
	timeout time.Duration, localOnly bool, process procx.Runner,
) map[string]any {
	isLocal := transport.IsLocal(spec)
	if localOnly && !isLocal {
		return map[string]any{
			"host": spec.Name, "reachable": false, "result": "failed",
			"error": "self identity changed; remote fallback refused",
		}
	}
	if isLocal {
		return runLocal(spec, payload, process)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return map[string]any{
			"host": spec.Name, "reachable": false, "result": "failed",
			"error": "invalid-agent-payload",
		}
	}
	outcome := transport.Invoke(spec, AgentSubcommand, string(encoded), timeout, process)
	if outcome.Err != nil {
		return map[string]any{
			"host": spec.Name, "reachable": false, "result": "failed",
			"error": errorKind(outcome),
		}
	}
	if outcome.MissingBinary() {
		return map[string]any{
			"host": spec.Name, "reachable": true, "transport": "ssh",
			"result": "failed", "error": transport.MissingBinaryDiagnosis,
			"returncode": outcome.Code,
			"stderr":     transport.Tail(outcome.Stderr, 2000),
		}
	}
	return decodeAgentOutput(spec.Name, "ssh", outcome.Code, outcome.Stdout, outcome.Stderr)
}

// AgentSubcommand is the hidden subcommand a target's binary is invoked with.
const AgentSubcommand = "__agent"

func runLocal(
	spec inventory.HostSpec, payload agent.Payload, process procx.Runner,
) map[string]any {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return map[string]any{
			"host": spec.Name, "reachable": false, "result": "failed",
			"error": "invalid-agent-payload",
		}
	}
	var output bytes.Buffer
	if err := agent.Run(bytes.NewReader(encoded), &output, process); err != nil {
		return map[string]any{
			"host": spec.Name, "reachable": false, "transport": "local",
			"result": "failed", "error": err.Error(),
		}
	}
	return decodeAgentOutput(spec.Name, "local", 0, output.String(), "")
}

// decodeAgentOutput reads the agent's single JSON result line. The last line
// that starts with a brace is the result, so an adapter that printed a warning
// cannot corrupt the report.
func decodeAgentOutput(host, kind string, code int, stdout, stderr string) map[string]any {
	lines := []string{}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "{") {
			lines = append(lines, line)
		}
	}
	if code != 0 || len(lines) == 0 {
		return map[string]any{
			"host": host, "reachable": false, "transport": kind,
			"result": "failed", "error": "agent-failed",
			"returncode": code, "stderr": transport.Tail(stderr, 2000),
		}
	}
	document, err := canonjson.DecodeObject([]byte(lines[len(lines)-1]))
	if err != nil {
		return map[string]any{
			"host": host, "reachable": false, "transport": kind,
			"result": "failed", "error": "invalid-agent-output",
		}
	}
	document["reachable"] = true
	document["transport"] = kind
	redacted, _ := redact.Value(document).(map[string]any)
	return redacted
}

// CheckReachable probes a host before dispatch, so that an offline machine
// costs one bounded connection instead of a whole deployment timeout.
//
// Only network unavailability is a quiet skip. An authentication failure is a
// real failure, because it means the fleet's trust assumptions changed.
func CheckReachable(spec inventory.HostSpec, process procx.Runner) map[string]any {
	if transport.IsLocal(spec) {
		return nil
	}
	result := transport.Check(spec, process)
	switch {
	case result.Reachable:
		return nil
	case result.Failed:
		return map[string]any{
			"host": spec.Name, "reachable": false, "result": "failed",
			"error": "reachability probe could not run",
		}
	case result.Offline && result.Stderr == "":
		return map[string]any{
			"host": spec.Name, "reachable": false, "result": "skipped",
			"skipped_unreachable": true, "error": "pre-dispatch connection timeout",
		}
	}
	entry := map[string]any{
		"host": spec.Name, "reachable": false,
		"result":              stateFor(result.Offline),
		"skipped_unreachable": result.Offline,
		"error":               reachabilityError(result.Offline),
		"stderr":              result.Stderr,
	}
	return entry
}

func stateFor(offline bool) string {
	if offline {
		return "skipped"
	}
	return "failed"
}

func reachabilityError(offline bool) string {
	if offline {
		return "unreachable before dispatch"
	}
	return "SSH preflight failed"
}

func errorKind(outcome transport.Outcome) string {
	if outcome.TimedOut {
		return "TimeoutExpired"
	}
	return "OSError"
}

func commandName(value string) string {
	if value == "" {
		return "pull"
	}
	return value
}

func boolField(value map[string]any, key string) bool {
	flag, _ := value[key].(bool)
	return flag
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for position := index; position > 0 && values[position-1] > values[position]; position-- {
			values[position-1], values[position] = values[position], values[position-1]
		}
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
