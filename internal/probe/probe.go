// Package probe collects one bounded, read-only observation of a host.
//
// The Python implementation streamed a Python program to "python3 -" on the
// target and parsed what it printed. The Go implementation keeps the document
// that program produced, key for key, but gathers it in this process: Collect
// is the former remote program rewritten in Go, and a remote host is asked for
// the same document by invoking its own upkeeper binary. Nothing on the
// production path executes Python any more.
//
// Every observation is read-only and every external command is bounded, because
// a probe that hangs on one unreachable host is worse than one that reports the
// host as offline.
package probe

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/failure"
	"github.com/iryzhkov/UpKeeper/internal/hostevidence"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/prereq"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/redact"
	"github.com/iryzhkov/UpKeeper/internal/safety"
	"github.com/iryzhkov/UpKeeper/internal/transport"
)

// Subcommand is the hidden subcommand a target's own binary answers with one
// probe document on standard output.
const Subcommand = "__probe"

// The bounds the Python probe used, preserved exactly. A command that needs the
// network gets more room than a local one, and nothing is unbounded.
const (
	commandTimeout  = 6 * time.Second
	lsRemoteTimeout = 10 * time.Second
	forgejoTimeout  = 8 * time.Second
)

// DefaultTimeout is how long a single host may take to answer, and
// MinimumTimeout and MaximumTimeout bound what an operator may ask for.
const (
	DefaultTimeout = 20 * time.Second
	MinimumTimeout = 5 * time.Second
	MaximumTimeout = 60 * time.Second
)

// MaximumConcurrentHosts is how many hosts are probed at once. The bound exists
// so that a large fleet cannot open an unbounded number of SSH connections.
const MaximumConcurrentHosts = 4

// outputLimit is the number of characters of command output the probe keeps.
// It is part of the observed document rather than a display detail: the count
// of dirty working-tree entries is derived from the truncated output, so
// changing the limit would change a reported value.
const outputLimit = 512

// These are variables rather than constants so that a test can point the probe
// at a fixture instead of the machine it runs on.
var (
	osReleasePath = "/etc/os-release"
	procPath      = "/proc"
)

// forgejoRemote is the Forgejo account the fleet authenticates against.
const forgejoRemote = "git@git.ryzhkov.dev"

// sourceRemotes are the canonical and mirror URLs of every component whose
// source reachability the probe reports. The SSH URL is tried first so that a
// host with a working deploy key reports "ssh" rather than falling back.
var sourceRemotes = []struct {
	component string
	urls      []string
}{
	{"t3-steward", []string{
		"git@github.com:iryzhkov/t3-steward.git",
		"https://github.com/iryzhkov/t3-steward.git",
	}},
	{"agent99", []string{
		"git@github.com:iryzhkov/agent99.git",
		"https://github.com/iryzhkov/agent99.git",
	}},
	{"nvim-configuration", []string{
		"git@github.com:iryzhkov/nvim-configuration.git",
		"https://github.com/iryzhkov/nvim-configuration.git",
	}},
	{"homelab-cli", []string{
		"git@github.com:iryzhkov/homelab-cli.git",
		"https://github.com/iryzhkov/homelab-cli.git",
	}},
}

// summary is what the probe keeps about one command it ran. It mirrors the
// dictionary the Python probe's run() helper returned.
type summary struct {
	// ok reports a clean exit. A command that could not be started at all is
	// not ok and has no output, which is how the Python probe recorded an
	// OSError or a timeout.
	ok     bool
	output string
}

// runCommand executes one bounded command and summarises it.
func runCommand(runner procx.Runner, command procx.Command) summary {
	result := runner(command)
	if result.Err != nil {
		return summary{}
	}
	// Python preferred standard output whenever the stream was non-empty and
	// only then fell back to standard error.
	raw := result.Stdout
	if raw == "" {
		raw = result.Stderr
	}
	return summary{ok: result.Code == 0, output: truncate(strings.TrimSpace(raw), outputLimit)}
}

