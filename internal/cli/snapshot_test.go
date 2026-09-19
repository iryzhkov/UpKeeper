package cli_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeUnder creates a file and every directory above it.
func writeUnder(t *testing.T, root, relative, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// The snapshot helper exists to prove that a checkout was left as it was found,
// and a worktree is what that claim is about. Git's own bookkeeping is no part
// of it, and Git rewrites that bookkeeping on its own schedule, so the walk must
// neither describe .git nor trip over it.
func TestSnapshotDescribesTheWorktreeAndNotGitBookkeeping(t *testing.T) {
	root := t.TempDir()
	components := writeUnder(t, root, "desired/components.yml", "components: {}\n")
	hosts := writeUnder(t, root, "inventory/hosts.yml", "hosts: {}\n")
	writeUnder(t, root, ".git/index", "git bookkeeping")
	writeUnder(t, root, ".git/objects/pack/pack-fixture.idx", "git bookkeeping")

	files := snapshot(t, root)

	for _, path := range []string{components, hosts} {
		if _, present := files[path]; !present {
			t.Errorf("the snapshot lost %s", path)
		}
	}
	for path := range files {
		if strings.Contains(path, string(filepath.Separator)+".git"+string(filepath.Separator)) {
			t.Errorf("the snapshot describes Git bookkeeping: %s", path)
		}
	}
	if len(files) != 2 {
		t.Errorf("the snapshot holds %d files, wanted 2: %v", len(files), files)
	}
}

// Removing a file underneath a running walk cannot be scheduled, so the walk
// function is driven directly with the two errors the race produces. A name
// that readdir listed and lstat could no longer find reaches the walk function
// as a nil FileInfo and an lstat error, which is how the background
// `git maintenance run` that `git commit` detaches used to fail this package.
func TestSnapshotEntryDistinguishesAVanishedPathFromAFailure(t *testing.T) {
	root := t.TempDir()
	lock := filepath.Join(root, ".git", "objects", "maintenance.lock")

	for _, test := range []struct {
		name     string
		path     string
		walkErr  error
		failWalk bool
	}{
		{
			name:    "a path removed under the walk was never part of the tree",
			path:    lock,
			walkErr: &fs.PathError{Op: "lstat", Path: lock, Err: fs.ErrNotExist},
		},
		{
			name:     "any other lstat failure still fails the snapshot",
			path:     lock,
			walkErr:  &fs.PathError{Op: "lstat", Path: lock, Err: fs.ErrPermission},
			failWalk: true,
		},
		{
			name:     "a root that is not there is a broken test, not a race",
			path:     root,
			walkErr:  &fs.PathError{Op: "lstat", Path: root, Err: fs.ErrNotExist},
			failWalk: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := map[string]string{}
			err := snapshotEntry(root, files)(test.path, nil, test.walkErr)
			if test.failWalk && !errors.Is(err, test.walkErr) {
				t.Fatalf("the walk returned %v, wanted %v", err, test.walkErr)
			}
			if !test.failWalk && err != nil {
				t.Fatalf("the walk returned %v", err)
			}
			if len(files) != 0 {
				t.Errorf("the snapshot recorded %v", files)
			}
		})
	}
}
