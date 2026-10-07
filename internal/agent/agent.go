// Package agent is the target-side half of a convergence.
//
// The controller hands the agent exactly one JSON payload on standard input and
// reads exactly one JSON result line from standard output. That wire format is
// unchanged from the Python implementation, which streamed a Python program to
// python3 on the target; the Go agent is the same program compiled into the
// binary, so a production convergence no longer needs a Python runtime anywhere.
//
// The agent observes every selected component, changes only what differs and
// verifies the result. A component it could not complete skips only the
// components that depend on it; a failed or incomplete rollback, an unknown
// adapter or a legacy updater it could not retire stops the host. It holds the target-side
// half of the deployment lock for the whole run.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/buildinfo"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/prereq"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// HostRef is the little the agent is told about the machine it runs on. The
// controller deliberately sends no inventory: a target never learns the fleet.
type HostRef struct {
	Name          string  `json:"name"`
	OSID          string  `json:"os_id"`
	SetupCheckout *string `json:"setup_checkout"`
	// InstallOSPackages is the host's inventory opt-in to having missing OS
	// prerequisites installed with passwordless sudo. Omitted when false, so a
	// payload for a host that did not opt in is exactly what it always was.
	InstallOSPackages bool `json:"install_os_packages,omitempty"`
	// AgentTools is the host's agent tool profile: the tools the tooling
	// component installs, registers and probes. Nil means every tool, and is
	// omitted, so a payload for a host without a profile is what it always was.
	AgentTools []string `json:"agent_tools,omitempty"`
}

// Payload is the complete instruction set for one host in one run.
type Payload struct {
	RunID          string            `json:"run_id"`
	RetireUpdater  bool              `json:"retire_updater"`
	AllowDowngrade bool              `json:"allow_downgrade"`
	DryRun         bool              `json:"dry_run"`
	ObserveOnly    bool              `json:"observe_only"`
	Host           HostRef           `json:"host"`
	Components     []string          `json:"components"`
	Manifest       map[string]any    `json:"manifest"`
	StoredFiles    map[string]string `json:"stored_files"`
	Lock           map[string]any    `json:"lock"`
	// ReconcileT3Transaction names one retained controlled T3 transaction left
	// at files-restored for the explicit, guarded reconciliation pull
	// --reconcile-t3-transaction asks for. Empty in every other run.
	ReconcileT3Transaction string `json:"reconcile_t3_transaction,omitempty"`
	// ExpectedProgramCommit is the source commit the controller requires this
	// binary to be built from, or "" for no requirement. It is omitted when
	// empty so that a payload without it is byte for byte what it always was.
	ExpectedProgramCommit string `json:"expected_program_commit,omitempty"`
}

// programCommit is the source commit of the running binary; a variable so that
// a test can play a binary built from another commit.
var programCommit = buildinfo.SourceCommit

// Blocked is a component that needs operator input before it can proceed.
// It blocks its dependents while allowing unrelated components to continue.
type Blocked struct{ Message string }

func (e *Blocked) Error() string { return e.Message }

// StepFailed is a component step that did not complete. Its dependents are
// skipped; the host carries on with every independent component.
type StepFailed struct{ Message string }

func (e *StepFailed) Error() string { return e.Message }

// Failf builds a StepFailed.
func Failf(format string, arguments ...any) *StepFailed {
	return &StepFailed{Message: fmt.Sprintf(format, arguments...)}
}

// RolledBack is a component that failed and was successfully restored: the
// machine is back in the state the run found it in. Like a failure, it skips
// only its dependents.
type RolledBack struct{ Message string }

func (e *RolledBack) Error() string { return e.Message }

// RolledBackf builds a RolledBack.
func RolledBackf(format string, arguments ...any) *RolledBack {
	return &RolledBack{Message: fmt.Sprintf(format, arguments...)}
}

// CommandResult is the agent's view of an external command, matching the shape
// the Python agent used so that component logic ports one to one.
type CommandResult struct {
	OK       bool
	Code     int
	Output   string
	TimedOut bool
}