// truncate keeps the first limit characters of a value.
func truncate(value string, limit int) string {
	characters := []rune(value)
	if len(characters) <= limit {
		return value
	}
	return string(characters[:limit])
}

// firstLine returns the first line of a command summary, or nil when the
// command produced nothing. The nil is meaningful: it is the JSON null the
// reports distinguish from an empty string.
func firstLine(command summary) any {
	if command.output == "" {
		return nil
	}
	if index := strings.IndexAny(command.output, "\r\n"); index >= 0 {
		return command.output[:index]
	}
	return command.output
}

// nullable renders an empty string as the JSON null the Python probe printed
// for a program it could not find.
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// Collect gathers the whole observation in this process and returns it as the
// document the Python remote probe printed: the same keys, the same nesting and
// the same value types. The caller re-encodes it, so the map is not ordered.
//
// A nil runner means the production runner.
func Collect(runner procx.Runner) map[string]any {
	if runner == nil {
		runner = procx.Run
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return collect(home, runner)
}

// collect is Collect with the home directory named explicitly, so that a test
// can observe a temporary tree instead of the operator's own home.
func collect(home string, runner procx.Runner) map[string]any {
	release := osRelease()
	architecture := firstLine(runCommand(runner, procx.Command{
		Argv: []string{"uname", "-m"}, Timeout: commandTimeout,
	}))
	systemd := runCommand(runner, procx.Command{
		Argv: []string{"systemctl", "--user", "is-system-running"}, Timeout: commandTimeout,
	})
	linger := runCommand(runner, procx.Command{
		Argv:    []string{"loginctl", "show-user", currentUser(), "-p", "Linger", "--value"},
		Timeout: commandTimeout,
	})

	sources := map[string]any{}
	for _, remote := range sourceRemotes {
		sources[remote.component] = sourceAccess(runner, remote.urls)
	}

	return map[string]any{
		"hostname":                hostname(),
		"user":                    currentUser(),
		"architecture":            architecture,
		"machine_id_sha256":       nullable(transport.LocalMachineIDSHA256()),
		"os":                      operatingSystem(release),
		"python":                  pythonRuntime(runner),
		"ansible":                 ansibleRuntime(runner),
		"systemd_user":            map[string]any{"state": firstLine(systemd), "linger": firstLine(linger)},
		"disk":                    map[string]any{"home_free_bytes": freeBytes(home)},
		"writable_existing_roots": writableRoots(home),
		"forgejo_access":          forgejoAccess(runner),
		"source_access":           sources,
		"components":              components(home, runner),
		"locks":                   locks(home),
		"prerequisites":           prerequisites(home, release, runner),
	}
}

// prerequisites reports the OS packages this host lacks for any component, with
// the command that installs each, so that doctor names a blocker before a
// converge runs into it. The search path is the one the agent runs with, never
// the SSH session's, because that is where the agent will look.
func prerequisites(home string, release map[string]string, runner procx.Runner) map[string]any {
	family := prereq.Family(release["ID"])
	if family == "" && goruntime.GOOS == "darwin" {
		family = prereq.Darwin
	}
	if family == "" {
		for _, like := range strings.Fields(release["ID_LIKE"]) {
			if family = prereq.Family(like); family != "" {
				break
			}
		}
	}
	env := append(os.Environ(), "PATH="+prereq.AgentPath(home, os.Getenv("PATH")))
	missing := map[string]any{}
	for _, component := range prereq.Components() {
		items := []any{}
		for _, item := range prereq.Catalogue[component] {
			if item.Present(env, runner) {
				continue
			}
			items = append(items, map[string]any{
				"name":    item.Name,
				"install": item.Remedy(family),
				"staging": item.Staging,
			})
		}
		if len(items) > 0 {
			missing[component] = items
		}
	}
	return map[string]any{"family": nullable(family), "missing": missing}
}

// hostname reports this machine's name, or nil when the kernel will not say.
func hostname() any {
	name, err := os.Hostname()
	if err != nil {
		return nil
	}
	return name
}

// currentUser reports the account the probe runs as. The environment is
// consulted first, the way Python's getpass.getuser does, so that a session
// which has deliberately assumed another identity is reported as that identity.
func currentUser() string {
	for _, name := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	if account, err := user.Current(); err == nil {
		return account.Username
	}
	return ""
}

// osRelease parses /etc/os-release into its raw key and value pairs.
func osRelease() map[string]string {
	values := map[string]string{}
	data, err := os.ReadFile(osReleasePath)
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		values[key] = strings.Trim(strings.TrimSpace(value), "\"")
	}
	return values
}

