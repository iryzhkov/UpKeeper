package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/exitcode"
)

// spoolSecret is the argument value every leak test drives through the CLI. It
// is shaped like a credential on purpose: if any part of the spool ever copies
// an argument value instead of a name, this string is what shows up in the
// record, and the record is shipped to another host.
const spoolSecret = "sk-live-0xDEADBEEFCAFE-SECRET"

// useSpool points the spool at a scratch directory and turns it on, and
// answers that directory. Every test in this file calls it: a test must never
// append to the operator's real spool under ~/.local/share/toolfeedback.
func useSpool(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TOOLFEEDBACK_DIR", dir)
	t.Setenv("UPKEEPER_FRICTION_DIR", "")
	t.Setenv("UPKEEPER_FRICTION", "1")
	return dir
}

// runSpooled drives one command line through the same entry point the binary
// uses and answers its exit code and both output streams.
func runSpooled(argv []string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := MainSpooled("upkeeper", argv, Streams{
		In: strings.NewReader(""), Out: &out, Err: &errOut,
	})
	return code, out.String(), errOut.String()
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob the spool: %v", err)
	}
	return matches
}

// spoolContent is everything the spool holds, for the assertion that a value
// appears nowhere in it at all - not in a field this test knows to look at,
// and not in one added later either.
func spoolContent(t *testing.T, dir string) string {
	t.Helper()
	var all strings.Builder
	for _, path := range spoolFiles(t, dir) {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		all.Write(content)
	}
	return all.String()
}

func spoolRecords(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimRight(spoolContent(t, dir), "\n"), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("record is not JSON: %v (%q)", err, line)
		}
		records = append(records, record)
	}
	return records
}

func spoolField(t *testing.T, record map[string]any, key string) string {
	t.Helper()
	value, present := record[key]
	if !present {
		t.Fatalf("record has no %q: %v", key, record)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("record %q is %T, want a string", key, value)
	}
	return text
}

func spoolRecordKeys(record map[string]any) []string {
	var keys []string
	// args is omitted entirely when a command line carried no option, which is
	// what the reader expects.
	if recorded, present := record["args"].([]any); present {
		for _, value := range recorded {
			keys = append(keys, value.(string))
		}
	}
	return keys
}

// A successful invocation writes exactly one record, and its tool is the full
// verb path rather than the binary name.
func TestSpoolRecordsOneSuccessWithTheFullVerbPath(t *testing.T) {
	dir := useSpool(t)

	code, out, errOut := runSpooled([]string{"project", "add", "--help"})
	if code != exitcode.Success {
		t.Fatalf("exit code %d, want %d (stderr %q)", code, exitcode.Success, errOut)
	}
	if out == "" {
		t.Fatal("the help text printed nothing")
	}

	records := spoolRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("%d records, want exactly 1: %v", len(records), records)
	}
	record := records[0]
	if got := spoolField(t, record, "tool"); got != "upkeeper project add" {
		t.Errorf("tool is %q, want the full verb path %q", got, "upkeeper project add")
	}
	if got := spoolField(t, record, "outcome"); got != spoolOK {
		t.Errorf("outcome is %q, want %q", got, spoolOK)
	}
	if ok, _ := record["ok"].(bool); !ok {
		t.Errorf("ok is %v, want true", record["ok"])
	}
	for _, key := range []string{"ts", "host", "session", "proc", "seq", "tool", "outcome", "ok", "exit", "ms"} {
		if _, present := record[key]; !present {
			t.Errorf("record has no %q: %v", key, record)
		}
	}
}

