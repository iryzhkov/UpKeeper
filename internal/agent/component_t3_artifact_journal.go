package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/release"
)

// Journal concurrency contract. The supported writers of a transaction's
// transaction.json are UpKeeper runs and the explicit --reconcile-t3-transaction
// command, and both hold the host's target deployment lock for the whole
// read-check-publish sequence, so they are serialized. Any other writer is
// unsupported. Publication compares the on-disk journal with the snapshot this
// transaction took under the lock (the record it last published, or read) and
// refuses on any difference, leaving the journal untouched and keeping the
// intended record under a quarantine name beside it. Nothing that is not
// provably this transaction's is exchanged, undone or deleted; a leftover
// temporary name is renamed to a quarantine name, not removed. An unsupported
// writer acting between the final re-check and the rename is outside the
// contract.

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

// t3Quarantine moves a leftover journal file to a new, unique quarantine name
// beside the journal and returns where its bytes are now. It never replaces an
// existing name and never deletes bytes: the old name is dropped only after
// the quarantine name is proved to hold the same inode, and when no
// quarantine name can be made the file keeps its own name.
func t3Quarantine(path string) string {
	dir := filepath.Dir(path)
	base := strings.TrimPrefix(filepath.Base(path), ".")
	for i := 0; i < 8; i++ {
		q := filepath.Join(dir, fmt.Sprintf("transaction.json.quarantine-%s-%d", base, i))
		if e := os.Link(path, q); e != nil {
			if os.IsExist(e) {
				continue
			}
			return path
		}
		from, e := os.Lstat(path)
		to, err := os.Lstat(q)
		if e == nil && err == nil && os.SameFile(from, to) {
			_ = os.Remove(path)
		}
		return q
	}
	return path
}

// t3Discard drops a name this publication made for a record it proved its
// own. A name that no longer holds exactly that record is quarantined instead.
func t3Discard(path string, expected os.FileInfo, want []byte) string {
	if t3SameRecord(path, expected, want) == nil {
		if os.Remove(path) == nil {
			return ""
		}
	}
	return t3Quarantine(path)
}

// journalCurrent proves the journal at target is still the snapshot this
// transaction took under the lock: absent before its first record, otherwise
// the same inode with the same bytes.
func (tx *t3Transaction) journalCurrent(target string) error {
	if tx.journalInfo == nil {
		if _, e := os.Lstat(target); !os.IsNotExist(e) {
			return errors.New("journal record appeared")
		}
		return nil
	}
	return t3SameRecord(target, tx.journalInfo, tx.journalBytes)
}

// Publish only a completely written, synced and closed journal, and only over
// the snapshot taken under the host's target lock: write a temporary file,
// sync it, re-check the journal, rename over it, sync the directory. The
// superseded inode is kept under a second name until the directory rename is
// durable; a failed directory sync restores it, but only while the journal is
// still exactly the record just published, and reports failure even if the
// restoration sync also fails.
func (tx *t3Transaction) persistJournal(phase string, b []byte) error {
	if !tx.c.lockHeld {
		return errors.New("journal not published: the host's target lock is not held")
	}
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
	target := filepath.Join(tx.dir, "transaction.json")
	f, e := create(tx.dir, ".journal-"+phase+"-*")
	if e != nil {
		return e
	}
	// Until the rename publishes it, the temporary name holds only the intended
	// record; a publication that stops early keeps it under a quarantine name.
	published := false
	defer func() {
		if !published && !t3Absent(f.Name()) {
			t3Quarantine(f.Name())
		}
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
	written, e := os.Lstat(f.Name())
	if e != nil {
		return e
	}
	// refuse leaves the journal as it is and keeps the intended record under a
	// quarantine name, reporting both paths.
	refuse := func(cause error) error {
		published = true
		return fmt.Errorf("journal not published: %w; %s left untouched; intended record kept at %s", cause, target, t3Quarantine(f.Name()))
	}
	if tx.publishFence != nil {
		if e = tx.publishFence(target); e != nil {
			return refuse(e)
		}
	}
	if e = tx.journalCurrent(target); e != nil {
		return refuse(fmt.Errorf("the journal differs from the record read under the target lock: %w", e))
	}
	// Pin the superseded record under a second name for durability recovery,
	// and prove the pinned inode is still the snapshot.
	previous := ""
	if tx.journalInfo != nil {
		previous = f.Name() + "-previous"
		if e = os.Link(target, previous); e != nil {
			return refuse(fmt.Errorf("the journal differs from the record read under the target lock: %w", e))
		}
		if e = t3SameRecord(previous, tx.journalInfo, tx.journalBytes); e != nil {
			return refuse(fmt.Errorf("the journal differs from the record read under the target lock: %w; its record is also kept at %s", e, t3Quarantine(previous)))
		}
	}
	if e = os.Rename(f.Name(), target); e != nil {
		if previous != "" {
			e = errors.Join(e, fmt.Errorf("superseded record kept at %s", t3Discard(previous, tx.journalInfo, tx.journalBytes)))
		}
		return e
	}
	published = true
	d, e = directory(tx.dir)
	if e == nil {
		e = errors.Join(d.Sync(), d.Close())
	}
	if e != nil {
		// Withdraw the record only while the journal is still exactly it. A
		// record another writer left stays, and so does the superseded one.
		if ours := t3SameRecord(target, written, b); ours != nil {
			e = errors.Join(e, fmt.Errorf("%s was changed by an unsupported writer and is left untouched: %w", target, ours))
			if previous != "" {
				e = errors.Join(e, fmt.Errorf("superseded record kept at %s", t3Quarantine(previous)))
			}
		} else if previous != "" {
			if restoreErr := os.Rename(previous, target); restoreErr != nil {
				e = errors.Join(e, restoreErr, fmt.Errorf("superseded record kept at %s", t3Quarantine(previous)))
			}
		} else {
			e = errors.Join(e, os.Remove(target))
		}
		if d, restoreErr := directory(tx.dir); restoreErr == nil {
			e = errors.Join(e, d.Sync(), d.Close())
		} else {
			e = errors.Join(e, restoreErr)
		}
		// The snapshot follows whatever this transaction's record now is.
		if t3SameRecord(target, written, b) == nil {
			tx.journalInfo, tx.journalBytes = written, b
		}
		return fmt.Errorf("journal replacement durability failed: %w", e)
	}
	// The pinned superseded record is dropped only while it is still exactly
	// the snapshot; otherwise it is quarantined.
	if previous != "" {
		t3Discard(previous, tx.journalInfo, tx.journalBytes)
	}
	tx.journalInfo, tx.journalBytes = written, b
	return nil
}
