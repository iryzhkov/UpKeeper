//go:build linux

package author

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func baselineTestTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

// Git inherits the caller's umask. Fixture custody must be explicit even on
// worker hosts with a group-writable umask; deliberate unsafe-mode mutations
// happen after this construction step.
func baselineTestPrivateDirectories(t *testing.T, root string) {
	t.Helper()
	// root is a child of the fixture-owned t.TempDir, which Go creates with
	// the caller's umask as well.
	if err := os.Chmod(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0700); err != nil {
				return err
			}
			fd, err := sharedHeld(path, true)
			if err != nil {
				return fmt.Errorf("private fixture directory %s: %w", path, err)
			}
			syscall.Close(fd)
		} else {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				return os.Chmod(path, info.Mode().Perm()&^0022)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// baselineTestQualifiedVersion fixtures only the version qualification decision,
// never the executable or object reads. /usr/bin/git must genuinely accept
// --no-lazy-fetch; the local-promisor regression exercises its no-fetch semantics.
func baselineTestQualifiedVersion(actual []byte) error {
	if len(actual) == 0 {
		return errors.New("missing actual Git version")
	}
	return baselineSupportedGitVersion([]byte("git version 2.48.0\n"))
}

func baselineTestAcquireOriginal(ctx context.Context, root string) (*CatalogOriginalImportSnapshot, error) {
	return acquireCatalogOriginalImport(ctx, root, &baselineOriginalOperation{versionCheck: baselineTestQualifiedVersion})
}

func TestCatalogBaselineProductionGitVersionFloor(t *testing.T) {
	for _, version := range []string{"git version 2.47.3", "git version 1.99.0", "git version 3.0.0", "garbage"} {
		if err := baselineSupportedGitVersion([]byte(version)); err == nil {
			t.Fatalf("unsupported production Git accepted: %q", version)
		}
	}
	for _, version := range []string{"git version 2.48.0", "git version 2.55.1"} {
		if err := baselineSupportedGitVersion([]byte(version)); err != nil {
			t.Fatalf("qualified production Git refused: %q: %v", version, err)
		}
	}
	op := &baselineOriginalOperation{}
	version, err := op.git(context.Background(), t.TempDir(), "--version")
	if err != nil {
		t.Fatal(err)
	}
	want := baselineSupportedGitVersion(version)
	got := op.supported(context.Background(), t.TempDir())
	if (got == nil) != (want == nil) {
		t.Fatalf("production operation bypassed actual host qualification: got %v want %v", got, want)
	}
}

// This reproduction must exercise the real object store even when ci-local's
// /usr/bin/git predates the separately qualified production version floor.
func TestCatalogBaselineHermeticGitQualification(t *testing.T) {
	root := baselineTestOriginal(t, baselineTestOuter(t))
	original, err := baselineTestAcquireOriginal(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if err := original.Verify(); err != nil {
		t.Fatal(err)
	}
	if len(original.SourceBytes(baselineLineages()[0].Original.Path)) == 0 {
		t.Fatal("fixture did not acquire genuine original bytes")
	}
}
