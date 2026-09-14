package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// A directory that merely holds two files named like the fleet's is not a
// release checkout. Converging out of one is the worst mistake this program
// can make, and the Python implementation could not be pointed at the working
// directory at all.
func TestTheWorkingDirectoryMustBeACheckout(t *testing.T) {
	planted := fleetFiles(t, t.TempDir(), false)
	chdir(t, planted)
	t.Setenv(ConfigRootEnvironment, "")
	t.Setenv("HOME", t.TempDir())

	root, err := ConfigRoot()
	if err == nil && sameDirectory(t, root, planted) {
		t.Fatalf("a plain directory was accepted as the configuration root: %s", root)
	}
}

// The documented scratch-clone procedure depends on a checkout in the working
// directory winning: without it, `push --no-commit` run in a scratch clone
// would write the live checkout's manifest.
func TestACheckoutInTheWorkingDirectoryWins(t *testing.T) {
	clone := fleetFiles(t, t.TempDir(), true)
	chdir(t, clone)
	t.Setenv(ConfigRootEnvironment, "")
	t.Setenv("HOME", t.TempDir())

	root, err := ConfigRoot()
	if err != nil {
		t.Fatalf("a checkout in the working directory was refused: %v", err)
	}
	if !sameDirectory(t, root, clone) {
		t.Fatalf("resolved %s, want %s", root, clone)
	}
}

// An explicit override is an operator's decision and needs no checkout: a host
// may carry the configuration without the history.
func TestAnOverrideNeedsNoCheckout(t *testing.T) {
	planted := fleetFiles(t, t.TempDir(), false)
	chdir(t, t.TempDir())
	t.Setenv(ConfigRootEnvironment, planted)

	root, err := ConfigRoot()
	if err != nil {
		t.Fatalf("the override was refused: %v", err)
	}
	if !sameDirectory(t, root, planted) {
		t.Fatalf("resolved %s, want the override %s", root, planted)
	}
}

func fleetFiles(t *testing.T, root string, asCheckout bool) string {
	t.Helper()
	for path, content := range map[string]string{
		filepath.Join("inventory", "hosts.yml"):    "{}",
		filepath.Join("desired", "components.yml"): "{}",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if asCheckout {
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func chdir(t *testing.T, directory string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
}

// sameDirectory compares resolved paths, because a temporary directory on this
// platform may sit behind a symlink.
func sameDirectory(t *testing.T, left, right string) bool {
	t.Helper()
	resolve := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		return path
	}
	return resolve(left) == resolve(right)
}
