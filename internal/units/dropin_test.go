package units

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

// A drop-in that is not byte for byte one UpKeeper shipped would silently
// change the unit the installer writes, so the installer refuses it, even
// under the legacy canary's file name.
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