// operatingSystem reports the distribution identity the inventory pins.
func operatingSystem(release map[string]string) map[string]any {
	document := map[string]any{"id": nil, "version": "unknown"}
	if id, present := release["ID"]; present {
		document["id"] = id
	}
	if version, present := release["VERSION_ID"]; present {
		document["version"] = version
	}
	return document
}

// pythonRuntime reports whichever Python the host happens to have.
//
// The field is retained only so that the reports keep their shape across the
// migration. UpKeeper no longer needs Python on a target for anything, so a
// host without it reports nulls here and is still fully converged.
func pythonRuntime(runner procx.Runner) map[string]any {
	executable := procx.Lookup("python3")
	document := map[string]any{"version": nil, "executable": nullable(executable)}
	if executable == "" {
		return document
	}
	reported := firstLine(runCommand(runner, procx.Command{
		Argv: []string{executable, "--version"}, Timeout: commandTimeout,
	}))
	if text, ok := reported.(string); ok {
		// "python3 --version" prints "Python 3.11.2" where the Python probe
		// reported platform.python_version(), which is the bare number.
		document["version"] = strings.TrimSpace(strings.TrimPrefix(text, "Python "))
	}
	return document
}

// ansibleRuntime reports whether this host can act as an Ansible controller.
func ansibleRuntime(runner procx.Runner) map[string]any {
	available := procx.Lookup("ansible") != ""
	document := map[string]any{"available": available, "version": nil}
	if !available {
		return document
	}
	document["version"] = firstLine(runCommand(runner, procx.Command{
		Argv: []string{"ansible", "--version"}, Timeout: commandTimeout,
	}))
	return document
}

// freeBytes reports the space left for the home directory, or nil when the
// filesystem cannot be interrogated.
func freeBytes(home string) any {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(home, &stat); err != nil {
		return nil
	}
	// Free space is what an unprivileged account may still use, which is the
	// available count rather than the free count.
	return uint64(stat.Bavail) * uint64(stat.Bsize)
}

// writableRoots reports which of the directories a deployment writes into exist
// and are writable, keyed by their path relative to the home directory.
func writableRoots(home string) map[string]any {
	roots := map[string]any{}
	for _, relative := range []string{".local", ".local/bin", ".local/state"} {
		path := filepath.Join(home, relative)
		info, err := os.Stat(path)
		roots[relative] = err == nil && info.IsDir() && syscall.Access(path, 0o2) == nil
	}
	return roots
}

// forgejoAccess reports whether this host can authenticate to Forgejo.
//
// Forgejo answers a shell-less SSH session with a greeting and a non-zero exit
// status, so a successful authentication has to be recognised from the message
// as well as from the status.
func forgejoAccess(runner procx.Runner) bool {
	result := runCommand(runner, procx.Command{
		Argv: []string{
			"ssh", "-n", "-T",
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=yes",
			"-o", "ConnectTimeout=5",
			forgejoRemote,
		},
		Timeout: forgejoTimeout,
	})
	message := strings.ToLower(result.output)
	return result.ok ||
		strings.Contains(message, "authenticated") ||
		strings.Contains(message, "successfully")
}

