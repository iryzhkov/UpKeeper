package fsatomic

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestRenameNoReplaceMovesADirectoryIntoAFreePath(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "stage", "checkout")
	if err := os.MkdirAll(filepath.Join(source, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README"), []byte("built\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "huyang")
	if err := RenameNoReplace(source, target); err != nil {
		t.Fatalf("RenameNoReplace: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "README")); err != nil || string(data) != "built\n" {
		t.Fatalf("moved content = %q (%v)", data, err)
	}
	if _, err := os.Lstat(source); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("source still present: %v", err)
	}
}

// Somebody else's directory, even an empty one, is never replaced.
func TestRenameNoReplaceRefusesAnExistingPath(t *testing.T) {
	for _, kind := range []string{"empty directory", "file", "dangling symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "candidate")
			if err := os.MkdirAll(source, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "live")
			var err error
			switch kind {
			case "empty directory":
				err = os.Mkdir(target, 0o755)
			case "file":
				err = os.WriteFile(target, []byte("theirs"), 0o644)
			default:
				err = os.Symlink(filepath.Join(root, "absent"), target)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := RenameNoReplace(source, target); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("RenameNoReplace onto an existing %s = %v, want fs.ErrExist", kind, err)
			}
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("the refused source moved: %v", err)
			}
			if _, err := os.Lstat(target); err != nil {
				t.Fatalf("the existing target was removed: %v", err)
			}
		})
	}
}
