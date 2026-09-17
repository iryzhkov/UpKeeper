package cli

import (
	"reflect"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/release"
)

func parseOrFail(t *testing.T, argv ...string) *arguments {
	t.Helper()
	parsed, err := parse(argv, release.ComponentOrder)
	if err != nil {
		t.Fatalf("parse(%v): %v", argv, err)
	}
	return parsed
}

func TestRepeatedValuesAfterOneFlag(t *testing.T) {
	// argparse's nargs="+" is the shape Go's flag package cannot express, and
	// operators have been typing it since the first release.
	parsed := parseOrFail(t, "pull", "--hosts", "normandy", "homelab", "--dry-run")
	if !reflect.DeepEqual(parsed.hosts, []string{"normandy", "homelab"}) {
		t.Errorf("hosts are %v", parsed.hosts)
	}
	if !parsed.dryRun {
		t.Error("the flag after the repeated values was lost")
	}
}

func TestPushTakesItsRemotesPositionally(t *testing.T) {
	parsed := parseOrFail(t, "push", "origin", "forgejo", "--no-push", "--json")
	if !reflect.DeepEqual(parsed.remotes, []string{"origin", "forgejo"}) {
		t.Errorf("remotes are %v", parsed.remotes)
	}
	if !parsed.noPush || !parsed.asJSON {
		t.Errorf("flags were lost: %#v", parsed)
	}
}

func TestDefaults(t *testing.T) {
	if parsed := parseOrFail(t, "doctor"); parsed.probeTimeout != 20 {
		t.Errorf("doctor timeout defaults to %d", parsed.probeTimeout)
	}
	if parsed := parseOrFail(t, "pull", "--all"); parsed.pullTimeout != 1800 {
		t.Errorf("pull timeout defaults to %d", parsed.pullTimeout)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		expected string
	}{
		{
			"a probe timeout outside the permitted range",
			[]string{"doctor", "--timeout", "120"},
			"argument --timeout: invalid choice: 120 (choose from 5 through 60)",
		},
		{
			"a probe timeout that is not a number",
			[]string{"status", "--timeout", "soon"},
			`argument --timeout: invalid int value: "soon"`,
		},
		{
			"no host selection at all",
			[]string{"pull"},
			"one of the arguments --hosts --all --self is required",
		},
		{
			"two host selections at once",
			[]string{"pull", "--all", "--self"},
			"argument --self: not allowed with argument --all",
		},
		{
			"a rollback without a target release",
			[]string{"rollback", "--all"},
			"the following arguments are required: --to",
		},
		{
			// A rollback converges named hosts; there is no self rollback,
			// because a host that rolled itself back could not be observed.
			"a self rollback",
			[]string{"rollback", "--to", "abcdef1", "--self"},
			"unrecognized arguments: --self",
		},
		{
			"an unknown component",
			[]string{"pull", "--all", "--components", "nonsense"},
			`argument --components: invalid choice: "nonsense" (choose from ` +
				`"omarchy-setup", "agent-config", "agent-tooling", "jocasta", "agent-environment", "huyang", ` +
				`"t3-steward", "t3", "worker-configuration", "steward-fleet-configuration")`,
		},
		{
			"an unknown flag",
			[]string{"doctor", "--verbose"},
			"unrecognized arguments: --verbose",
		},
		{
			"an unknown command",
			[]string{"converge"},
			`argument command: invalid choice: "converge" (choose from "doctor", ` +
				`"status", "push", "pull", "rollback", "fleet-plan", "self-pull", "scheduled-pull", ` +
				`"install-self-timer", "install-pull-timer", "hold", "version")`,
		},
		{
			"no command at all",
			nil,
			"the following arguments are required: command",
		},
		{
			"a flag that wants a value and did not get one",
			[]string{"doctor", "--limit"},
			"argument --limit: expected one argument",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parse(testCase.argv, release.ComponentOrder)
			if err == nil {
				t.Fatal("expected a usage error")
			}
			if _, ok := err.(*usageError); !ok {
				t.Fatalf("error is %T, expected a usage error", err)
			}
			if err.Error() != testCase.expected {
				t.Errorf("produced %q, expected %q", err.Error(), testCase.expected)
			}
		})
	}
}