// gitEnvironment is the environment every source check runs under: Git may
// never prompt, SSH may never accept an unknown host key, and a connection
// attempt is bounded.
//
// The connect timeout is appended after the shared environment because the
// last assignment of a variable is the one a child process sees.
func gitEnvironment() []string {
	return append(procx.GitEnvironment(),
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=5",
	)
}

// sourceAccess reports whether a component's source can still be fetched, and
// over which of its URLs.
func sourceAccess(runner procx.Runner, urls []string) map[string]any {
	for _, url := range urls {
		result := runCommand(runner, procx.Command{
			Argv:    []string{"git", "ls-remote", url, "HEAD"},
			Env:     gitEnvironment(),
			Timeout: lsRemoteTimeout,
		})
		if result.ok {
			via := "https"
			if strings.HasPrefix(url, "git@") {
				via = "ssh"
			}
			return map[string]any{"reachable": true, "via": via}
		}
	}
	return map[string]any{"reachable": false, "via": nil}
}

// gitInfo reports the state of a checkout without changing it.
func gitInfo(runner procx.Runner, path string) map[string]any {
	if _, err := os.Stat(path); err != nil {
		return map[string]any{"present": false}
	}
	head := runCommand(runner, procx.Command{
		Argv: []string{"git", "-C", path, "rev-parse", "HEAD"}, Timeout: commandTimeout,
	})
	if !head.ok {
		return map[string]any{"present": true, "git": false}
	}
	branch := runCommand(runner, procx.Command{
		Argv:    []string{"git", "-C", path, "symbolic-ref", "--short", "-q", "HEAD"},
		Timeout: commandTimeout,
	})
	status := runCommand(runner, procx.Command{
		Argv: []string{
			"git", "-C", path, "status", "--porcelain=v1", "--untracked-files=normal",
		},
		Timeout: commandTimeout,
	})
	var dirty any
	if status.ok {
		dirty = len(lines(status.output))
	}
	name := branch.output
	if value, ok := firstLine(branch).(string); ok {
		name = value
	}
	if name == "" {
		// A checkout that is not on a branch still has to name its position.
		name = "detached"
	}
	return map[string]any{
		"present":       true,
		"git":           true,
		"head":          firstLine(head),
		"branch":        name,
		"dirty_entries": dirty,
	}
}

// lines splits command output the way Python's splitlines does for the counts
// the probe reports: a trailing newline does not add an empty entry.
func lines(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(value, "\n"), "\n")
}

// metadata reports what a path is without reading it.
func metadata(path string) map[string]any {
	info, err := os.Stat(path)
	if err != nil {
		return map[string]any{"exists": false}
	}
	kind := "file"
	if info.IsDir() {
		kind = "directory"
	}
	return map[string]any{
		"exists": true,
		"kind":   kind,
		"mode":   fmt.Sprintf("%04o", info.Mode().Perm()),
		"size":   info.Size(),
	}
}

// activeBridges lists the agent99 bridges currently running on this host.
func activeBridges() []any {
	// The list is always an array in the document, never null.
	bridges := []any{}
	entries, err := os.ReadDir(procPath)
	if err != nil {
		return bridges
	}
	type process struct {
		pid      int
		document map[string]any
	}
	found := []process{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		directory := filepath.Join(procPath, entry.Name())
		command, err := os.ReadFile(filepath.Join(directory, "cmdline"))
		if err != nil || !bytes.Contains(command, []byte("agent99-bridge")) {
			continue
		}
		found = append(found, process{pid: pid, document: map[string]any{
			"pid":        pid,
			"executable": resolve(filepath.Join(directory, "exe")),
			"cwd":        resolve(filepath.Join(directory, "cwd")),
		}})
	}
	sort.Slice(found, func(first, second int) bool { return found[first].pid < found[second].pid })
	for _, item := range found {
		bridges = append(bridges, item.document)
	}
	return bridges
}

