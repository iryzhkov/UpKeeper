package fsatomic

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// procFlushError is the error the kernel gives for a flush of /proc, a
// directory whose filesystem cannot flush one: a real failed fsync.
func procFlushError(t *testing.T) error {
	t.Helper()
	handle, err := os.Open("/proc")
	if err != nil {
		t.Skipf("no /proc to flush: %v", err)
	}
	defer func() { _ = handle.Close() }()
	underlying := handle.Sync()
	if underlying == nil {
		t.Skip("this machine's /proc accepts a flush")
	}
	return underlying
}

// A directory whose flush really fails is reported by the strict flush, and
// only the best-effort SyncDirectory keeps treating it as success.
func TestARealDirectoryFlushFailureIsReported(t *testing.T) {
	underlying := procFlushError(t)
	err := Flusher{}.Directory("/proc")
	if err == nil {
		t.Fatalf("a real directory flush failure was swallowed: %v", underlying)
	}
	if !errors.Is(err, underlying) && !strings.Contains(err.Error(), underlying.Error()) {
		t.Errorf("the flush failure %q is not the one the kernel gave, %q", err, underlying)
	}
	if err := SyncDirectory("/proc"); err != nil {
		t.Errorf("the best-effort SyncDirectory changed behaviour: %v", err)
	}
}

// failOn flushes for real, except that the flush of the path it names fails.
func failOn(path string, failed error, hits *int) Flusher {
	return Flusher{Sync: func(handle *os.File) error {
		if handle.Name() == path {
			*hits++
			return failed
		}
		return handle.Sync()
	}}
}

func TestEveryFlushFailureIsReturned(t *testing.T) {
	failed := errors.New("flush failed")
	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		hits := 0
		if err := failOn(path, failed, &hits).File(path); !errors.Is(err, failed) || hits != 1 {
			t.Errorf("a failed file flush returned %v after %d flushes", err, hits)
		}
	})
	t.Run("directory", func(t *testing.T) {
		directory := t.TempDir()
		hits := 0
		if err := failOn(directory, failed, &hits).Directory(directory); !errors.Is(err, failed) || hits != 1 {
			t.Errorf("a failed directory flush returned %v after %d flushes", err, hits)
		}
	})
	t.Run("written file", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "record")
		flusher := Flusher{Sync: func(handle *os.File) error {
			if strings.HasPrefix(filepath.Base(handle.Name()), ".record-") {
				return failed
			}
			return handle.Sync()
		}}
		if err := flusher.WriteFile(path, []byte("new"), 0o600); !errors.Is(err, failed) {
			t.Errorf("a failed flush of the written file returned %v", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a file whose flush failed was published: %v", err)
		}
	})
	t.Run("written file's directory", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "record")
		hits := 0
		if err := failOn(directory, failed, &hits).WriteFile(path, []byte("new"), 0o600); !errors.Is(err, failed) || hits != 1 {
			t.Errorf("a failed flush of the directory returned %v after %d flushes", err, hits)
		}
	})
	t.Run("success", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "record")
		if err := (Flusher{}).WriteFile(path, []byte("new"), 0o640); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("written file: %v %v", info, err)
		}
		if content, _ := os.ReadFile(path); string(content) != "new" {
			t.Errorf("written content %q", content)
		}
	})
}
