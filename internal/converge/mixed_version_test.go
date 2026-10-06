package converge_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/agent"
	"github.com/iryzhkov/UpKeeper/internal/cli"
	"github.com/iryzhkov/UpKeeper/internal/converge"
	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/transport"
)

// preFenceCommit is the last UpKeeper main commit before the program fence.
// Its agent decodes a payload with unknown fields ignored, so it would accept
// expected_program_commit and do the work anyway.
const preFenceCommit = "1ecf8482465d3fdede1466ce9c51ff2e4a9a78c4"

// buildPreFenceAgent builds the upkeeper binary of preFenceCommit from this
// repository's own history, offline. A clone without that commit, such as a
// shallow CI checkout, skips the test rather than fetch it.
func buildPreFenceAgent(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", root, "cat-file", "-e", preFenceCommit+"^{commit}").CombinedOutput(); err != nil {
		t.Skipf("this clone does not have the pre-fence commit %s: %s", preFenceCommit, output)
	}
	source := t.TempDir()
	archive := filepath.Join(t.TempDir(), "source.tar")
	for _, argv := range [][]string{
		{"git", "-C", root, "archive", "--format=tar", "-o", archive, preFenceCommit},
		{"tar", "-xf", archive, "-C", source},
	} {
		if output, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", argv, err, output)
		}
	}
	binary := filepath.Join(t.TempDir(), "upkeeper")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, "./cmd/upkeeper")
	build.Dir = source
	build.Env = append(os.Environ(), "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOWORK=off",
		"GOTOOLCHAIN=local", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the pre-fence agent: %v\n%s", err, output)
	}
	return binary
}

// remoteAs plays a target over SSH: the command after "--" runs with the
// given binary as the target's ~/.local/bin/upkeeper and home as its HOME.
func remoteAs(t *testing.T, binary, home string, invoked *[][]string) procx.Runner {
	return func(command procx.Command) procx.Result {
		separator := slices.Index(command.Argv, "--")
		if separator < 0 || len(command.Argv) < separator+2 || command.Argv[separator+1] != transport.RemoteBinary {
			t.Errorf("not an agent invocation: %v", command.Argv)
			return procx.Result{Code: 255}
		}
		remote := command.Argv[separator+2:]
		*invoked = append(*invoked, remote)
		process := exec.Command(binary, remote...)
		process.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		process.Stdin = strings.NewReader(command.Input)
		var stdout, stderr bytes.Buffer
		process.Stdout, process.Stderr = &stdout, &stderr
		err := process.Run()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			return procx.Result{Err: err}
		}
		return procx.Result{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
	}
}

// The controller's fenced invocation reaches this program's own agent,
// through its real command line, and that agent applies the fence before its
// lock: a test binary is built from no named commit, so it is not the program
// the release requires.
func TestAFencedPullReachesTheCurrentAgentsFence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	spec := inventory.HostSpec{Name: "homelab", User: "igor", Address: "homelab.invalid"}
	payload := agent.Payload{
		RunID: "20260914T090000Z-0a1b2c3d", Host: agent.HostRef{Name: "homelab"},
		Components: []string{}, ExpectedProgramCommit: strings.Repeat("b", 40),
	}
	invoked := [][]string{}
	runner := func(command procx.Command) procx.Result {
		remote := command.Argv[slices.Index(command.Argv, "--")+2:]
		invoked = append(invoked, remote)
		var stdout, stderr bytes.Buffer
		code := cli.Main("upkeeper", remote, cli.Streams{In: strings.NewReader(command.Input), Out: &stdout, Err: &stderr})
		return procx.Result{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
	}
	result := converge.RunHost(spec, payload, time.Minute, false, runner)
	if !reflect.DeepEqual(invoked, [][]string{{converge.AgentSubcommand, converge.AgentProgramFlag, strings.Repeat("b", 40)}}) {
		t.Fatalf("invoked %v", invoked)
	}
	if message, _ := result["error"].(string); result["result"] != "failed" ||
		!strings.Contains(message, "the release requires "+strings.Repeat("b", 40)) {
		t.Fatalf("the current agent did not apply the fence: %v", result)
	}
	if _, err := os.Stat(filepath.Join(home, ".local/state/dev-fleet/locks")); err == nil {
		t.Error("a refused agent took its host lock")
	}
	// Without a fence, as for a rollback, the invocation is what it always was.
	invoked = nil
	payload.ExpectedProgramCommit = ""
	converge.RunHost(spec, payload, time.Minute, false, runner)
	if !reflect.DeepEqual(invoked, [][]string{{converge.AgentSubcommand}}) {
		t.Errorf("an unfenced invocation %v", invoked)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// A controller that fences a pull on the program its release pins must not
// get work done by an agent from before the fence, which ignores the payload
// field. The pre-fence binary, built from history, takes its host lock for a
// fenced payload sent the old way; sent the way the controller sends it now,
// the same binary refuses the invocation before reading the payload, and the
// report says the host needs the pinned binary.
func TestAPreFenceAgentDoesNoWorkForAFencedPull(t *testing.T) {
	binary := buildPreFenceAgent(t)
	spec := inventory.HostSpec{Name: "homelab", User: "igor", Address: "homelab.invalid"}
	payload := agent.Payload{
		RunID: "20260914T090000Z-0a1b2c3d", Host: agent.HostRef{Name: "homelab"},
		Components: []string{}, ExpectedProgramCommit: strings.Repeat("b", 40),
	}
	locks := func(home string) string { return filepath.Join(home, ".local/state/dev-fleet/locks") }

	// The old invocation: the fence travels only in the payload, and the
	// pre-fence agent does the work regardless.
	home := t.TempDir()
	invoked := [][]string{}
	outcome := transport.Invoke(spec, converge.AgentSubcommand, mustJSON(t, payload), time.Minute,
		remoteAs(t, binary, home, &invoked))
	if outcome.Code != 0 || !strings.Contains(outcome.Stdout, `"result"`) {
		t.Fatalf("the pre-fence agent did not run a plain invocation: %d %s %s", outcome.Code, outcome.Stdout, outcome.Stderr)
	}
	if _, err := os.Stat(locks(home)); err != nil {
		t.Fatalf("the pre-fence agent took no lock for a fenced payload, so this test proves nothing: %v", err)
	}

	// The controller's invocation.
	home = t.TempDir()
	invoked = [][]string{}
	result := converge.RunHost(spec, payload, time.Minute, false, remoteAs(t, binary, home, &invoked))
	if result["result"] != "failed" {
		t.Fatalf("a pre-fence agent converged a fenced pull: %v", result)
	}
	if message, _ := result["error"].(string); !strings.Contains(message, "program fence") ||
		!strings.Contains(message, strings.Repeat("b", 40)) {
		t.Errorf("the refusal does not name the fence and the required program: %v", result)
	}
	if _, err := os.Stat(locks(home)); err == nil {
		t.Error("the pre-fence agent took its host lock for a fenced pull")
	}
	if len(invoked) != 1 || !slices.Contains(invoked[0], strings.Repeat("b", 40)) {
		t.Errorf("the fence is not part of the invocation: %v", invoked)
	}
}
