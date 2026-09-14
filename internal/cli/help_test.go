package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/cli"
)

// The parser this replaced answered -h and --help by printing help on stdout
// and exiting 0. A help flag that writes to stderr and exits 2 is
// indistinguishable from a typo, so both halves are pinned here.
func TestHelpPrintsOnStdoutAndSucceeds(t *testing.T) {
	for _, argv := range [][]string{
		{"-h"},
		{"--help"},
		{"doctor", "-h"},
		{"doctor", "--help"},
		{"status", "--help"},
		{"push", "--help"},
		{"pull", "--help"},
		{"rollback", "--help"},
		{"fleet-plan", "--help"},
		{"self-pull", "--help"},
		{"scheduled-pull", "--help"},
		{"install-self-timer", "--help"},
		{"install-pull-timer", "--help"},
		{"version", "--help"},
	} {
		var out, errs bytes.Buffer
		code := cli.Main("upkeeper", argv, cli.Streams{Out: &out, Err: &errs})
		if code != 0 {
			t.Errorf("%v: exit %d, want 0", argv, code)
		}
		if errs.Len() != 0 {
			t.Errorf("%v: wrote %q to stderr, want nothing", argv, errs.String())
		}
		if !strings.HasPrefix(out.String(), "usage: upkeeper") {
			t.Errorf("%v: stdout does not start with a usage line: %q", argv, out.String())
		}
		if !strings.Contains(out.String(), "-h, --help") {
			t.Errorf("%v: help does not document the help flag: %q", argv, out.String())
		}
	}
}

// A help flag asked for after other options is still a help request, which is
// how argparse behaved. Getting this wrong would converge a fleet because
// somebody appended --help to a command they were unsure about.
func TestHelpAfterOtherOptionsDoesNotRunTheCommand(t *testing.T) {
	var out, errs bytes.Buffer
	code := cli.Main("upkeeper", []string{"pull", "--all", "--help"}, cli.Streams{Out: &out, Err: &errs})
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr=%q", code, errs.String())
	}
	if !strings.Contains(out.String(), "usage: upkeeper pull") {
		t.Fatalf("stdout is not the pull help: %q", out.String())
	}
}

// The top-level help must list every subcommand the binary accepts, so that
// the added scheduled entry points are discoverable without reading the
// source.
func TestTopLevelHelpListsEveryCommand(t *testing.T) {
	var out, errs bytes.Buffer
	if code := cli.Main("upkeeper", []string{"--help"}, cli.Streams{Out: &out, Err: &errs}); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	for _, command := range cli.Commands {
		if !strings.Contains(out.String(), command) {
			t.Errorf("the top-level help does not mention %q", command)
		}
	}
}

// A genuinely malformed command line still exits 2 on stderr. The help path
// must not have swallowed that.
func TestMalformedCommandLineStillExitsTwo(t *testing.T) {
	for _, argv := range [][]string{
		{},
		{"nonsense"},
		{"doctor", "--bogus"},
		{"pull"},
	} {
		var out, errs bytes.Buffer
		code := cli.Main("upkeeper", argv, cli.Streams{Out: &out, Err: &errs})
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", argv, code)
		}
		if !strings.Contains(errs.String(), "error:") {
			t.Errorf("%v: stderr does not report an error: %q", argv, errs.String())
		}
	}
}