// A refusal writes one record whose outcome is the refusal class, and the exit
// code the caller sees is the one Main returns without the spool.
func TestSpoolRecordsARefusalWithoutChangingTheExitCode(t *testing.T) {
	dir := useSpool(t)

	argv := []string{"pull"}
	code, _, errOut := runSpooled(argv)
	if code != exitcode.Usage {
		t.Fatalf("exit code %d, want %d (stderr %q)", code, exitcode.Usage, errOut)
	}
	// Main is the path the binary took before the spool existed. It writes no
	// record, so the comparison does not cost the test a second line.
	var out, plain bytes.Buffer
	want := Main("upkeeper", argv, Streams{In: strings.NewReader(""), Out: &out, Err: &plain})
	if want != code {
		t.Fatalf("MainSpooled returned %d, Main returns %d", code, want)
	}

	records := spoolRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("%d records, want exactly 1: %v", len(records), records)
	}
	record := records[0]
	if got := spoolField(t, record, "outcome"); got != spoolUsage {
		t.Errorf("outcome is %q, want %q: a malformed command line is a refusal the caller can fix", got, spoolUsage)
	}
	if got := spoolField(t, record, "err"); got != "usage-error" {
		t.Errorf("err is %q, want %q", got, "usage-error")
	}
	if ok, _ := record["ok"].(bool); ok {
		t.Error("ok is true for a refusal")
	}
	if got := spoolField(t, record, "tool"); got != "upkeeper pull" {
		t.Errorf("tool is %q, want %q", got, "upkeeper pull")
	}
}

// The test that matters most. A command line whose option values and
// positional arguments all carry a recognisable secret produces a record in
// which the secret appears nowhere at all. A spool that leaks argument values
// is worse than no spool, because these records are shipped.
//
// Every case carries one option this program does not declare, so that the
// parser refuses the line before anything reads a configuration, reaches a
// host or writes a marker. That is a property of the test, not of the spool:
// the record is built from the command line and does not depend on how far the
// command got.
func TestSpoolNeverRecordsAnArgumentValue(t *testing.T) {
	for _, testCase := range []struct {
		name string
		argv []string
		tool string
		keys []string
	}{
		{
			name: "secret in a host list and a release name",
			argv: []string{"pull", "--hosts", spoolSecret, "--to", spoolSecret, "--" + spoolSecret},
			tool: "upkeeper pull",
			keys: []string{"hosts", "to", spoolUndeclaredFlag},
		},
		{
			name: "secret in a project name and a repository URL",
			argv: []string{"project", "add", spoolSecret,
				"--repository", "git@github.com:x/" + spoolSecret + ".git", "--" + spoolSecret},
			tool: "upkeeper project add",
			keys: []string{"repository", spoolUndeclaredFlag},
		},
		{
			name: "secret in a hold reason",
			argv: []string{"hold", "clear", spoolSecret, "--reason", "deploy " + spoolSecret, "--" + spoolSecret},
			tool: "upkeeper hold clear",
			keys: []string{"reason", spoolUndeclaredFlag},
		},
		{
			name: "secret joined to the option with an equals sign",
			argv: []string{"status", "--limit=" + spoolSecret, "--" + spoolSecret},
			tool: "upkeeper status",
			keys: []string{"limit", spoolUndeclaredFlag},
		},
		{
			name: "secret standing where a command should",
			argv: []string{spoolSecret, "--json"},
			tool: "upkeeper",
			keys: []string{"json"},
		},
		{
			name: "secret standing where a subcommand should",
			argv: []string{"project", spoolSecret, "--" + spoolSecret},
			tool: "upkeeper project",
			keys: []string{spoolUndeclaredFlag},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := useSpool(t)
			runSpooled(testCase.argv)

			content := spoolContent(t, dir)
			if content == "" {
				t.Fatal("no record was written")
			}
			if strings.Contains(content, spoolSecret) {
				t.Fatalf("the secret reached the spool: %s", content)
			}
			// The same assertion for the pieces a partial copy would leave
			// behind: a fragment of the value, or the URL around it.
			for _, fragment := range []string{"DEADBEEF", "sk-live", "git@", "github.com", "deploy "} {
				if strings.Contains(content, fragment) {
					t.Fatalf("the spool carries %q from the command line: %s", fragment, content)
				}
			}

			records := spoolRecords(t, dir)
			if len(records) != 1 {
				t.Fatalf("%d records, want exactly 1", len(records))
			}
			if got := spoolField(t, records[0], "tool"); got != testCase.tool {
				t.Errorf("tool is %q, want %q", got, testCase.tool)
			}
			if got := spoolRecordKeys(records[0]); strings.Join(got, ",") != strings.Join(testCase.keys, ",") {
				t.Errorf("args are %v, want %v", got, testCase.keys)
			}
		})
	}
}

