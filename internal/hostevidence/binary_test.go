package hostevidence_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/buildinfo"
	"github.com/iryzhkov/UpKeeper/internal/hostevidence"
)

// A host says which program answered and which generation its binary path
// runs, so that a controller can tell an old binary from a stale release.
func TestTheEvidenceNamesTheRunningBinary(t *testing.T) {
	home := t.TempDir()
	binary, _ := hostevidence.Collect(home, nil, time.Now())["binary"].(map[string]any)
	if binary["commit"] != buildinfo.SourceCommit() || binary["version"] != buildinfo.Version {
		t.Errorf("binary %v", binary)
	}
	if binary["generation"] != nil {
		t.Errorf("an unmanaged home reports generation %v", binary["generation"])
	}

	generation := strings.Repeat("1", 64)
	lib := filepath.Join(home, ".local/lib/upkeeper")
	if err := os.MkdirAll(filepath.Join(lib, "generations", generation), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("generations/"+generation, filepath.Join(lib, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".local/bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../lib/upkeeper/current/upkeeper", filepath.Join(home, ".local/bin/upkeeper")); err != nil {
		t.Fatal(err)
	}
	binary, _ = hostevidence.Collect(home, nil, time.Now())["binary"].(map[string]any)
	if binary["generation"] != generation {
		t.Errorf("a managed home reports generation %v", binary["generation"])
	}
}