// resolve follows a symbolic link, and reports the link itself when the target
// cannot be read. That is what a non-strict realpath does, and a bridge owned
// by another account is worth reporting even without its executable.
func resolve(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return path
	}
	return target
}

// serviceProperties parses the "Key=Value" lines systemctl show prints.
func serviceProperties(runner procx.Runner) map[string]any {
	result := runCommand(runner, procx.Command{
		Argv: []string{
			"systemctl", "--user", "show", "t3-steward.service",
			"-p", "LoadState", "-p", "ActiveState", "-p", "SubState", "-p", "UnitFileState",
		},
		Timeout: commandTimeout,
	})
	properties := map[string]any{}
	for _, line := range lines(result.output) {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		properties[key] = value
	}
	return properties
}

// components reports the installed state of every component the manifest knows.
func components(home string, runner procx.Runner) map[string]any {
	stewardCommand := procx.Lookup("t3-steward")
	var stewardVersion any
	if stewardCommand != "" {
		stewardVersion = firstLine(runCommand(runner, procx.Command{
			Argv: []string{"t3-steward", "version"}, Timeout: commandTimeout,
		}))
	}
	commands := map[string]any{}
	for _, name := range []string{"feed", "t3-backlog"} {
		commands[name] = nullable(procx.Lookup(name))
	}
	return map[string]any{
		"t3-steward": map[string]any{
			"command": nullable(stewardCommand),
			"version": stewardVersion,
			"service": serviceProperties(runner),
			"config":  metadata(filepath.Join(home, ".config/t3-steward")),
			"state":   metadata(filepath.Join(home, ".local/state/t3-steward/state.db")),
			// A worker restart that waits for an idle worker, read exactly as
			// the component observes it; nil when none is pending.
			"worker_restart": agent.ObserveWorkerRestart(home, runner, time.Now()),
		},
		"agent99": map[string]any{
			"checkout":       gitInfo(runner, filepath.Join(home, ".local/share/nvim/lazy/agent99")),
			"active_bridges": activeBridges(),
		},
		"nvim-configuration": map[string]any{
			"checkout": gitInfo(runner, filepath.Join(home, ".config/nvim")),
		},
		"homelab-cli": map[string]any{
			"checkout": gitInfo(runner, filepath.Join(home, "Work/homelab-cli")),
			"commands": commands,
		},
		"agent-config": map[string]any{
			"config":        metadata(filepath.Join(home, ".config/agents")),
			"codex_skills":  metadata(filepath.Join(home, ".codex/skills")),
			"claude_skills": metadata(filepath.Join(home, ".claude/skills")),
		},
		// UpKeeper's own evidence on this host: its scheduled runs, hold
		// markers and timers, so a controller can explain this host's drift
		// from the host's own facts rather than saying it cannot.
		"upkeeper": hostevidence.Collect(home, runner, time.Now()),
	}
}

// locks lists the deployment locks this host currently holds.
func locks(home string) []any {
	held := []any{}
	entries, err := os.ReadDir(filepath.Join(home, ".local/state/dev-fleet/locks"))
	if err != nil {
		return held
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".lock") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		held = append(held, name)
	}
	return held
}

// decode turns a completed probe invocation into an observation document.
//
// The three failure documents are part of the contract the reports render, so
// they are reproduced exactly as the Python implementation wrote them.
func decode(stdout string, code int) map[string]any {
	if code != 0 {
		return map[string]any{
			"reachable":  false,
			"error":      "probe-failed",
			"returncode": code,
		}
	}
	payload, err := canonjson.DecodeObject([]byte(stdout))
	if err != nil {
		return map[string]any{"reachable": false, "error": "invalid-probe-output"}
	}
	return reachableDocument(payload)
}

// reachableDocument redacts a payload and marks it reachable. A payload that
// carries its own "reachable" key wins, exactly as the Python merge did.
func reachableDocument(payload map[string]any) map[string]any {
	document := map[string]any{"reachable": true}
	redacted, _ := redact.Value(payload).(map[string]any)
	for key, value := range redacted {
		document[key] = value
	}
	return document
}

