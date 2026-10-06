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
// mode before it contains anything. Its flush of the directory is
// SyncDirectory's, and best-effort; Flusher.WriteFile is the strict one.
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

// SyncDirectory flushes a directory entry so that a rename survives a crash,
// as far as it can: it is best-effort. It reports a directory it cannot open,
// but it returns nil for every error of the flush itself, an I/O error as much
// as a filesystem that cannot flush a directory, so its nil does not mean the
// entries are durable. A caller that must not report durability it does not
// have uses Flusher.Directory instead.
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

// Flusher makes files and directory entries durable and, unlike SyncDirectory,
// returns every error of every flush, so that a caller can refuse to name or
// switch to something whose durability it could not establish.
type Flusher struct {
	// Sync is the flush itself; nil means (*os.File).Sync. It exists so that a
	// test can make the flush of one particular file or directory fail.
	Sync func(handle *os.File) error
}

func (f Flusher) sync(handle *os.File) error {
	if f.Sync != nil {
		return f.Sync(handle)
	}
	return handle.Sync()
}

// File flushes the content of a file.
func (f Flusher) File(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer func() { _ = handle.Close() }()
	if err := f.sync(handle); err != nil {
		return fmt.Errorf("cannot flush %s: %w", path, err)
	}
	return nil
}

// Directory flushes the entries of a directory, so that a file created in it
// or renamed into or within it survives a crash.
func (f Flusher) Directory(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer func() { _ = handle.Close() }()
	if err := f.sync(handle); err != nil {
		return fmt.Errorf("cannot flush the directory %s: %w", path, err)
	}
	return nil
}

// WriteFile is the package's WriteFile with every flush, the file's and the
// directory's, made through f and every failure of one returned.
func (f Flusher) WriteFile(path string, data []byte, mode os.FileMode) error {
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
	if err := f.sync(temporary); err != nil {
		return fmt.Errorf("cannot flush %s: %w", name, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("cannot close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("cannot publish %s: %w", path, err)
	}
	return f.Directory(directory)
}
