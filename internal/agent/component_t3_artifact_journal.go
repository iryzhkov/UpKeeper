package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type t3JournalError struct{ error }

func (e *t3JournalError) Unwrap() error { return e.error }

func (e *t3JournalError) Error() string { return "T3 journal persistence failed: " + e.error.Error() }

type t3JournalFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Name() string
}
type t3JournalFiles struct {
	create    func(string, string) (t3JournalFile, error)
	directory func(string) (t3JournalFile, error)
}

func t3Absent(path string) bool {
	_, e := os.Lstat(path)
	return os.IsNotExist(e)
}

// Publish only a completely written, synced and closed journal. Keep the old
// inode until the directory rename is durable; a failed directory sync restores
// that inode and reports failure even if the restoration sync also fails.
func (tx *t3Transaction) persistJournal(phase string, b []byte) error {
	create := func(dir, pattern string) (t3JournalFile, error) { return os.CreateTemp(dir, pattern) }
	directory := func(dir string) (t3JournalFile, error) { return os.Open(dir) }
	if tx.c.t3JournalFiles != nil {
		if tx.c.t3JournalFiles.create != nil {
			create = tx.c.t3JournalFiles.create
		}
		if tx.c.t3JournalFiles.directory != nil {
			directory = tx.c.t3JournalFiles.directory
		}
	}
	f, e := create(tx.dir, ".journal-"+phase+"-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	n, e := f.Write(b)
	if e == nil && n != len(b) {
		e = io.ErrShortWrite
	}
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	d, e := directory(tx.dir)
	if e != nil {
		return e
	}
	// Detect sync/close failures before replacing the last usable journal.
	if e = errors.Join(d.Sync(), d.Close()); e != nil {
		return e
	}
	target := filepath.Join(tx.dir, "transaction.json")
	previous := f.Name() + "-previous"
	if e = os.Link(target, previous); e != nil && !os.IsNotExist(e) {
		return e
	}
	hadPrevious := e == nil
	cleanupPrevious := false
	defer func() {
		if cleanupPrevious {
			_ = os.Remove(previous)
		}
	}()
	if e = os.Rename(f.Name(), target); e != nil {
		return e
	}
	d, e = directory(tx.dir)
	if e == nil {
		e = errors.Join(d.Sync(), d.Close())
	}
	if e != nil {
		if hadPrevious {
			e = errors.Join(e, os.Rename(previous, target))
		} else {
			e = errors.Join(e, os.Remove(target))
		}
		if d, restoreErr := directory(tx.dir); restoreErr == nil {
			e = errors.Join(e, d.Sync(), d.Close())
		} else {
			e = errors.Join(e, restoreErr)
		}
		return fmt.Errorf("journal replacement durability failed: %w", e)
	}
	cleanupPrevious = true
	return nil
}
