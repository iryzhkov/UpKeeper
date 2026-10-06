package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func fenceContext(t *testing.T, payload Payload, running string) *Context {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	previous := programCommit
	programCommit = func() string { return running }
	t.Cleanup(func() { programCommit = previous })
	context, err := NewContext(payload, func(procx.Command) procx.Result {
		t.Error("the fence test ran a command")
		return procx.Result{Code: 1}
	})
	if err != nil {
		t.Fatal(err)
	}
	return context
}

// A controller that names the program a release was gated with is refused by
// any other one, before the lock is taken and before any component is touched.
func TestAnAgentRefusesAProgramItIsNot(t *testing.T) {
	payload := Payload{
		RunID: "20260914T090000Z-0a1b2c3d", Host: HostRef{Name: "homelab"},
		Components: []string{"huyang"}, ExpectedProgramCommit: strings.Repeat("b", 40),
	}
	context := fenceContext(t, payload, strings.Repeat("a", 40))
	result := Execute(context)
	if result["result"] != "failed" {
		t.Fatalf("result %v", result)
	}
	message, _ := result["error"].(string)
	if !strings.Contains(message, strings.Repeat("a", 40)) || !strings.Contains(message, strings.Repeat("b", 40)) {
		t.Errorf("the refusal does not name both programs: %q", message)
	}
	if components, _ := result["components"].([]any); len(components) != 0 {
		t.Errorf("a refused payload reached components: %v", components)
	}
	if _, err := os.Stat(filepath.Dir(context.lockDirectory)); err == nil {
		t.Error("a refused payload created the lock directory")
	}
}

// The same program passes the fence, and observation never needs it.
func TestTheFencePassesTheProgramItNamesAndIgnoresObservation(t *testing.T) {
	commit := strings.Repeat("b", 40)
	matching := fenceContext(t, Payload{
		RunID: "20260914T090000Z-0a1b2c3d", Host: HostRef{Name: "homelab"},
		ExpectedProgramCommit: commit,
	}, commit)
	if result := Execute(matching); result["result"] != "converged" {
		t.Errorf("the named program was refused: %v", result)
	}
	observing := fenceContext(t, Payload{
		RunID: "20260914T090000Z-0a1b2c3d", Host: HostRef{Name: "homelab"},
		ObserveOnly: true, ExpectedProgramCommit: commit,
	}, strings.Repeat("a", 40))
	observing.Runner = func(procx.Command) procx.Result { return procx.Result{Code: 1} }
	if result := Execute(observing); result["result"] == "failed" {
		t.Errorf("observation was fenced: %v", result)
	}
}

// The fence on the command line refuses a payload that carries none, before
// the lock, and a payload that names another program contradicts it.
func TestTheInvocationFenceRefusesAnotherProgram(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	previous := programCommit
	programCommit = func() string { return strings.Repeat("a", 40) }
	t.Cleanup(func() { programCommit = previous })
	unfenced := `{"run_id":"20260914T090000Z-0a1b2c3d","host":{"name":"homelab"},"components":[]}`
	var output bytes.Buffer
	if err := RunFenced(strings.NewReader(unfenced), &output, nil, strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"result": "failed"`) || !strings.Contains(output.String(), strings.Repeat("b", 40)) {
		t.Errorf("output %s", output.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".local/state/dev-fleet/locks")); err == nil {
		t.Error("a refused invocation took the lock")
	}
	contradicting := `{"run_id":"20260914T090000Z-0a1b2c3d","host":{"name":"homelab"},"expected_program_commit":"` +
		strings.Repeat("c", 40) + `"}`
	if err := RunFenced(strings.NewReader(contradicting), &output, nil, strings.Repeat("b", 40)); err == nil {
		t.Error("a payload that contradicts its invocation was accepted")
	}
}

// An agent from before the fence decodes a fenced payload without refusing
// it, which is why the fence is a new field rather than a changed one. The
// field alone does not stop such an agent: see the converge package's
// TestAPreFenceAgentDoesNoWorkForAFencedPull for the command-line fence.
func TestAFencedPayloadDecodes(t *testing.T) {
	payload, err := DecodePayload([]byte(`{"run_id":"r","expected_program_commit":"` +
		strings.Repeat("b", 40) + `","host":{"name":"homelab"}}`))
	if err != nil || payload.ExpectedProgramCommit != strings.Repeat("b", 40) {
		t.Fatalf("decoded %#v, %v", payload, err)
	}
}
