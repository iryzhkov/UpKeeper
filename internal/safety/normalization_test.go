package safety_test

import (
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/pyfmt"
	"github.com/iryzhkov/UpKeeper/internal/safety"
)

func reprOf(value string) string { return pyfmt.Repr(value) }

// The Python validator built a PurePosixPath first, which drops "." components
// before any check runs, so an unnormalized path was refused as unnormalized
// and not as traversal. Both refuse either way; the message is the part a
// report and an operator's runbook read.
func TestUnnormalizedPathsAreRefusedByName(t *testing.T) {
	for _, value := range []string{
		".local/./bin",
		".local/.",
		"./.local/bin",
		".config/a/./b",
	} {
		_, err := safety.ValidateHomeRelativePath(value, nil)
		if err == nil {
			t.Errorf("%q was accepted", value)
			continue
		}
		want := "path is not normalized: " + reprOf(value)
		if err.Error() != want {
			t.Errorf("%q: %q, want %q", value, err.Error(), want)
		}
	}
}

// Real traversal still says so.
func TestTraversalKeepsItsOwnRefusal(t *testing.T) {
	for _, value := range []string{".local/../etc", ".local/bin/.."} {
		_, err := safety.ValidateHomeRelativePath(value, nil)
		if err == nil {
			t.Errorf("%q was accepted", value)
			continue
		}
		want := "path traversal is forbidden: " + reprOf(value)
		if err.Error() != want {
			t.Errorf("%q: %q, want %q", value, err.Error(), want)
		}
	}
}

// A path that is nothing but "." components made Python raise IndexError,
// which escaped as an unexpected failure. Refusing is the same answer without
// the traceback.
func TestAPathOfOnlyDotsIsRefusedCleanly(t *testing.T) {
	for _, value := range []string{".", "./."} {
		if _, err := safety.ValidateHomeRelativePath(value, nil); err == nil {
			t.Errorf("%q was accepted", value)
		}
	}
}

// Normalized paths below an allowed root are still accepted unchanged.
func TestNormalizedPathsStillPass(t *testing.T) {
	for _, value := range []string{".local/bin", ".config/agents/x.toml", ".local"} {
		if _, err := safety.ValidateHomeRelativePath(value, nil); err != nil {
			t.Errorf("%q was refused: %v", value, err)
		}
	}
}
