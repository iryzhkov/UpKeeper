package procx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTool puts an executable shell script named name in directory.
func writeTool(t *testing.T, directory, name, body string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A non-interactive SSH session on Debian starts the agent with the system PATH
// only: ~/.bashrc returns before any PATH it sets. The agent hands every
// command an environment whose PATH names the mise shims, and the program has
// to be found there rather than on the PATH the agent itself inherited.
func TestRunResolvesTheProgramOnTheCommandsOwnPath(t *testing.T) {
	tools := t.TempDir()
	writeTool(t, tools, "upkeeper-test-npm", "#!/bin/sh\necho npm from the shims\n")
	t.Setenv("PATH", "/usr/bin:/bin")
	result := Run(Command{
		Argv:    []string{"upkeeper-test-npm", "ci"},
		Env:     []string{"PATH=" + tools},
		Timeout: 10 * time.Second,
	})
	if !result.OK() || strings.TrimSpace(result.Stdout) != "npm from the shims" {
		t.Fatalf("result = %+v", result)
	}
}

// The command's PATH is the one its program will run with, so a program that
// only the parent can see is not one the command has.
func TestRunDoesNotBorrowTheParentPath(t *testing.T) {
	parent := t.TempDir()
	writeTool(t, parent, "upkeeper-test-only-parent", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", parent+":/usr/bin:/bin")
	result := Run(Command{
		Argv:    []string{"upkeeper-test-only-parent"},
		Env:     []string{"PATH=" + t.TempDir()},
		Timeout: 10 * time.Second,
	})
	if result.Err == nil || !strings.Contains(result.Err.Error(), "executable file not found") {
		t.Fatalf("result = %+v", result)
	}
}
