// Package transport decides how a controller reaches a host, and reaches it.
//
// There are exactly two transports. When the inventory host is this machine,
// identified by the SHA-256 of /etc/machine-id, the work runs in this process:
// no subprocess, no network, no interpreter. Every other host is reached over
// SSH with host-key checking on, batch mode on and a bounded connect timeout,
// from an argument vector that no value from a manifest can escape.
//
// The Python implementation streamed a Python program to "python3 -" on the
// target. The Go implementation invokes the target's own upkeeper binary
// instead, which is what removes Python from the production path; a host that
// does not have the binary yet is diagnosed before anything is mutated.
package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/inventory"
	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// RemoteBinary is where a target's own upkeeper binary is expected, relative to
// the target user's home directory. SSH starts in the home directory, so the
// relative spelling is what reaches the remote shell.
const RemoteBinary = ".local/bin/upkeeper"

// MissingBinaryDiagnosis is reported when a target has no upkeeper binary. It
// names the remedy, because a host that silently does nothing is worse than one
// that refuses loudly.
const MissingBinaryDiagnosis = "target has no upkeeper binary at ~/" + RemoteBinary +
	"; install the released binary on the host before converging it"

// machineIDPath is read to decide whether an inventory host is this machine.
var machineIDPath = "/etc/machine-id"

// LocalMachineIDSHA256 returns this machine's durable identity, or "" when it
// cannot be read. An unreadable identity never matches any host.
func LocalMachineIDSHA256() string {
	data, err := os.ReadFile(machineIDPath)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// IsLocal reports whether an inventory host is the machine running this code.
func IsLocal(spec inventory.HostSpec) bool {
	identity := LocalMachineIDSHA256()
	return identity != "" && identity == spec.MachineIDSHA256
}

// Outcome is one remote or local execution.
type Outcome struct {
	Stdout    string
	Stderr    string
	Code      int
	Transport string
	TimedOut  bool
	Err       error
}

// MissingBinary reports whether the outcome is a target without the binary.
// A shell reports 127 for a command it cannot find.
func (o Outcome) MissingBinary() bool {
	if o.Code != 127 {
		return false
	}
	return strings.Contains(o.Stderr, RemoteBinary) ||
		strings.Contains(o.Stderr, "not found") ||
		strings.Contains(o.Stderr, "No such file or directory")
}

// SSHArguments builds the fixed, strict argument vector for a host.
//
// Host-key checking and batch mode are not configurable: a fleet that accepts
// an unknown host key has no identity guarantee left, and a prompt in a timer
// unit is a hang.
func SSHArguments(
	spec inventory.HostSpec, connectTimeout time.Duration, remote ...string,
) []string {
	seconds := int(connectTimeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return append([]string{
		"ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ConnectTimeout=" + strconv.Itoa(seconds),
		spec.User + "@" + spec.Address,
		"--",
	}, remote...)
}

// Invoke runs the target's upkeeper binary over SSH with input on its standard
// input, and returns exactly what it produced.
func Invoke(
	spec inventory.HostSpec, subcommand string, input string,
	timeout time.Duration, runner procx.Runner,
) Outcome {
	return InvokeCommand(spec, []string{subcommand}, input, timeout, runner)
}

// InvokeCommand is Invoke for a subcommand with arguments. The arguments
// reach the remote shell as words, so each must be free of shell syntax.
func InvokeCommand(
	spec inventory.HostSpec, command []string, input string,
	timeout time.Duration, runner procx.Runner,
) Outcome {
	if runner == nil {
		runner = procx.Run
	}
	connect := 10 * time.Second
	if timeout < connect {
		connect = timeout
	}
	argv := SSHArguments(spec, connect, append([]string{RemoteBinary}, command...)...)
	result := runner(procx.Command{Argv: argv, Input: input, Timeout: timeout})
	return Outcome{
		Stdout:    result.Stdout,
		Stderr:    result.Stderr,
		Code:      result.Code,
		Transport: "ssh",
		TimedOut:  result.TimedOut,
		Err:       result.Err,
	}
}

// OfflineMessages are the SSH failures that mean "the host is not on the
// network right now" rather than "the host refused us". Only these may be
// skipped quietly; an authentication failure always fails the run.
var OfflineMessages = []string{
	"Connection timed out",
	"No route to host",
	"Network is unreachable",
	"Connection refused",
	"Could not resolve hostname",
}

// Reachability is the result of a pre-dispatch connection check.
type Reachability struct {
	// Reachable is true when the host answered and dispatch may proceed.
	Reachable bool
	// Offline distinguishes a host that is merely absent from one that
	// actively refused the controller.
	Offline bool
	// Failed marks a check that could not be performed at all.
	Failed bool
	Stderr string
}

// Check probes a host before dispatch so that an offline machine costs one
// bounded connection rather than a whole deployment timeout.
func Check(spec inventory.HostSpec, runner procx.Runner) Reachability {
	if runner == nil {
		runner = procx.Run
	}
	argv := SSHArguments(spec, 10*time.Second, "true")
	result := runner(procx.Command{Argv: argv, Timeout: 15 * time.Second})
	if result.TimedOut {
		return Reachability{Offline: true}
	}
	if result.Err != nil {
		return Reachability{Failed: true}
	}
	if result.Code == 0 {
		return Reachability{Reachable: true}
	}
	offline := result.Code == 255 && containsAny(result.Stderr, OfflineMessages)
	return Reachability{Offline: offline, Stderr: tail(result.Stderr, 2000)}
}

func containsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

// Tail returns the last size bytes of a value, which is how every report bounds
// captured output.
func Tail(value string, size int) string { return tail(value, size) }

func tail(value string, size int) string {
	if len(value) <= size {
		return value
	}
	return value[len(value)-size:]
}
