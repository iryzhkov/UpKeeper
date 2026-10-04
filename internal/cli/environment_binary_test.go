//go:build linux

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvironmentBinaryNoExternalCalls(t *testing.T) {
	// Build before removing PATH. The tested binary gets only trap executables.
	// Build first, before setting the inspection HOME and PATH.

	bin := filepath.Join(t.TempDir(), "upkeeper")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/upkeeper")
	build.Env = append(os.Environ(), "GOMAXPROCS=2", "GOFLAGS=-p=1")
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	argv, meta, home := environmentCandidate(t)
	trapDir := t.TempDir()
	marker := filepath.Join(trapDir, "called")
	for _, name := range []string{"git", "ssh", "curl", "wget", "python3", "uv", "systemctl", "t3-steward", "omarchy", "hostname", "uname"} {
		body := "#!/bin/sh\n: > '" + marker + "'\nexit 97\n"
		if e := os.WriteFile(filepath.Join(trapDir, name), []byte(body), 0755); e != nil {
			t.Fatal(e)
		}
	}
	spool := useSpool(t)
	t.Setenv("PATH", trapDir)
	cases := []struct {
		argv []string
		code int
	}{
		{argv, 0}, {append(append([]string{}, argv...), "--json"), 0},
		{[]string{"environment", "tools", "--help"}, 0}, {[]string{"environment", "--help"}, 0},
		{[]string{"environment", "unknown", "--help"}, 2},
		{[]string{"environment", "tools", "--metadata", meta, "--root", "/absent", "--expect-sha256", strings.Repeat("0", 64), "--json"}, 30},
	}
	for _, c := range cases {
		cmd := exec.Command(bin, c.argv...)
		var out, errOut bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errOut
		e := cmd.Run()
		code := 0
		if e != nil {
			exit, ok := e.(*exec.ExitError)
			if !ok {
				t.Fatal(e)
			}
			code = exit.ExitCode()
		}
		if code != c.code {
			t.Fatalf("%v: %d %s %s", c.argv, code, out.String(), errOut.String())
		}
		if _, e := os.Stat(marker); !os.IsNotExist(e) {
			t.Fatal("external command called", e)
		}
		for _, dir := range []string{home, spool} {
			entries, e := os.ReadDir(dir)
			if e != nil || len(entries) != 0 {
				t.Fatal("binary wrote state", dir, entries, e)
			}
		}
	}
}
