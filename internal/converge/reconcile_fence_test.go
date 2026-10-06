package converge_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/cli"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// `pull --reconcile-t3-transaction NAME --components t3` dispatches exactly the
// t3 component. The T3 pair expansion does not apply: the agent refuses a
// reconciliation that selects anything but t3 alone, so expanding it to
// t3-steward made every reconciliation fail.
func TestAReconciliationDispatchesExactlyTheT3Component(t *testing.T) {
	choices := options(t, []string{"normandy"})
	choices.Components = []string{"t3"}
	choices.ReconcileT3 = ".upkeeper-t3-1"
	var seen []agent.Payload
	choices.Runner = stateRunner("converged", &seen)
	if _, _, err := converge.Fleet(fleetConfig(t), unpinnedRelease(t), choices); err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("the reconciliation was dispatched %d times", len(seen))
	}
	if !slices.Equal(seen[0].Components, []string{"t3"}) ||
		seen[0].ReconcileT3Transaction != ".upkeeper-t3-1" {
		t.Fatalf("the reconciliation dispatched components %v for transaction %q",
			seen[0].Components, seen[0].ReconcileT3Transaction)
	}
}

// A reconciliation names one transaction of one component; any other
// selection is refused before anything is dispatched.
func TestAReconciliationRefusesAnyOtherSelection(t *testing.T) {
	for _, components := range [][]string{nil, {"t3-steward"}, {"t3", "t3-steward"}} {
		choices := options(t, []string{"normandy"})
		choices.Components = components
		choices.ReconcileT3 = ".upkeeper-t3-1"
		var seen []agent.Payload
		choices.Runner = stateRunner("converged", &seen)
		_, _, err := converge.Fleet(fleetConfig(t), unpinnedRelease(t), choices)
		if err == nil || !strings.Contains(err.Error(), "--reconcile-t3-transaction") {
			t.Errorf("components %v: want a reconciliation refusal, got %v", components, err)
		}
		if len(seen) != 0 {
			t.Errorf("components %v: a refused reconciliation was dispatched", components)
		}
	}
}

// An explicit `pull --reconcile-t3-transaction` under a release that pins the
// program carries the fence like any other pull, and the target's agent,
// reached through its real command line, refuses it before its target lock and
// before the retained transaction's journal is read or written.
func TestAFencedReconciliationReachesTheAgentsFenceBeforeAnyWork(t *testing.T) {
	value := pinnedRelease(t)
	choices := options(t, []string{"normandy"})
	choices.Components = []string{"t3"}
	choices.ReconcileT3 = ".upkeeper-t3-1"
	choices.ExpectedProgramCommit = converge.ProgramFence(value, "pull")
	var seen []agent.Payload
	choices.Runner = stateRunner("converged", &seen)
	if _, _, err := converge.Fleet(fleetConfig(t), value, choices); err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(seen) != 1 || seen[0].ReconcileT3Transaction != ".upkeeper-t3-1" ||
		seen[0].ExpectedProgramCommit != strings.Repeat("b", 40) {
		t.Fatalf("the reconcile payload does not carry both the transaction and the fence: %#v", seen)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	journal := filepath.Join(home, ".npm-global/lib/node_modules/.upkeeper-t3-1/transaction.json")
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"phase":"files-restored"}` + "\n")
	if err := os.WriteFile(journal, body, 0o600); err != nil {
		t.Fatal(err)
	}
	invoked := [][]string{}
	runner := func(command procx.Command) procx.Result {
		remote := command.Argv[slices.Index(command.Argv, "--")+2:]
		invoked = append(invoked, remote)
		var stdout, stderr bytes.Buffer
		code := cli.Main("upkeeper", remote, cli.Streams{In: strings.NewReader(command.Input), Out: &stdout, Err: &stderr})
		return procx.Result{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
	}
	spec := inventory.HostSpec{Name: "normandy", User: "igor", Address: "normandy.invalid"}
	result := converge.RunHost(spec, seen[0], time.Minute, false, runner)
	if !reflect.DeepEqual(invoked, [][]string{{converge.AgentSubcommand, converge.AgentProgramFlag, strings.Repeat("b", 40)}}) {
		t.Fatalf("invoked %v", invoked)
	}
	if message, _ := result["error"].(string); result["result"] != "failed" ||
		!strings.Contains(message, "the release requires "+strings.Repeat("b", 40)) {
		t.Fatalf("the agent did not apply the fence to the reconciliation: %v", result)
	}
	if _, considered := result["legacy_updater"]; considered {
		t.Errorf("a refused reconciliation considered the legacy updater: %v", result)
	}
	if _, err := os.Stat(filepath.Join(home, ".local/state/dev-fleet/locks")); err == nil {
		t.Error("a refused reconciliation took its host lock")
	}
	if after, err := os.ReadFile(journal); err != nil || !bytes.Equal(after, body) {
		t.Errorf("the retained journal changed: %q, %v", after, err)
	}
}