// errorKind names why a probe could not run at all.
//
// The names are Python exception names because operators and stored reports
// already match on them; only the way they are produced has changed.
func errorKind(outcome transport.Outcome) string {
	if outcome.TimedOut {
		return "TimeoutExpired"
	}
	return "OSError"
}

// Host observes one inventory host.
//
// A host that is this machine is observed in process. Every other host is asked
// for the same document over SSH, from its own upkeeper binary. A nil runner
// means the production runner.
//
// A host that could not be contacted at all reports no transport, which is what
// the Python implementation did: the transport is only known once a command
// actually ran.
func Host(spec inventory.HostSpec, timeout time.Duration, runner procx.Runner) map[string]any {
	if runner == nil {
		runner = procx.Run
	}
	if transport.IsLocal(spec) {
		document := reachableDocument(Collect(runner))
		document["transport"] = "local"
		return document
	}
	outcome := transport.Invoke(spec, Subcommand, "", timeout, runner)
	if outcome.TimedOut || outcome.Err != nil {
		return map[string]any{"reachable": false, "error": errorKind(outcome)}
	}
	var document map[string]any
	if outcome.MissingBinary() {
		// The host answered, so this is a diagnosis rather than an outage: it
		// names the remedy instead of reporting an opaque exit status.
		document = map[string]any{
			"reachable": false,
			"error":     transport.MissingBinaryDiagnosis,
		}
	} else {
		document = decode(outcome.Stdout, outcome.Code)
	}
	document["transport"] = "ssh"
	return document
}

// Fleet observes every selected host, at most MaximumConcurrentHosts at a time.
//
// The result is keyed by host name. Go maps have no order, so the caller keeps
// the selection it passed in and hands the same slice to the report builder,
// which is what makes a report deterministic.
func Fleet(
	config *inventory.Config, selected []string, timeout time.Duration,
) (map[string]any, error) {
	return fleet(config, selected, timeout, procx.Run)
}

// fleet is Fleet with the runner named explicitly, so that a test can observe
// hosts without a network.
func fleet(
	config *inventory.Config, selected []string, timeout time.Duration, runner procx.Runner,
) (map[string]any, error) {
	if timeout < MinimumTimeout || timeout > MaximumTimeout {
		return nil, failure.Value("timeout must be between 5 and 60 seconds")
	}
	specs := make([]inventory.HostSpec, 0, len(selected))
	for _, name := range selected {
		spec, known := config.Hosts[name]
		if !known {
			// The Python implementation raised a KeyError here, which crashed
			// the process. A selection that names an unknown host is an input
			// error, so it is refused cleanly instead.
			return nil, failure.Validation("unknown host: %s", name)
		}
		specs = append(specs, spec)
	}

	observations := make(map[string]any, len(selected))
	var guard sync.Mutex
	var group sync.WaitGroup
	slots := make(chan struct{}, MaximumConcurrentHosts)
	for position, name := range selected {
		group.Add(1)
		go func(name string, spec inventory.HostSpec) {
			defer group.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			document := isolatedHost(spec, timeout, runner)
			guard.Lock()
			defer guard.Unlock()
			observations[name] = document
		}(name, specs[position])
	}
	group.Wait()
	return observations, nil
}

// isolatedHost keeps a failure inside the host it belongs to: one broken target
// must not cost the whole fleet its report.
func isolatedHost(
	spec inventory.HostSpec, timeout time.Duration, runner procx.Runner,
) (document map[string]any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// The panic value itself is not reported: it can carry a path or a
			// command line, and this document is printed and stored.
			document = map[string]any{"reachable": false, "error": "panic"}
		}
	}()
	return Host(spec, timeout, runner)
}

// Order returns the host names of a set of observations in report order. It is
// the fallback for a caller that has lost the selection it probed with.
func Order(observations map[string]any) []string {
	return safety.SortedKeys(observations)
}
