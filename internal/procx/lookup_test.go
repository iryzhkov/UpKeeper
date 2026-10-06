package procx

import (
	"path/filepath"
	"testing"
)

// The last PATH assignment wins, as it does for the child, and a command
// without an environment keeps resolving against the parent's PATH.
func TestLookupInFollowsTheLastPathAndFallsBackToTheParent(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeTool(t, second, "upkeeper-test-tool", "#!/bin/sh\n")
	env := []string{"PATH=" + first, "HOME=/nowhere", "PATH=" + second}
	if got := LookupIn("upkeeper-test-tool", env); got != filepath.Join(second, "upkeeper-test-tool") {
		t.Fatalf("LookupIn = %q", got)
	}
	if got := LookupIn("upkeeper-test-tool", []string{"PATH=" + first}); got != "" {
		t.Fatalf("LookupIn found %q outside the given PATH", got)
	}
	t.Setenv("PATH", second)
	if got := LookupIn("upkeeper-test-tool", []string{"HOME=/nowhere"}); got != filepath.Join(second, "upkeeper-test-tool") {
		t.Fatalf("LookupIn without PATH = %q", got)
	}
	if got := LookupIn("upkeeper-test-tool", nil); got != filepath.Join(second, "upkeeper-test-tool") {
		t.Fatalf("LookupIn with no environment = %q", got)
	}
}
