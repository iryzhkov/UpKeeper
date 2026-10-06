package cli

// Per-invocation instrumentation spool: one JSON line per run of the upkeeper
// command, appended to the same tool-feedback spool the fleet's MCP servers
// already write. Until now every claim about how this CLI behaves for an agent
// rested on a scheduled probe; this records what actually happened, one line
// per call, on every host and from every harness. UpKeeper is called less
// often than the other fleet tools and its refusals are the ones that block a
// release, so the per-verb refusal rate is worth more here per call than
// anywhere else.
//
// Three properties decide whether the spool is worth shipping, and each one is
// enforced here rather than left to the caller.
//
// It carries no argument values. The record holds the verb path, the flag keys
// and an outcome class, and nothing else that came from the command line. A
// value can be a host name, a repository URL, a hold reason or a token, and
// these records are shipped to another host, so the safe set is the set of
// names: a word extends the verb path only when it names a command this
// program publishes, and a flag key is recorded only when the parser declares
// it. An error is recorded as the name of its exit code rather than as its
// message, because a message quotes what the caller typed and what a host
// answered.
//
// It never changes what the caller sees. Every failure in here - no directory,
// a full disk, a read-only home - is dropped in silence. Losing an event is
// better than turning a working command into a failing one, and a warning on
// standard error would corrupt the output of every verb an agent parses.
//
// Its cost is bounded. One O_APPEND write of a short line, no fsync, no
// network, no retry, and a record that exceeds spoolRecordLimit is dropped.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/buildinfo"
	"github.com/iryzhkov/UpKeeper/internal/exitcode"
)

// spoolSource names this tool's directory in the spool. Every tool that writes
// the spool has its own, so one report can rank them side by side.
const spoolSource = "upkeeper"

const (
	// spoolMaxArgs bounds the flag keys recorded for one call.
	spoolMaxArgs = 24
	// spoolMaxIdentLen bounds a session or client identifier that crossed a
	// process boundary, because it is written into the line.
	spoolMaxIdentLen = 128
	// spoolRecordLimit bounds the encoded record. A line over it is dropped:
	// the spool is a measurement, not a log.
	spoolRecordLimit = 4096
	// spoolUndeclaredFlag stands for a word that was shaped like a flag and is
	// not one this program declares. Recording the word itself would be
	// recording whatever the caller put after a dash.
	spoolUndeclaredFlag = "?"
)

// spoolRecord is one line of the spool. The field names and their order are
// the shape the tool-feedback reader already consumes from the MCP producers;
// exit is this producer's addition, because a CLI has an exit code and an MCP
// server does not.
type spoolRecord struct {
	TS      string   `json:"ts"`
	Host    string   `json:"host"`
	Session string   `json:"session"`
	Proc    string   `json:"proc"`
	Client  string   `json:"client,omitempty"`
	Ver     string   `json:"ver,omitempty"`
	Seq     int      `json:"seq"`
	Tool    string   `json:"tool"`
	Args    []string `json:"args,omitempty"`
	Outcome string   `json:"outcome"`
	OK      bool     `json:"ok"`
	Err     string   `json:"err,omitempty"`
	Exit    int      `json:"exit"`
	Ms      int64    `json:"ms"`
}

// The five outcome classes an agent actually experiences, mapped from the
// stable exit codes this program already publishes rather than from a taxonomy
// invented for the spool.
const (
	// spoolOK is a command that did what it was asked. It includes exit 10:
	// drift is a report's finding, not a refusal, and counting it as friction
	// would make every honest status report look like a failure.
	spoolOK = "ok"
	// spoolUsage is a refusal the caller can fix on its own side: a malformed
	// command line (2) or a preflight refusal (30), which is what this program
	// returns when it refuses before touching anything.
	spoolUsage = "usage"
	// spoolState is a refusal about the fleet: some hosts converged and some
	// did not (20), a deployment failed (40) or a rollback failed (50). The
	// command was well formed and the answer will not change by retyping it.
	spoolState = "state"
	// spoolTransport is a failure to get the work done at all because
	// something else holds the resource (60). It is the class whose right
	// response is to come back later.
	spoolTransport = "transport"
	// spoolInternal is exit 1, which this program returns only for a failure
	// it did not expect, so that it can never be mistaken for a clean refusal.
	spoolInternal = "internal"
)

// spoolClassUnexpected names exit 1, which exitcode.Names deliberately does
// not, because it is the code for everything that was never classified.
const spoolClassUnexpected = "unexpected"

// spoolErrClass names the outcome of an invocation, from the stable exit codes
// this program publishes. It is deliberately not the error message: a message
// quotes the host, the repository or the reason that produced it, and the
// record is shipped to another host. exitcode.Names is the same table the
// reports use, so a spooled class and a printed result read the same.
func spoolErrClass(code int) string {
	if code == exitcode.Success {
		return ""
	}
	if code == exitcode.Usage {
		return "usage-error"
	}
	if name, named := exitcode.Names[code]; named {
		return name
	}
	return spoolClassUnexpected
}

