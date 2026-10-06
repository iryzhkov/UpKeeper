package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/iryzhkov/UpKeeper/internal/release"
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

// t3RenameExchange atomically exchanges two paths with renameat2
// RENAME_EXCHANGE, so neither record is ever unlinked by the swap.
func t3RenameExchange(a, b string) error {
	from, e := syscall.BytePtrFromString(a)
	if e != nil {
		return e
	}
	to, e := syscall.BytePtrFromString(b)
	if e != nil {
		return e
	}
	const renameExchange = 1 << 1
	atFDCWD := -100 // AT_FDCWD: both paths resolve from the working directory.
	fd := uintptr(atFDCWD)
	if _, _, errno := syscall.Syscall6(t3SysRenameat2, fd, uintptr(unsafe.Pointer(from)), fd, uintptr(unsafe.Pointer(to)), renameExchange, 0); errno != 0 {
		return &os.LinkError{Op: "renameat2 exchange", Old: a, New: b, Err: errno}
	}
	return nil
}

// t3SameRecord proves path holds the expected journal record: the same inode
// with the same bytes.
func t3SameRecord(path string, expected os.FileInfo, want []byte) error {
	current, e := os.Lstat(path)
	if e != nil || !os.SameFile(current, expected) {
		return errors.New("journal record was replaced")
	}
	if got, e := release.T3ReadFile(path, t3JournalLimit); e != nil || !bytes.Equal(got, want) {
		return errors.New("journal record changed")
	}
	return nil
}

// t3ExchangeRecord atomically swaps the records at from and to, then proves the
// record it moved away from to was the expected one. Otherwise it swaps them
// back, so a record another writer published or changed meanwhile is returned
// to its place and never overwritten or unlinked.
func t3ExchangeRecord(from, to string, expected os.FileInfo, want []byte) error {
	if e := t3RenameExchange(from, to); e != nil {
		return fmt.Errorf("atomic journal exchange failed: %w", e)
	}
	if e := t3SameRecord(from, expected, want); e != nil {
		return errors.Join(fmt.Errorf("another writer changed the journal; its record is kept: %w", e), t3RenameExchange(from, to))
	}
	return nil
}

// Publish only a completely written, synced and closed journal. Keep the old
// inode until the directory rename is durable; a failed directory sync restores
// that inode and reports failure even if the restoration sync also fails.
// Publication and restoration each exchange records atomically and verify what
// they displaced, so a record another writer left after the publish fence is
// kept, together with the superseded record recovery needs.
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
	// After an exchange the temporary name holds the superseded record, and if
	// undoing an exchange failed, another writer's record, which is kept.
	var written, superseded os.FileInfo
	defer func() {
		if info, e := os.Lstat(f.Name()); e == nil && written != nil && !os.SameFile(info, written) && (superseded == nil || !os.SameFile(info, superseded)) {
			return
		}
		_ = os.Remove(f.Name())
	}()
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
	var supersededBytes []byte
	if hadPrevious {
		if superseded, e = os.Lstat(previous); e == nil {
			supersededBytes, e = release.T3ReadFile(previous, t3JournalLimit)
		}
		if e != nil {
			cleanupPrevious = true
			return e
		}
	}
	// The hard link pins the journal about to be replaced, so the fence proves
	// the exact record this write supersedes, not a newer one.
	if tx.publishFence != nil {
		if e = tx.publishFence(previous); e != nil {
			cleanupPrevious = hadPrevious
			return e
		}
	}
	if written, e = os.Lstat(f.Name()); e != nil {
		cleanupPrevious = hadPrevious
		return e
	}
	// A writer acting after the fence either left a record where none was, which
	// the link refuses to replace, or replaced or changed the superseded record,
	// which the exchange detects and undoes.
	if hadPrevious {
		e = t3ExchangeRecord(f.Name(), target, superseded, supersededBytes)
	} else {
		e = os.Link(f.Name(), target)
	}
	if e != nil {
		cleanupPrevious = hadPrevious
		return e
	}
	d, e = directory(tx.dir)
	if e == nil {
		e = errors.Join(d.Sync(), d.Close())
	}
	if e != nil {
		// Withdraw this record only while the journal is still exactly it. A
		// newer record stays, and so does the superseded one recovery needs.
		switch {
		case !hadPrevious:
			if ours := t3SameRecord(target, written, b); ours != nil {
				e = errors.Join(e, fmt.Errorf("another writer changed the journal; its record is kept: %w", ours))
			} else {
				e = errors.Join(e, os.Remove(target))
			}
		case t3SameRecord(target, written, b) != nil:
			e = errors.Join(e, fmt.Errorf("another writer changed the journal; its record is kept and the superseded record is at %s", previous))
		default:
			if restoreErr := t3ExchangeRecord(previous, target, written, b); restoreErr != nil {
				e = errors.Join(e, restoreErr, fmt.Errorf("superseded record kept at %s", previous))
			} else {
				// The withdrawn record now has the superseded record's name.
				cleanupPrevious = true
			}
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
