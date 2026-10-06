// Package agent is the target-side half of a convergence.
//
// The controller hands the agent exactly one JSON payload on standard input and
// reads exactly one JSON result line from standard output. That wire format is
// unchanged from the Python implementation, which streamed a Python program to
// python3 on the target; the Go agent is the same program compiled into the
// binary, so a production convergence no longer needs a Python runtime anywhere.
//
// The agent observes every selected component, changes only what differs,
// verifies the result and stops at the first component it could not complete.
// It holds the target-side half of the deployment lock for the whole run.
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

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// HostRef is the little the agent is told about the machine it runs on. The
// controller deliberately sends no inventory: a target never learns the fleet.
type HostRef struct {
	Name          string  `json:"name"`
	OSID          string  `json:"os_id"`
	SetupCheckout *string `json:"setup_checkout"`
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
}

// StepFailed is a component step that did not complete. The host stops here.
type StepFailed struct{ Message string }

func (e *StepFailed) Error() string { return e.Message }

// Failf builds a StepFailed.
func Failf(format string, arguments ...any) *StepFailed {
	return &StepFailed{Message: fmt.Sprintf(format, arguments...)}
}

// RolledBack is a component that failed and was successfully restored. The
// host stops, but the machine is back in the state the run found it in.
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
	lockDirectory  string
	lockOwned      bool
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
var rank = []string{
	"converged", "updated", "ahead", "skipped", "deferred",
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
// forbids any interactive Git or SSH prompt.
func agentEnvironment(home string) []string {
	inherited := os.Environ()
	current := os.Getenv("PATH")
	if current == "" {
		current = "/usr/local/bin:/usr/bin:/bin"
	}
	path := strings.Join([]string{
		filepath.Join(home, ".local/share/mise/shims"),
		filepath.Join(home, ".local/bin"),
		filepath.Join(home, "go/bin"),
		"/usr/local/go/bin",
		current,
	}, ":")
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
	stop := false
	// Explicit T3 transaction reconciliation only rewrites that transaction's
	// journal, so it never performs the unrelated legacy updater retirement.
	if context.Payload.RetireUpdater && context.Payload.ReconcileT3Transaction == "" {
		retirement := RetireLegacyUpdater(context)
		result["legacy_updater"] = retirement
		state, _ := retirement["state"].(string)
		worst = state
		stop = state == "failed" || state == "deferred"
	}

	entries := make([]any, 0, len(context.Selected))
	for _, name := range context.Selected {
		entry := map[string]any{"name": name, "actions": []string{}}
		if stop {
			entry["state"] = "skipped"
			entries = append(entries, entry)
			continue
		}
		handler, known := handlers[name]
		if !known {
			entry["state"] = "failed"
			entry["error"] = "no adapter for component " + name
			entries = append(entries, entry)
			stop = true
			worst = "failed"
			continue
		}
		spec := context.Spec(name)
		actions := []string{}
		state, entryError := runComponent(context, handler, spec, entry, &actions)
		entry["actions"] = actions
		entry["state"] = state
		if entryError != "" {
			entry["error"] = entryError
		}
		if state == "deferred" || state == "rolled-back" ||
			state == "rollback-failed" || state == "failed" {
			stop = true
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
	case *RolledBack:
		return "rolled-back", typed.Message
	case *StepFailed:
		if strings.HasPrefix(typed.Message, "rollback failed") {
			return "rollback-failed", typed.Message
		}
		return "failed", typed.Message
	default:
		return "failed", err.Error()
	}
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
	data, err := io.ReadAll(io.LimitReader(input, 64<<20))
	if err != nil {
		return err
	}
	payload, err := DecodePayload(data)
	if err != nil {
		return err
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
