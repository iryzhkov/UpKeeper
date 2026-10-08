package agentenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An I/O error while restoring leaves part of the previous environment
// unrestored. The agent stops the whole host only for a failure that says its
// rollback failed, so restore must say so for every error, not only for the
// two it detects itself.
func TestARestoreIOFailureSaysTheRollbackFailed(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		prepare func(t *testing.T, a, b string)
	}{
		// Reading back the activated target fails: it became a directory.
		{"read", func(t *testing.T, a, b string) {
			if err := os.Mkdir(b, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		// Writing the prior bytes back fails: the directory is read-only.
		{"write", func(t *testing.T, a, b string) {
			if os.Geteuid() == 0 {
				t.Skip("root ignores directory permissions")
			}
			if err := os.WriteFile(b, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Dir(b)
			if err := os.Chmod(directory, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := t.TempDir()
			a, b := filepath.Join(home, "a"), filepath.Join(home, "b")
			if err := os.WriteFile(a, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			testCase.prepare(t, a, b)
			observation := &Observation{
				Desired: map[string][]byte{"a": []byte("new"), "b": []byte("new")},
				Prior:   map[string][]byte{"a": []byte("old"), "b": []byte("old")},
			}
			err := restore(observation, map[string]string{"a": a, "b": b},
				map[string]os.FileMode{"a": 0o600, "b": 0o600}, []string{"a", "b"})
			if err == nil {
				t.Fatal("restore succeeded on a target it cannot restore")
			}
			if !strings.HasPrefix(err.Error(), "rollback failed: ") {
				t.Fatalf("an unrestored environment is reported as an ordinary failure: %v", err)
			}
			if strings.Count(err.Error(), "rollback failed") != 1 {
				t.Errorf("the rollback failure is named more than once: %v", err)
			}
		})
	}
}

// The two failures restore detects itself keep their exact wording.
func TestRestoreKeepsItsOwnRollbackWording(t *testing.T) {
	home := t.TempDir()
	a := filepath.Join(home, "a")
	if err := os.WriteFile(a, []byte("somebody else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	observation := &Observation{
		Desired: map[string][]byte{"a": []byte("new")},
		Prior:   map[string][]byte{"a": []byte("old")},
	}
	err := restore(observation, map[string]string{"a": a}, map[string]os.FileMode{"a": 0o600}, []string{"a"})
	if err == nil || err.Error() != "rollback failed: unexplained managed-path change" {
		t.Fatalf("err = %v", err)
	}
}
