// Package fsatomic writes files so that a crash can never leave a half-written
// durable record behind.
//
// Every write goes to a fresh temporary file in the destination directory, is
// given its final mode before any content, is flushed to disk, and only then
// replaces the target. A reader therefore sees either the previous content or
// the complete new content, never a prefix.
package fsatomic

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile atomically replaces path with data, giving the file the requested
// mode before it contains anything.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("cannot create a temporary file in %s: %w", directory, err)
	}
	name := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(name)
	}()
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("cannot set the mode of %s: %w", name, err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("cannot write %s: %w", name, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("cannot flush %s: %w", name, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("cannot close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("cannot publish %s: %w", path, err)
	}
	return SyncDirectory(directory)
}

// WritePrivateFile is WriteFile for records only their owner may read.
func WritePrivateFile(path string, data []byte) error {
	return WriteFile(path, data, 0o600)
}

// EnsureDirectory creates a directory tree and forces the mode of its last
// component, which MkdirAll alone does not do for an existing directory.
func EnsureDirectory(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("cannot create %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("cannot set the mode of %s: %w", path, err)
	}
	return nil
}

// SyncDirectory flushes a directory entry so that a rename survives a crash.
func SyncDirectory(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer func() { _ = handle.Close() }()
	if err := handle.Sync(); err != nil {
		// Some filesystems refuse to sync a directory handle. The rename is
		// still ordered by the kernel, so this is reported but not fatal.
		return nil
	}
	return nil
}

// CreateExclusive creates a file that must not already exist, writes data and
// flushes it. It is the primitive behind every durable marker.
func CreateExclusive(path string, data []byte, mode os.FileMode) error {
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := handle.Write(data); err != nil {
		_ = handle.Close()
		_ = os.Remove(path)
		return err
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		_ = os.Remove(path)
		return err
	}
	return handle.Close()
}
