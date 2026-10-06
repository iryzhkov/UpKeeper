package cli

import (
	"reflect"
	"strings"
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

func TestAuthoredPushRequiresBothFencesAndRejectsComponents(t *testing.T) {
	head := strings.Repeat("a", 40)
	digest := strings.Repeat("b", 64)
	parsed := parseOrFail(t, "push", "--authored", "--expect-head", head, "--expect-release-sha256", digest)
	if !parsed.authored || parsed.expectHead != head || parsed.expectReleaseSHA256 != digest {
		t.Fatalf("authored flags = %#v", parsed)
	}
	for _, argv := range [][]string{
		{"push", "--authored", "--expect-head", head},
		{"push", "--expect-head", head, "--expect-release-sha256", digest},
		{"push", "--authored", "--expect-head", head, "--expect-release-sha256", digest, "--components", "t3"},
	} {
		if _, err := parse(argv, release.ComponentOrder); err == nil {
			t.Fatalf("accepted invalid authored invocation %v", argv)
		}
	}
}

func TestProjectUpdateTracksPresenceSeparatelyFromEmpty(t *testing.T) {
	digest := strings.Repeat("c", 64)
	parsed := parseOrFail(t, "project", "update", "upkeeper",
		"--expect-release-sha256", digest, "--setup-profile", "")
	if !parsed.projectSetupProfileSet || parsed.projectSetupProfile != "" {
		t.Fatalf("explicit empty field was lost: %#v", parsed)
	}
	if _, err := parse([]string{"project", "update", "upkeeper", "--expect-release-sha256", digest}, release.ComponentOrder); err == nil {
		t.Fatal("fieldless project update accepted")
	}
}

// Reconciling a retained T3 transaction names one transaction on one host and
// nothing else; every broader or ambiguous form is a usage error.
func TestReconcileT3TransactionIsNarrow(t *testing.T) {
	parsed := parseOrFail(t, "pull", "--hosts", "normandy", "--components", "t3", "--reconcile-t3-transaction", ".upkeeper-t3-3563961840", "--dry-run")
	if parsed.reconcileT3 != ".upkeeper-t3-3563961840" || !parsed.dryRun {
		t.Fatalf("reconciliation flag = %#v", parsed)
	}
	for _, argv := range [][]string{
		{"pull", "--all", "--components", "t3", "--reconcile-t3-transaction", ".upkeeper-t3-1"},
		{"pull", "--self", "--components", "t3", "--reconcile-t3-transaction", ".upkeeper-t3-1"},
		{"pull", "--hosts", "normandy", "homelab", "--components", "t3", "--reconcile-t3-transaction", ".upkeeper-t3-1"},
		{"pull", "--hosts", "normandy", "--reconcile-t3-transaction", ".upkeeper-t3-1"},
		{"pull", "--hosts", "normandy", "--components", "t3", "t3-steward", "--reconcile-t3-transaction", ".upkeeper-t3-1"},
		{"pull", "--hosts", "normandy", "--components", "t3", "--reconcile-t3-transaction", "../t3"},
		{"pull", "--hosts", "normandy", "--components", "t3", "--reconcile-t3-transaction", ".upkeeper-t3-1/x"},
		{"rollback", "--to", "abcdef1", "--hosts", "normandy", "--components", "t3", "--reconcile-t3-transaction", ".upkeeper-t3-1"},
	} {
		if _, err := parse(argv, release.ComponentOrder); err == nil {
			t.Fatalf("accepted %v", argv)
		}
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
				`"omarchy-setup", "agent-config", "agent-tooling", "jocasta", "codex-controlled-runtime", "agent-environment", "huyang", ` +
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
				`"status", "push", "pull", "rollback", "fleet-plan", "project", "provider", "author", "self-pull", ` +
				`"scheduled-pull", "install-self-timer", "install-pull-timer", "hold", "self", "version", "environment")`,
		},
		{
			"author without its pins",
			[]string{"author", "--dry-run"},
			"the following arguments are required: --steward, --omarchy-setup",
		},
		{
			"author with a flag it does not take",
			[]string{"author", "--steward", "0.11.0", "--omarchy-setup", "abcdef1", "--components", "t3"},
			"unrecognized arguments: --components",
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
