package units

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// Regressions for the self-review of the legacy drop-in reconciliation.

func installWithin(t *testing.T, limit time.Duration, calls *[]string) (Installation, error) {
	t.Helper()
	type outcome struct {
		done Installation
		err  error
	}
	result := make(chan outcome, 1)
	go func() {
		done, err := install(SelfTimer(), "upkeeper-self.timer", recordingRunner(calls))
		result <- outcome{done, err}
	}()
	select {
	case got := <-result:
		return got.done, got.err
	case <-time.After(limit):
		t.Fatalf("the installer did not return within %s", limit)
		return Installation{}, nil
	}
}

// A FIFO under the legacy name would block an open forever. It is refused as
// not a regular file without being opened.
func TestAFIFOUnderTheLegacyNameIsRefusedWithoutBlocking(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	directory := filepath.Join(systemdUserDirectory(home), "upkeeper-self.service.d")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(directory, legacyDropInName)
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	var calls []string
	_, err := installWithin(t, 10*time.Second, &calls)
	if err == nil || !strings.Contains(err.Error(), fifo) ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("the FIFO was not refused by name and reason: %v", err)
	}
	assertNothingInstalled(t, home, SelfTimer())
}

// A symbolic link under the legacy name is refused with that reason, also
// when it dangles because the checkout it pointed into no longer has the file.
func TestASymlinkedLegacyDropInIsRefusedWithItsReason(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		target := filepath.Join(home, "checkout", legacyDropInName)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if !dangling {
			if err := os.WriteFile(target, []byte(historicalSelfDropIn), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		directory := filepath.Join(systemdUserDirectory(home), "upkeeper-self.service.d")
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(directory, legacyDropInName)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		var calls []string
		_, err := install(SelfTimer(), "upkeeper-self.timer", recordingRunner(&calls))
		if err == nil || !strings.Contains(err.Error(), link) ||
			!strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("dangling=%v: the link was not refused by name and reason: %v", dangling, err)
		}
		assertNothingInstalled(t, home, SelfTimer())
		if !dangling {
			if _, err := os.Stat(target); err != nil {
				t.Errorf("the link's target was touched: %v", err)
			}
		}
	}
}

// A drop-in directory that is a symbolic link, for instance into a release
// checkout, is never written through: the installer refuses and the file in
// the link's target stays.
func TestASymlinkedDropInDirectoryIsRefusedAndItsTargetKept(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	outside := filepath.Join(home, "checkout", "scripts", "upkeeper-self.service.d")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(outside, legacyDropInName)
	if err := os.WriteFile(tracked, []byte(historicalSelfDropIn), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(systemdUserDirectory(home), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(systemdUserDirectory(home), "upkeeper-self.service.d")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	var calls []string
	_, err := install(SelfTimer(), "upkeeper-self.timer", recordingRunner(&calls))
	if err == nil || !strings.Contains(err.Error(), link) ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("the linked directory was not refused by name: %v", err)
	}
	if _, err := os.Stat(tracked); err != nil {
		t.Errorf("the installer removed a file outside the systemd directory: %v", err)
	}
	assertNothingInstalled(t, home, SelfTimer())
	if len(calls) != 0 {
		t.Errorf("the installer ran %q despite refusing", calls)
	}
}

// A run killed after removing the drop-in and before removing its directory
// leaves an empty directory; the rerun removes it and says so.
func TestARerunRemovesTheEmptyDropInDirectoryAnInterruptedRunLeft(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	directory := filepath.Join(systemdUserDirectory(home), "upkeeper-self.service.d")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	var calls []string
	done, err := install(SelfTimer(), "upkeeper-self.timer", recordingRunner(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Errorf("the empty drop-in directory survived the rerun: %v", err)
	}
	if strings.Join(done.Removed, "\n") != directory {
		t.Errorf("reported removed = %q, want the directory", done.Removed)
	}
}

// A run killed inside writeUnit leaves its temporary file; the rerun removes
// it.
func TestARerunRemovesTheTemporaryFileAKilledWriteLeft(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(systemdUserDirectory(home), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(systemdUserDirectory(home), ".upkeeper-self.service.123456.tmp")
	if err := os.WriteFile(stale, []byte("[Unit]\nDesc"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	if _, err := install(SelfTimer(), "upkeeper-self.timer", recordingRunner(&calls)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale temporary file survived the rerun: %v", err)
	}
	assertInstalled(t, home, SelfTimer())
}

// A systemctl failure after the removal returns what was already changed, so
// the command can report it.
func TestASystemctlFailureAfterTheRemovalReturnsWhatChanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dropIn := writeDropIn(t, home, "upkeeper-self.service", legacyDropInName, historicalSelfDropIn)
	failing := func(procx.Command) procx.Result { return procx.Result{Code: 1} }
	done, err := install(SelfTimer(), "upkeeper-self.timer", failing)
	if err == nil || !strings.Contains(err.Error(), "daemon-reload") {
		t.Fatalf("the reload failure was not returned: %v", err)
	}
	if len(done.Written) != 2 || len(done.Removed) != 2 || done.Removed[0] != dropIn {
		t.Errorf("the partial installation is not returned: %+v", done)
	}
}
