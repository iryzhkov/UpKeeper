package runstore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/runstore"
)

func TestWriteIsPrivateRedactedAndAtomic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	store := &runstore.Store{Root: root}
	commit := "a" + strings.Repeat("0", 39)
	record, err := runstore.New(
		"20260914T090000Z-0a1b2c3d", "pull", "normandy", &commit,
		[]string{"huyang"}, []string{"omarchy-pc"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	record.Details = map[string]any{
		"token": "ghp_abcdefghijklmnopqrstuvwxyz0123",
		"note":  "password=hunter2",
	}
	path, err := store.Write(record)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("record mode is %v, expected 0600", info.Mode().Perm())
	}
	directory, err := os.Stat(root)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if directory.Mode().Perm() != 0o700 {
		t.Errorf("store mode is %v, expected 0700", directory.Mode().Perm())
	}

	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(payload), "hunter2") ||
		strings.Contains(string(payload), "ghp_abcdef") {
		t.Error("a secret reached the run record")
	}
	if !strings.HasSuffix(string(payload), "}\n") {
		t.Error("the record is not newline terminated")
	}

	// A temporary file must never survive the write.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the store holds %d entries, expected only the record", len(entries))
	}

	read, err := store.Read(record.RunID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read["command"] != "pull" || read["phase"] != "created" {
		t.Errorf("round trip lost fields: %#v", read)
	}
}

func TestListIDsIgnoresAnythingThatIsNotARunRecord(t *testing.T) {
	root := t.TempDir()
	store := &runstore.Store{Root: root}
	for _, name := range []string{
		"20260914T090000Z-0a1b2c3d.json",
		"20260913T090000Z-ffffffff.json",
		"not-a-run.json",
		"20260914T090000Z-0a1b2c3d.txt",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("{}"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	ids := store.ListIDs()
	if len(ids) != 2 || ids[0] != "20260913T090000Z-ffffffff" {
		t.Errorf("produced %v", ids)
	}
}

func TestWriteRefusesAnInvalidRunIdentifier(t *testing.T) {
	store := &runstore.Store{Root: t.TempDir()}
	if _, err := runstore.New("nonsense", "pull", "normandy", nil, nil, nil); err == nil {
		t.Fatal("expected a refusal from New")
	}
	if _, err := store.Write(&runstore.Record{RunID: "nonsense"}); err == nil {
		t.Fatal("expected a refusal from Write")
	}
}