// spoolOutcome maps an exit code onto the five classes an agent experiences.
// The mapping is by code rather than by class name so that a code this
// program adds later is internal until somebody classifies it deliberately.
func spoolOutcome(code int) string {
	switch code {
	case exitcode.Success, exitcode.Drift:
		return spoolOK
	case exitcode.Usage, exitcode.Preflight:
		return spoolUsage
	case exitcode.Partial, exitcode.Deployment, exitcode.Rollback:
		return spoolState
	case exitcode.LockContention:
		return spoolTransport
	default:
		return spoolInternal
	}
}

// spoolEnabled reports whether invocations are recorded. Off unless someone
// asked for it, in the same way and by the same switches as the MCP producers
// that share this spool: create the file "enabled" in the spool directory, or
// set UPKEEPER_FRICTION=1, or set TOOLFEEDBACK=1 to move every tool together.
// Either variable set to 0 turns it off again.
func spoolEnabled() bool {
	for _, name := range []string{"UPKEEPER_FRICTION", "TOOLFEEDBACK"} {
		switch os.Getenv(name) {
		case "1":
			return true
		case "0":
			return false
		}
	}
	dir := spoolDir()
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "enabled"))
	return err == nil
}

// spoolDir is where the spool files live, one directory per tool and one file
// per host per day. The precedence is the one the reader and the other
// producers use, so that pointing TOOLFEEDBACK_DIR at a scratch directory
// moves the whole spool at once - which is how the tests keep out of the
// operator's real one.
func spoolDir() string {
	for _, name := range []string{"UPKEEPER_FRICTION_DIR", "TOOLFEEDBACK_DIR"} {
		if dir := os.Getenv(name); dir != "" {
			return dir
		}
	}
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "toolfeedback")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "toolfeedback")
}

// spoolIdent keeps an identifier that came from the environment to the
// characters an identifier is made of, so that a spool line stays one line
// whatever an environment held, and bounds its length.
func spoolIdent(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > spoolMaxIdentLen {
		value = value[:spoolMaxIdentLen]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return -1
		}
	}, value)
}

// spoolSession is the agent session this invocation belongs to, as the harness
// exported it. A harness that exports none leaves the field empty rather than
// costing the record.
func spoolSession() string {
	for _, name := range []string{
		"TOOLFEEDBACK_SESSION",
		"UPKEEPER_SESSION_ID",
		"CLAUDE_CODE_SESSION_ID",
		"CODEX_SESSION_ID",
		"OPENCODE_SESSION_ID",
	} {
		if value := spoolIdent(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// spoolClient is the harness that ran this command. The reason the spool is
// written by the binary rather than derived from Claude Code's transcripts is
// that a call made from Codex or OpenCode leaves no transcript to derive it
// from, and those are exactly the harnesses that read the most stale
// documentation, so a transcript-derived report would under-count them.
func spoolClient() string {
	if value := spoolIdent(os.Getenv("TOOLFEEDBACK_CLIENT")); value != "" {
		return value
	}
	for _, probe := range []struct {
		name      string
		variables []string
	}{
		{"claude-code", []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}},
		{"codex", []string{"CODEX_HOME", "CODEX_SANDBOX"}},
		{"opencode", []string{"OPENCODE", "OPENCODE_BIN_PATH"}},
	} {
		for _, variable := range probe.variables {
			if os.Getenv(variable) != "" {
				return probe.name
			}
		}
	}
	return ""
}

// spoolHost is this machine's short name.
func spoolHost() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	if index := strings.IndexByte(host, '.'); index > 0 {
		host = host[:index]
	}
	return host
}

// spoolProc tells two invocations apart when they share a session and a
// timestamp, which is what a shell pipeline of two calls produces.
func spoolProc() string {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return ""
	}
	return hex.EncodeToString(buffer)
}

