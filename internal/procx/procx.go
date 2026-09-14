// Package procx runs external programs with a bound on how long they may take.
//
// UpKeeper never builds a shell string: every program is executed from an
// argument vector, so a value that reached a manifest can never become a shell
// fragment. Every call carries a timeout, because a convergence that hangs on a
// single unreachable host is worse than one that fails.
package procx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Command describes one bounded execution.
type Command struct {
	Argv []string
	// Dir is the working directory, or "" for the current one.
	Dir string
	// Input is written to standard input and the pipe is then closed.
	Input string
	// Env replaces the environment entirely when non-nil.
	Env []string
	// Timeout bounds the whole execution. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Result is what a bounded execution produced.
type Result struct {
	Code     int
	Stdout   string
	Stderr   string
	TimedOut bool
	// Err is set when the program could not be started or was killed by the
	// timeout. A non-zero exit status alone is not an error.
	Err error
}

// OK reports a clean exit.
func (r Result) OK() bool { return r.Err == nil && r.Code == 0 }

// Output returns standard output, or standard error when nothing was written to
// standard output, trimmed. It matches how the probe summarised a command.
func (r Result) Output() string {
	if strings.TrimSpace(r.Stdout) != "" {
		return strings.TrimSpace(r.Stdout)
	}
	return strings.TrimSpace(r.Stderr)
}

// FirstLine returns the first line of Output, or "" when there is none.
func (r Result) FirstLine() string {
	output := r.Output()
	if output == "" {
		return ""
	}
	if index := strings.IndexByte(output, '\n'); index >= 0 {
		return output[:index]
	}
	return output
}

// DefaultTimeout applies to a command that does not choose its own bound.
const DefaultTimeout = 30 * time.Second

// Runner executes a command. Tests substitute their own.
type Runner func(Command) Result

// Run is the production runner.
func Run(command Command) Result {
	if len(command.Argv) == 0 {
		return Result{Code: -1, Err: errors.New("no program to run")}
	}
	timeout := command.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	process := exec.CommandContext(ctx, command.Argv[0], command.Argv[1:]...)
	process.Dir = command.Dir
	process.Env = command.Env
	if command.Input != "" {
		process.Stdin = strings.NewReader(command.Input)
	}
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr

	err := process.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.Code = -1
		result.TimedOut = true
		result.Err = &TimeoutError{Argv: command.Argv, Timeout: timeout}
	case err == nil:
		result.Code = process.ProcessState.ExitCode()
	default:
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			result.Code = exitError.ExitCode()
		} else {
			result.Code = -1
			result.Err = err
		}
	}
	return result
}

// TimeoutError reports a program that outlived its bound.
type TimeoutError struct {
	Argv    []string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return e.Argv[0] + " exceeded its " + e.Timeout.String() + " timeout"
}

// Expected marks a timeout as a refusal the CLI may report.
func (e *TimeoutError) Expected() bool { return true }

// Lookup reports the absolute path of an executable on PATH, or "" when it is
// not installed. It is the Go equivalent of shutil.which.
func Lookup(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

// GitEnvironment returns an environment in which Git may never prompt and SSH
// may never accept an unknown host key.
func GitEnvironment() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=yes",
	)
}