// Context carries everything a component adapter needs.
type Context struct {
	Payload Payload
	Home    string
	Host    HostRef
	RunID   string
	DryRun  bool
	// Components is the manifest's component map after the host-specific
	// omarchy-setup checkout override has been applied.
	Components map[string]any
	Selected   []string
	Stored     map[string]string
	Env        []string
	Runtime    string
	Runner     procx.Runner
	// Now and Sleep are the clock a component waits by. Nil means the real
	// one; a test that waits for something sets both so that it never waits.
	Now   func() time.Time
	Sleep func(time.Duration)

	t3JournalFiles *t3JournalFiles // Owned journal IO; nil uses the filesystem.
	// stewardCheckBinary, when set, is the steward program that answers for
	// the running T3 instead of the installed one: the verified candidate, on
	// a host that has no steward yet.
	stewardCheckBinary string
	lockDirectory      string
	lockOwned          bool
	// lockHeld is true while this run holds the host's target lock, whether it
	// created the lock or re-entered the one its own controller holds. T3
	// transaction journals are published only under it.
	lockHeld bool
}

// now reads the context's clock.
func (c *Context) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// sleep pauses by the context's clock.
func (c *Context) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Spec returns the manifest entry for a component, or nil when the release does
// not carry it.
func (c *Context) Spec(name string) map[string]any {
	spec, _ := c.Components[name].(map[string]any)
	return spec
}

// Has reports whether the release carries a component at all.
func (c *Context) Has(name string) bool {
	_, present := c.Components[name]
	return present
}

// Path joins a home-relative path onto this machine's home directory.
func (c *Context) Path(elements ...string) string {
	return filepath.Join(append([]string{c.Home}, elements...)...)
}

// Run executes a command under the agent's environment and never raises.
func (c *Context) Run(argv []string, timeout time.Duration, dir string) CommandResult {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return commandResult(c.Runner(procx.Command{Argv: argv, Dir: dir, Env: c.Env, Timeout: timeout}))
}

// commandResult is the bounded result Run reports for one runner result.
func commandResult(result procx.Result) CommandResult {
	if result.TimedOut {
		return CommandResult{Code: -1, Output: "timeout", TimedOut: true}
	}
	if result.Err != nil {
		return CommandResult{Code: -1, Output: result.Err.Error()}
	}
	output := strings.TrimSpace(result.Stdout + result.Stderr)
	if len(output) > 2000 {
		output = output[len(output)-2000:]
	}
	return CommandResult{OK: result.Code == 0, Code: result.Code, Output: output}
}

// Must executes a command that has to succeed.
func (c *Context) Must(argv []string, timeout time.Duration, dir, what string) (string, error) {
	result := c.Run(argv, timeout, dir)
	if !result.OK {
		label := what
		if label == "" {
			label = argv[0]
		}
		tail := result.Output
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return "", Failf("%s failed: %s", label, tail)
	}
	return result.Output, nil
}