// spoolVersion is which build made the call, so that a failure rate before a
// fix can be compared with the rate after it.
func spoolVersion() string {
	commit := spoolIdent(buildinfo.SourceCommit())
	if commit == "" || commit == "unknown" {
		return buildinfo.Version
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return buildinfo.Version + "/" + commit
}

// spoolSubcommands are the commands that take a further word, with the words
// they take. They are named here because this program's parser enumerates them
// inline in args.go rather than in a table; a contract test in this package
// fails when the parser accepts a word this list does not have.
var spoolSubcommands = map[string][]string{
	"project":  {"add", "remove", "update"},
	"provider": {"add", "remove"},
	"hold":     {"show", "clear"},
}

// spoolCommands is every command this program publishes. The top-level list
// comes from commandSummaries, which is the same list the help screen prints,
// plus the two commands a controller invokes over SSH and that no operator
// types.
func spoolCommands() map[string]struct{} {
	commands := make(map[string]struct{}, len(commandSummaries)+2)
	for _, summary := range commandSummaries {
		commands[summary[0]] = struct{}{}
	}
	commands[agentCommand] = struct{}{}
	commands[probeCommand] = struct{}{}
	return commands
}

// spoolFlags is every option this program's parser accepts. The parser reads
// the arguments by hand rather than through a flagset, so there is no registry
// to derive this from; a contract test in this package reads args.go and fails
// when the parser accepts an option this list does not have.
var spoolFlags = []string{
	"all", "authored", "components", "converge", "default-ref", "dry-run", "expect-head", "expect-release-sha256", "help", "h",
	"hosts", "json", "limit", "models", "no-commit", "no-push", "omarchy-setup", "omarchy-setup-repo", "previous",
	"publish-diverged-instructions", "quota-pool", "reason", "reconcile-t3-transaction", "repository",
	"self", "setup-profile", "skip-unreachable", "steward", "timeout", "to", "type", "workers",
}

func spoolDeclaredFlags() map[string]struct{} {
	declared := make(map[string]struct{}, len(spoolFlags))
	for _, name := range spoolFlags {
		declared[name] = struct{}{}
	}
	return declared
}

// spoolVerbPath is the verb this invocation ran, as a full path: "upkeeper
// project add", not "upkeeper".
//
// The words are read from the front of the command line, where this program's
// parser requires them, and each is recorded only when it names a command this
// program publishes. That is what keeps operands and mistyped values out: a
// host name, a project name or a repository URL never stands in a position
// this function reads, and a word that is not a published command records as
// the bare program name.
func spoolVerbPath(argv []string) string {
	commands := spoolCommands()
	if len(argv) == 0 {
		return spoolSource
	}
	command := argv[0]
	if _, published := commands[command]; !published {
		return spoolSource
	}
	if len(argv) > 1 {
		for _, word := range spoolSubcommands[command] {
			if argv[1] == word {
				return spoolSource + " " + command + " " + word
			}
		}
	}
	return spoolSource + " " + command
}

// spoolArgKeys is the ordered set of option names this command line carried,
// with every value removed. The names are the part worth keeping: they say
// which way of addressing the work the caller reached for.
//
// An option is recorded only when this program's parser declares it, and
// otherwise as spoolUndeclaredFlag. Shape is not enough:
// "--sk-live-0xDEADBEEF" is shaped exactly like an option, and a caller that
// mistypes a value into option position would otherwise put it in a record
// that is shipped. An option this program does not have is still worth
// counting, so the fact is recorded and the word is not.
func spoolArgKeys(argv []string) []string {
	declared := spoolDeclaredFlags()
	var keys []string
	seen := map[string]bool{}
	for _, word := range argv {
		// A bare "--" hands the rest to whatever the caller is quoting.
		if word == "--" {
			break
		}
		if !strings.HasPrefix(word, "-") || word == "-" {
			continue
		}
		name := strings.TrimLeft(strings.SplitN(word, "=", 2)[0], "-")
		key := spoolUndeclaredFlag
		if _, ok := declared[name]; ok {
			key = name
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
		if len(keys) == spoolMaxArgs {
			break
		}
	}
	return keys
}

// newSpoolRecord builds the record for one finished invocation. It is separate
// from writing it so that a test can assert what a command line becomes
// without going near a file.
func newSpoolRecord(argv []string, code int, elapsed time.Duration) spoolRecord {
	return spoolRecord{
		TS:      time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Host:    spoolHost(),
		Session: spoolSession(),
		Proc:    spoolProc(),
		Client:  spoolClient(),
		Ver:     spoolVersion(),
		// One process is one call, so the sequence is always the first. The
		// field is kept because the reader orders on it and every other
		// producer writes it.
		Seq:     1,
		Tool:    spoolVerbPath(argv),
		Args:    spoolArgKeys(argv),
		Outcome: spoolOutcome(code),
		OK:      spoolOutcome(code) == spoolOK,
		Err:     spoolErrClass(code),
		Exit:    code,
		Ms:      elapsed.Milliseconds(),
	}
}

// writeSpoolRecord appends one record. Every failure is dropped in silence;
// see the file comment for why that is the correct behaviour here.
func writeSpoolRecord(record spoolRecord) {
	dir := spoolDir()
	if dir == "" {
		return
	}
	line, err := json.Marshal(record)
	if err != nil || len(line) > spoolRecordLimit {
		return
	}
	host := record.Host
	if host == "" {
		host = "unknown"
	}
	// One directory per tool, because a host name contains hyphens and so does
	// a tool name: with both in one file name there is no way to tell where
	// the first ends and the second begins.
	dir = filepath.Join(dir, spoolSource)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, host+"-"+time.Now().UTC().Format("2006-01-02")+".jsonl")
	// One O_APPEND write of a short line, so that several processes spooling
	// at once interleave whole lines rather than fragments. No fsync: a
	// measurement is not worth a disk flush on every command.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(line, '\n'))
	_ = file.Close()
}

// spoolInvocation records one finished invocation, if the spool is on.
func spoolInvocation(argv []string, code int, elapsed time.Duration) {
	if !spoolEnabled() {
		return
	}
	writeSpoolRecord(newSpoolRecord(argv, code, elapsed))
}