// The two allowlists the spool records from are the parser's own vocabulary.
// This program parses its arguments by hand, so neither can be derived at run
// time; they are read back out of the source here instead, so that an option
// or a subcommand added to the parser fails this test until it is added to the
// spool, rather than turning into a "?" nobody notices.
func TestSpoolAllowlistsMatchTheParser(t *testing.T) {
	source, err := os.ReadFile("args.go")
	if err != nil {
		t.Fatalf("read the parser: %v", err)
	}
	declared := spoolDeclaredFlags()
	parsed := map[string]struct{}{}
	for _, quoted := range regexp.MustCompile(`"--[a-z][a-z0-9-]*"`).FindAllString(string(source), -1) {
		name := strings.TrimLeft(strings.Trim(quoted, `"`), "-")
		parsed[name] = struct{}{}
		if _, present := declared[name]; !present {
			t.Errorf("the parser accepts --%s and the spool would record it as %q", name, spoolUndeclaredFlag)
		}
	}
	// -h and --help are recognised by isHelpFlag rather than by the parser's
	// option table, so they are the only names the scan cannot find.
	for _, name := range spoolFlags {
		if name == "help" || name == "h" {
			continue
		}
		if _, present := parsed[name]; !present {
			t.Errorf("the spool allows --%s and the parser does not accept it", name)
		}
	}

	// Every command the help screen lists is a verb path the spool records.
	for _, summary := range commandSummaries {
		if got := spoolVerbPath([]string{summary[0]}); got != "upkeeper "+summary[0] {
			t.Errorf("the help screen lists %q and the spool records it as %q", summary[0], got)
		}
	}
	// And every subcommand word the parser offers in its refusal.
	for command, words := range spoolSubcommands {
		sort.Strings(words)
		for _, word := range words {
			if !strings.Contains(string(source), `"`+word+`"`) {
				t.Errorf("the spool records %s %s and the parser names no such word", command, word)
			}
			if got := spoolVerbPath([]string{command, word}); got != "upkeeper "+command+" "+word {
				t.Errorf("verb path for %s %s is %q", command, word, got)
			}
		}
	}
}

// A spool that cannot be written changes neither the exit code nor a byte of
// either output stream. This is the one place in this program where silence is
// the correct behaviour.
func TestUnwritableSpoolChangesNothingTheCallerSees(t *testing.T) {
	argv := []string{"pull"}

	writable := useSpool(t)
	wantCode, wantOut, wantErr := runSpooled(argv)
	if len(spoolFiles(t, writable)) != 1 {
		t.Fatalf("the writable run wrote %d spool files, want 1", len(spoolFiles(t, writable)))
	}

	// A regular file where the spool's parent directory must be. MkdirAll
	// fails with ENOTDIR on it for every user, including root, so this test
	// does not quietly pass by running as somebody who can write anywhere.
	blocked := t.TempDir()
	obstruction := filepath.Join(blocked, "not-a-directory")
	if err := os.WriteFile(obstruction, []byte(""), 0o600); err != nil {
		t.Fatalf("write the obstruction: %v", err)
	}
	t.Setenv("TOOLFEEDBACK_DIR", filepath.Join(obstruction, "spool"))
	t.Setenv("UPKEEPER_FRICTION", "1")

	code, out, errOut := runSpooled(argv)
	if code != wantCode {
		t.Errorf("exit code %d with an unwritable spool, %d with a writable one", code, wantCode)
	}
	if out != wantOut {
		t.Errorf("stdout changed:\n unwritable %q\n writable   %q", out, wantOut)
	}
	if errOut != wantErr {
		t.Errorf("stderr changed:\n unwritable %q\n writable   %q", errOut, wantErr)
	}
	if entries, err := os.ReadDir(blocked); err != nil || len(entries) != 1 {
		t.Errorf("the blocked directory holds %v (err %v), want only the obstruction", entries, err)
	}
}