// SHA256File returns the digest of a file, or "" when it cannot be read.
func (c *Context) SHA256File(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// UnitActive reports whether a user unit is running.
func (c *Context) UnitActive(unit string) bool {
	return c.Run([]string{"systemctl", "--user", "is-active", unit}, 10*time.Second, "").Output == "active"
}

// UnitLoaded reports whether systemd knows a user unit at all.
func (c *Context) UnitLoaded(unit string) bool {
	result := c.Run(
		[]string{"systemctl", "--user", "show", unit, "-p", "LoadState", "--value"},
		10*time.Second, "")
	return result.OK && result.Output == "loaded"
}

// GitHead returns the commit a checkout is on, or "" when it is not a checkout.
func (c *Context) GitHead(checkout string) string {
	result := c.Run([]string{"git", "-C", checkout, "rev-parse", "HEAD"}, 15*time.Second, "")
	if !result.OK {
		return ""
	}
	return result.Output
}

// GitDirty reports whether a checkout has local modifications. A nil result
// means the question could not be answered, which is never treated as clean.
func (c *Context) GitDirty(checkout string) *bool {
	result := c.Run([]string{"git", "-C", checkout, "status", "--porcelain"}, 15*time.Second, "")
	if !result.OK {
		return nil
	}
	dirty := result.Output != ""
	return &dirty
}

// GitContains reports whether descendant has ancestor in its history.
func (c *Context) GitContains(checkout, ancestor, descendant string) bool {
	return c.Run(
		[]string{"git", "-C", checkout, "merge-base", "--is-ancestor", ancestor, descendant},
		15*time.Second, "").OK
}

// Handler is one component's observe/apply pair.
type Handler struct {
	Observe func(c *Context, spec map[string]any) (map[string]any, error)
	Apply   func(c *Context, spec map[string]any, before map[string]any, actions *[]string) (string, error)
}

var handlers = map[string]Handler{}

// Register adds a component adapter. Adapters register from their own file's
// init so that the agent's dispatch table stays a single, obvious map.
func Register(name string, handler Handler) {
	handlers[name] = handler
}

// Handlers exposes the registered adapters for tests.
func Handlers() map[string]Handler {
	copied := make(map[string]Handler, len(handlers))
	for name, handler := range handlers {
		copied[name] = handler
	}
	return copied
}

// rank orders the outcomes from best to worst. A host reports its worst one.
// A blocked component waits for its operator to provide something, which is
// worse than a deferral and better than any failure.
var rank = []string{
	"converged", "updated", "ahead", "skipped", "deferred", "blocked",
	"rolled-back", "failed", "rollback-failed",
}

func rankOf(state string) int {
	for index, name := range rank {
		if name == state {
			return index
		}
	}
	return len(rank)
}

// NewContext prepares the agent's execution environment for a payload.
func NewContext(payload Payload, runner procx.Runner) (*Context, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve the home directory: %w", err)
	}
	components := map[string]any{}
	if manifest, ok := payload.Manifest["components"].(map[string]any); ok {
		for name, value := range manifest {
			components[name] = value
		}
	}
	// A host may keep omarchy-setup somewhere other than the release's default.
	if payload.Host.SetupCheckout != nil && *payload.Host.SetupCheckout != "" {
		if setup, ok := components["omarchy-setup"].(map[string]any); ok {
			overridden := make(map[string]any, len(setup))
			for key, value := range setup {
				overridden[key] = value
			}
			overridden["path"] = *payload.Host.SetupCheckout
			components["omarchy-setup"] = overridden
		}
	}
	if runner == nil {
		runner = procx.Run
	}
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	context := &Context{
		Payload:    payload,
		Home:       home,
		Host:       payload.Host,
		RunID:      payload.RunID,
		DryRun:     payload.DryRun,
		Components: components,
		Selected:   payload.Components,
		Stored:     payload.StoredFiles,
		Env:        agentEnvironment(home),
		Runtime:    runtime,
		Runner:     runner,
	}
	context.lockDirectory = filepath.Join(
		home, ".local", "state", "dev-fleet", "locks", payload.Host.Name+".lock")
	return context, nil
}

// agentEnvironment puts the fleet's tool directories in front of PATH and
// forbids any interactive Git or SSH prompt. Every command the agent runs gets
// this PATH, and is looked up on it, so a host's shell startup files never
// decide whether a tool is found.
func agentEnvironment(home string) []string {
	inherited := os.Environ()
	path := prereq.AgentPath(home, os.Getenv("PATH"))
	environment := make([]string, 0, len(inherited)+3)
	for _, entry := range inherited {
		if strings.HasPrefix(entry, "PATH=") ||
			strings.HasPrefix(entry, "GIT_TERMINAL_PROMPT=") ||
			strings.HasPrefix(entry, "GIT_SSH_COMMAND=") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment,
		"PATH="+path,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=10",
	)
}

