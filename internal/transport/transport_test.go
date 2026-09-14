package transport_test

import (
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/transport"
)

func spec() inventory.HostSpec {
	return inventory.HostSpec{
		Name: "omarchy-pc", User: "igor", Address: "192.168.90.212",
		MachineIDSHA256: strings.Repeat("a", 64),
	}
}

// Host-key checking and batch mode are not configurable. A fleet that accepts
// an unknown host key has no identity guarantee left, and a prompt inside a
// timer unit is a hang nobody is there to answer.
func TestTheSSHVectorIsAlwaysStrict(t *testing.T) {
	argv := transport.SSHArguments(spec(), 10*time.Second, transport.RemoteBinary, "__agent")
	joined := strings.Join(argv, " ")
	for _, required := range []string{
		"-o BatchMode=yes",
		"-o StrictHostKeyChecking=yes",
		"-o ConnectTimeout=10",
		"igor@192.168.90.212",
		"-- .local/bin/upkeeper __agent",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("the vector is missing %q: %s", required, joined)
		}
	}
	// The whole point of the migration: no interpreter is invoked on a target.
	for _, forbidden := range []string{"python", "uv", "ansible", "sh -c", "bash -c"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("the vector mentions %q: %s", forbidden, joined)
		}
	}
}

func TestTheConnectTimeoutNeverExceedsTheWholeBudget(t *testing.T) {
	var seen []string
	runner := func(command procx.Command) procx.Result {
		seen = command.Argv
		return procx.Result{}
	}
	transport.Invoke(spec(), "__agent", "{}", 3*time.Second, runner)
	if !strings.Contains(strings.Join(seen, " "), "ConnectTimeout=3") {
		t.Errorf("the connect timeout outlived the run's budget: %v", seen)
	}
}

func TestAMissingRemoteBinaryIsRecognised(t *testing.T) {
	cases := []struct {
		name      string
		outcome   transport.Outcome
		recognise bool
	}{
		{
			"a shell that could not find the binary",
			transport.Outcome{Code: 127, Stderr: "bash: .local/bin/upkeeper: No such file or directory"},
			true,
		},
		{
			"a command-not-found message",
			transport.Outcome{Code: 127, Stderr: "upkeeper: command not found"},
			true,
		},
		{
			"an ordinary non-zero exit",
			transport.Outcome{Code: 1, Stderr: "boom"},
			false,
		},
		{
			"a program that exited 127 by itself",
			transport.Outcome{Code: 127, Stderr: "some unrelated failure"},
			false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.outcome.MissingBinary() != testCase.recognise {
				t.Errorf("produced %v", testCase.outcome.MissingBinary())
			}
		})
	}
}

func TestReachabilityDistinguishesAbsenceFromRefusal(t *testing.T) {
	cases := []struct {
		name     string
		result   procx.Result
		expected transport.Reachability
	}{
		{"the host answered", procx.Result{}, transport.Reachability{Reachable: true}},
		{
			"the host is not on the network",
			procx.Result{Code: 255, Stderr: "ssh: connect: No route to host"},
			transport.Reachability{Offline: true, Stderr: "ssh: connect: No route to host"},
		},
		{
			// A key the fleet no longer trusts is a change in its security
			// posture, never something to skip quietly.
			"the host refused us",
			procx.Result{Code: 255, Stderr: "Permission denied (publickey)"},
			transport.Reachability{Stderr: "Permission denied (publickey)"},
		},
		{
			"the connection timed out",
			procx.Result{TimedOut: true, Code: -1},
			transport.Reachability{Offline: true},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			produced := transport.Check(spec(), func(procx.Command) procx.Result {
				return testCase.result
			})
			if produced != testCase.expected {
				t.Errorf("produced %#v, expected %#v", produced, testCase.expected)
			}
		})
	}
}

func TestAnUnreadableMachineIdentityMatchesNoHost(t *testing.T) {
	// A host whose identity cannot be read must never be mistaken for this
	// machine, or a controller would converge itself instead of the target.
	empty := inventory.HostSpec{MachineIDSHA256: ""}
	if transport.IsLocal(empty) {
		t.Error("a host with no recorded identity was treated as local")
	}
}