// The file layout and the record shape are the ones homelab-cli's toolfeedback
// reader consumes: <dir>/<source>/<host>-<YYYY-MM-DD>.jsonl, one JSON object
// per line, with the keys it reads.
func TestSpoolMatchesTheReaderShape(t *testing.T) {
	dir := useSpool(t)
	runSpooled([]string{"hold", "--help"})

	paths := spoolFiles(t, dir)
	if len(paths) != 1 {
		t.Fatalf("%d spool files, want 1: %v", len(paths), paths)
	}
	if source := filepath.Base(filepath.Dir(paths[0])); source != spoolSource {
		t.Errorf("the spool directory is %q, want %q", source, spoolSource)
	}
	// SPOOL_NAME_RE in homelab-cli/bin/toolfeedback.
	name := regexp.MustCompile(`^(?P<host>.+)-(?P<date>\d{4}-\d{2}-\d{2})\.jsonl$`)
	if !name.MatchString(filepath.Base(paths[0])) {
		t.Errorf("%q is not a spool file name the reader recognises", filepath.Base(paths[0]))
	}

	record := spoolRecords(t, dir)[0]
	// event_time in the reader parses this after replacing Z with +00:00.
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", spoolField(t, record, "ts")); err != nil {
		t.Errorf("ts %q is not the timestamp format the reader parses: %v", record["ts"], err)
	}
	for key, want := range map[string]string{
		"host": "string", "session": "string", "proc": "string", "tool": "string",
		"seq": "number", "ms": "number", "ok": "bool",
	} {
		value, present := record[key]
		if !present {
			t.Errorf("record has no %q, which the reader reads", key)
			continue
		}
		var got string
		switch value.(type) {
		case string:
			got = "string"
		case float64:
			got = "number"
		case bool:
			got = "bool"
		default:
			got = "other"
		}
		if got != want {
			t.Errorf("record %q is a %s, the reader wants a %s", key, got, want)
		}
	}
}

// Every stable exit code this program publishes maps onto one of the five
// classes an agent experiences, and no code is left to a default nobody chose.
func TestSpoolOutcomeMapsThePublishedExitCodes(t *testing.T) {
	for _, testCase := range []struct {
		code    int
		class   string
		outcome string
	}{
		{exitcode.Success, "", spoolOK},
		{exitcode.Drift, "drift", spoolOK},
		{exitcode.Partial, "partial", spoolState},
		{exitcode.Preflight, "preflight-failure", spoolUsage},
		{exitcode.Deployment, "deployment-failure", spoolState},
		{exitcode.Rollback, "rollback-failure", spoolState},
		{exitcode.LockContention, "lock-contention", spoolTransport},
		{exitcode.Usage, "usage-error", spoolUsage},
		{1, spoolClassUnexpected, spoolInternal},
	} {
		if class := spoolErrClass(testCase.code); class != testCase.class {
			t.Errorf("exit %d classes as %q, want %q", testCase.code, class, testCase.class)
		}
		if outcome := spoolOutcome(testCase.code); outcome != testCase.outcome {
			t.Errorf("exit %d is outcome %q, want %q", testCase.code, outcome, testCase.outcome)
		}
	}
	// Every code in the published table is classified deliberately; a code
	// added later falls to internal until somebody decides otherwise.
	for code := range exitcode.Names {
		if spoolErrClass(code) == spoolClassUnexpected {
			t.Errorf("exit %d is published and classes as %q", code, spoolClassUnexpected)
		}
	}
}

// The spool is off unless it was asked for, in the same way as the MCP
// producers that share the directory.
func TestSpoolIsOffUnlessAskedFor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOOLFEEDBACK_DIR", dir)
	t.Setenv("UPKEEPER_FRICTION", "")
	t.Setenv("TOOLFEEDBACK", "")
	if spoolEnabled() {
		t.Fatal("the spool is on in a directory with no enabled marker")
	}
	runSpooled([]string{"hold", "--help"})
	if paths := spoolFiles(t, dir); len(paths) != 0 {
		t.Fatalf("the spool wrote %v while off", paths)
	}

	if err := os.WriteFile(filepath.Join(dir, "enabled"), nil, 0o600); err != nil {
		t.Fatalf("write the enabled marker: %v", err)
	}
	if !spoolEnabled() {
		t.Fatal("the enabled marker did not turn the spool on")
	}
	t.Setenv("TOOLFEEDBACK", "0")
	if spoolEnabled() {
		t.Fatal("TOOLFEEDBACK=0 did not turn the spool off")
	}
}
