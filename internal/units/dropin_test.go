package units

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// The drop-in and the installed unit are not composable: a host that has run
// the installer and then deletes the drop-in still runs the binary, while
// believing it rolled back. The installer refuses rather than making the
// rollback silently do nothing.
func TestInstallRefusesWhenADropInOverridesTheUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dropIn := filepath.Join(home, ".config", "systemd", "user", "upkeeper-self.service.d")
	if err := os.MkdirAll(dropIn, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropIn, "10-upkeeper-binary.conf"),
		[]byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ran := false
	_, err := install(SelfTimer(), "upkeeper-self.timer", func(procx.Command) procx.Result {
		ran = true
		return procx.Result{Code: 0}
	})
	if err == nil {
		t.Fatal("the installer overwrote a unit a drop-in was overriding")
	}
	if !strings.Contains(err.Error(), "drop-in") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
	if ran {
		t.Error("the installer reloaded systemd despite refusing")
	}
	if _, err := os.Stat(filepath.Join(
		home, ".config", "systemd", "user", "upkeeper-self.service")); err == nil {
		t.Error("the installer wrote the unit anyway")
	}
}
