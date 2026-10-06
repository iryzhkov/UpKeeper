package procx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Like exec.LookPath, a file this process may not execute does not end the
// search, even when somebody else may run it.
func TestRunSkipsAProgramOnlySomebodyElseMayRun(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root may execute any file with an execute bit")
	}
	first, second := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "tool"), []byte("#!/bin/sh\necho first\n"), 0o610); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "tool"), []byte("#!/bin/sh\necho second\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := first + string(os.PathListSeparator) + second
	t.Setenv("PATH", path)
	if found, err := exec.LookPath("tool"); err != nil || found != filepath.Join(second, "tool") {
		t.Fatalf("exec.LookPath = %q, %v", found, err)
	}
	result := Run(Command{Argv: []string{"tool"}, Env: []string{"PATH=" + path}})
	if !result.OK() || result.Stdout != "second\n" {
		t.Fatalf("Run = %+v", result)
	}
}
