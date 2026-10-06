package agent

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// seedRetainedJournal writes a stand-in retained T3 transaction journal under
// home and returns its path and bytes, so a test can prove a refused run left
// it exactly as it was.
func seedRetainedJournal(t *testing.T, home, name string) (string, []byte) {
	t.Helper()
	dir := filepath.Join(home, ".npm-global/lib/node_modules", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "transaction.json")
	body := []byte(`{"phase":"files-restored"}` + "\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, body
}

// treeOf lists every path under root, so a test can prove a refused run
// created, renamed or removed nothing.
func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func reconcilePayload(fence string) Payload {
	return Payload{
		RunID: "20260914T090000Z-0a1b2c3d", Host: HostRef{Name: "homelab"},
		Components: []string{"t3"}, RetireUpdater: true,
		ReconcileT3Transaction: ".upkeeper-t3-1", ExpectedProgramCommit: fence,
	}
}

// The explicit T3 reconciliation is a journal writer, so the program fence
// covers it like any other run: a reconcile payload for another program is
// refused before the target lock, before any observation, before the legacy
// updater is considered and before the retained journal is read or published.
func TestAFencedReconciliationIsRefusedBeforeTheLockAndTheJournal(t *testing.T) {
	context := fenceContext(t, reconcilePayload(strings.Repeat("b", 40)), strings.Repeat("a", 40))
	home := os.Getenv("HOME")
	journal, body := seedRetainedJournal(t, home, ".upkeeper-t3-1")
	before := treeOf(t, home)
	result := Execute(context)
	if result["result"] != "failed" {
		t.Fatalf("result %v", result)
	}
	if message, _ := result["error"].(string); !strings.Contains(message, "the release requires "+strings.Repeat("b", 40)) {
		t.Errorf("the refusal is not the program fence: %q", message)
	}
	if components, _ := result["components"].([]any); len(components) != 0 {
		t.Errorf("a refused reconciliation reached components: %v", components)
	}
	if _, considered := result["legacy_updater"]; considered {
		t.Errorf("a refused reconciliation considered the legacy updater: %v", result)
	}
	if context.lockHeld {
		t.Error("a refused reconciliation holds the target lock")
	}
	if _, err := os.Stat(filepath.Dir(context.lockDirectory)); err == nil {
		t.Error("a refused reconciliation created the lock directory")
	}
	if after, err := os.ReadFile(journal); err != nil || !bytes.Equal(after, body) {
		t.Errorf("the retained journal changed: %q, %v", after, err)
	}
	if after := treeOf(t, home); !slices.Equal(after, before) {
		t.Errorf("a refused reconciliation changed the home tree:\nbefore %v\nafter  %v", before, after)
	}
}

// The invocation fence, `__agent --expect-program COMMIT`, refuses a reconcile
// payload that carries no fence of its own, with the same guarantees.
func TestTheInvocationFenceRefusesAReconciliationBeforeTheLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	previous := programCommit
	programCommit = func() string { return strings.Repeat("a", 40) }
	t.Cleanup(func() { programCommit = previous })
	journal, body := seedRetainedJournal(t, home, ".upkeeper-t3-1")
	before := treeOf(t, home)
	unfenced := `{"run_id":"20260914T090000Z-0a1b2c3d","host":{"name":"homelab"},` +
		`"components":["t3"],"retire_updater":true,"reconcile_t3_transaction":".upkeeper-t3-1"}`
	var output bytes.Buffer
	runner := func(command procx.Command) procx.Result {
		t.Errorf("a fenced reconciliation ran %v", command.Argv)
		return procx.Result{Code: 1}
	}
	if err := RunFenced(strings.NewReader(unfenced), &output, runner, strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"result": "failed"`) || !strings.Contains(output.String(), strings.Repeat("b", 40)) {
		t.Errorf("output %s", output.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".local/state/dev-fleet/locks")); err == nil {
		t.Error("a refused reconciliation took the lock")
	}
	if after, err := os.ReadFile(journal); err != nil || !bytes.Equal(after, body) {
		t.Errorf("the retained journal changed: %q, %v", after, err)
	}
	if after := treeOf(t, home); !slices.Equal(after, before) {
		t.Errorf("a refused reconciliation changed the home tree:\nbefore %v\nafter  %v", before, after)
	}
}

// The named program passes the fence and the reconciliation still runs under
// the target lock, without the legacy updater retirement: the fence adds a
// refusal in front of the T3 repair's guarantees and removes none of them.
func TestTheNamedProgramReconcilesUnderTheLock(t *testing.T) {
	commit := strings.Repeat("b", 40)
	context := fenceContext(t, reconcilePayload(commit), commit)
	var ran [][]string
	context.Runner = func(command procx.Command) procx.Result {
		ran = append(ran, command.Argv)
		return procx.Result{Code: 1}
	}
	result := Execute(context)
	if _, refused := result["program"]; refused {
		t.Fatalf("the named program was refused: %v", result)
	}
	if _, considered := result["legacy_updater"]; considered {
		t.Errorf("reconciliation performed the legacy updater retirement: %v", result)
	}
	for _, argv := range ran {
		if strings.Contains(strings.Join(argv, " "), "t3-update") {
			t.Errorf("reconciliation queried the legacy updater: %v", argv)
		}
	}
	components, _ := result["components"].([]any)
	if len(components) != 1 {
		t.Fatalf("the reconciliation did not reach the t3 component: %v", result)
	}
	entry, _ := components[0].(map[string]any)
	if message, _ := entry["error"].(string); strings.Contains(message, "target lock is not held") {
		t.Errorf("the reconciliation ran without the target lock: %v", entry)
	}
	if context.lockHeld {
		t.Error("the target lock is still marked held after the run")
	}
}