// Execute converges this machine and returns the result document the
// controller parses.
func Execute(context *Context) map[string]any {
	if context.Payload.ObserveOnly {
		return ObserveRelease(context)
	}
	result := map[string]any{
		"host":       context.Host.Name,
		"run_id":     context.RunID,
		"dry_run":    context.DryRun,
		"components": []any{},
	}
	// The program fence: a controller that names the program a release was
	// gated with is never served by another one. An old binary applying a new
	// manifest is exactly the mismatch the self-pull path refuses, and on this
	// path nothing else would notice it. It is checked before the lock, so a
	// refused payload has touched nothing.
	if expected := context.Payload.ExpectedProgramCommit; expected != "" {
		if running := programCommit(); running != expected {
			result["result"] = "failed"
			result["program"] = map[string]any{"expected_commit": expected, "running_commit": running}
			result["error"] = fmt.Sprintf(
				"this host runs upkeeper built from %s and the release requires %s; "+
					"update it with `upkeeper self-pull` on the host, or install that "+
					"release's binary, before converging it", running, expected)
			return result
		}
	}
	owner, err := context.acquireLock()
	if err != nil {
		result["result"] = "failed"
		result["error"] = err.Error()
		return result
	}
	if owner != nil {
		result["result"] = "lock-contention"
		result["lock_owner"] = owner
		return result
	}
	defer context.releaseLock()

	worst := "converged"
	// abortedBy names what stopped the host, and abortState its outcome. Once
	// it is set, every later component is skipped without being observed.
	abortedBy, abortState := "", ""
	// Explicit T3 transaction reconciliation only rewrites that transaction's
	// journal, so it never performs the unrelated legacy updater retirement.
	if context.Payload.RetireUpdater && context.Payload.ReconcileT3Transaction == "" {
		retirement := RetireLegacyUpdater(context)
		result["legacy_updater"] = retirement
		state, _ := retirement["state"].(string)
		worst = state
		if state == "failed" || state == "deferred" {
			abortedBy, abortState = "legacy-updater", state
		}
	}

	entries := make([]any, 0, len(context.Selected))
	// unavailable holds the reported state of every component that did not
	// complete, and roots the first component in its chain that did not; the
	// dependents of an unavailable component are skipped in turn.
	unavailable := map[string]string{}
	roots := map[string]string{}
	for _, name := range context.Selected {
		entry := map[string]any{"name": name, "actions": []string{}}
		if abortedBy != "" {
			entry["state"] = "skipped"
			entry["aborted_by"] = abortedBy
			entry["error"] = "skipped: run aborted after " + abortState + " " + abortedBy
			entries = append(entries, entry)
			continue
		}
		if dependency := unavailableDependency(name, unavailable); dependency != "" {
			root := roots[dependency]
			unavailable[name], roots[name] = "skipped", root
			entry["state"] = "skipped"
			entry["skipped_dependency"] = dependency
			entry["skipped_root"] = root
			reason := "skipped: depends on " + unavailable[dependency] + " " + dependency
			if root != dependency {
				reason += "; root cause " + unavailable[root] + " " + root
			}
			entry["error"] = reason
			entries = append(entries, entry)
			continue
		}
		handler, known := handlers[name]
		if !known {
			entry["state"] = "failed"
			entry["error"] = "no adapter for component " + name
			entries = append(entries, entry)
			abortedBy, abortState = name, "failed"
			worst = "failed"
			continue
		}
		spec := context.Spec(name)
		actions := []string{}
		if blocker := requirementsBlock(context, name, spec, &actions); blocker != "" {
			// Nothing was changed and nothing is observed: the operator has
			// to provide something first, and the message says what.
			unavailable[name], roots[name] = "blocked", name
			entry["actions"] = actions
			entry["state"] = "blocked"
			entry["error"] = blocker
			entries = append(entries, entry)
			if rankOf("blocked") > rankOf(worst) {
				worst = "blocked"
			}
			continue
		}
		state, entryError := runComponent(context, handler, spec, entry, &actions)
		entry["actions"] = actions
		entry["state"] = state
		if entryError != "" {
			entry["error"] = entryError
		}
		if dependencyStates[state] {
			unavailable[name], roots[name] = state, name
		}
		// A failed or incomplete rollback leaves the host in a state the run
		// neither found nor restored, so nothing else is applied on top of it.
		if restorationIncomplete(state, entryError) {
			abortedBy, abortState = name, state
		}
		if after, err := observeQuietly(context, handler, spec); err != nil {
			entry["after"] = map[string]any{"error": err.Error()}
		} else {
			entry["after"] = after
		}
		entries = append(entries, entry)
		if rankOf(state) > rankOf(worst) {
			worst = state
		}
	}
	result["components"] = entries
	result["harness"] = HarnessReport(context)
	result["result"] = worst
	return result
}

func runComponent(
	context *Context, handler Handler, spec map[string]any,
	entry map[string]any, actions *[]string,
) (state string, entryError string) {
	before, err := handler.Observe(context, spec)
	if err != nil {
		return classify(err)
	}
	entry["before"] = before
	state, err = handler.Apply(context, spec, before, actions)
	if err != nil {
		return classify(err)
	}
	return state, ""
}

