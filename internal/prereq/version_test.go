package prereq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// Debian 12's golang-go is Go 1.19, which cannot build a module that names a
// newer Go; 1.21 and later fetch the toolchain the module asks for.
func TestTheGoPrerequisiteNeedsAGoThatFetchesToolchains(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var goItem Prerequisite
	for _, item := range Catalogue["huyang"] {
		if item.Name == "go" {
			goItem = item
		}
	}
	version := ""
	runner := func(command procx.Command) procx.Result {
		if strings.Join(command.Argv, " ") != "go env GOVERSION" {
			t.Fatalf("ran %v", command.Argv)
		}
		return procx.Result{Stdout: version + "\n"}
	}
	env := []string{"PATH=" + bin}
	for reported, present := range map[string]bool{
		"go1.19.8": false, "go1.20": false, "go1.21.0": true, "go1.24.4": true, "go2.0": true, "": false,
	} {
		version = reported
		if got := goItem.Present(env, runner); got != present {
			t.Errorf("go reporting %q: present = %v, want %v", reported, got, present)
		}
	}
	if remedy := goItem.Remedy(Debian); !strings.Contains(remedy, "golang-go") || !strings.Contains(remedy, "1.21 or newer") {
		t.Errorf("Debian remedy = %q", remedy)
	}
}