func classify(err error) (string, string) {
	switch typed := err.(type) {
	case *Blocked:
		return "blocked", typed.Message
	case *RolledBack:
		return "rolled-back", typed.Message
	case *StepFailed:
		if rollbackFailed(typed.Message) {
			return "rollback-failed", typed.Message
		}
		return "failed", typed.Message
	default:
		if rollbackFailed(err.Error()) {
			return "rollback-failed", err.Error()
		}
		return "failed", err.Error()
	}
}

// rollbackFailed reports whether a failure says the component could not put
// back what the run found. Most adapters lead with "rollback failed", but some
// name what they restored first ("Jocasta rollback failed; inspect ...",
// "controlled Codex tree rollback failed"), and an interrupted T3 transaction
// says "rollback incomplete". Every such failure stops the host: a component
// that ran after it would build on a state nobody can vouch for.
func rollbackFailed(message string) bool {
	return strings.Contains(message, "rollback failed") ||
		strings.Contains(message, "rollback incomplete")
}

// restorationIncomplete reports whether a component outcome leaves the host
// unrestored, which stops the host. That is every rollback-failed outcome, and
// every failure that says a rollback or restoration did not finish without the
// words "rollback failed": "files restored; previous service restoration
// failed" (T3), "steward restored but session hook restoration failed", a
// restore error joined to its cause (the Steward unit), and a rollback that
// stopped at a concurrent change and left it in place ("concurrent state
// preserved for recovery", "concurrent entry point preserved").
//
// A successful compensation is a typed RolledBack and never reaches this as a
// failure, so any failure that mentions a rollback or restoration is one that
// did not complete. Matching one too many only stops the host, which is what
// every failure did before a failure could skip only its dependents.
func restorationIncomplete(state, message string) bool {
	if state == "rollback-failed" {
		return true
	}
	if state != "failed" {
		return false
	}
	lower := strings.ToLower(message)
	for _, marker := range []string{"rollback", "restor", "preserved"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func observeQuietly(
	context *Context, handler Handler, spec map[string]any,
) (observed map[string]any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			observed, err = nil, fmt.Errorf("panic: %v", recovered)
		}
	}()
	return handler.Observe(context, spec)
}

// Run reads one payload from input, converges, and writes one result line.
func Run(input io.Reader, output io.Writer, runner procx.Runner) error {
	return RunFenced(input, output, runner, "")
}

// RunFenced is Run for an agent invoked with the program fence on its command
// line. The fence is the same requirement as the payload's
// expected_program_commit; a payload that names another program contradicts
// its own invocation and is refused, before the lock like any other fence.
func RunFenced(input io.Reader, output io.Writer, runner procx.Runner, program string) error {
	data, err := io.ReadAll(io.LimitReader(input, 64<<20))
	if err != nil {
		return err
	}
	payload, err := DecodePayload(data)
	if err != nil {
		return err
	}
	if program != "" {
		if payload.ExpectedProgramCommit != "" && payload.ExpectedProgramCommit != program {
			return fmt.Errorf("the payload requires program %s and the invocation requires %s",
				payload.ExpectedProgramCommit, program)
		}
		payload.ExpectedProgramCommit = program
	}
	context, err := NewContext(payload, runner)
	if err != nil {
		return err
	}
	result := Execute(context)
	encoded, err := canonjson.MarshalSpaced(result)
	if err != nil {
		return err
	}
	_, err = output.Write(append(encoded, '\n'))
	return err
}

// DecodePayload parses the controller's instruction document.
func DecodePayload(data []byte) (Payload, error) {
	object, err := canonjson.DecodeObject(data)
	if err != nil {
		return Payload{}, fmt.Errorf("agent payload is not a JSON object: %w", err)
	}
	var payload Payload
	if err := decodeStruct(object, &payload); err != nil {
		return Payload{}, err
	}
	// The manifest and the lock stay generic so that a component the agent does
	// not know about is carried through rather than silently dropped.
	if manifest, ok := object["manifest"].(map[string]any); ok {
		payload.Manifest = manifest
	}
	if lock, ok := object["lock"].(map[string]any); ok {
		payload.Lock = lock
	}
	return payload, nil
}

// SortedNames returns map keys in report order.
func SortedNames[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
