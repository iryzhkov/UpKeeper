package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func TestT3RepairReceiptNullIntegers(t *testing.T) {
	for _, key := range []string{"size", "mode"} {
		t.Run(key, func(t *testing.T) {
			s, _, body := t3Fixture(t, "linux-x64")
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			m["files"].([]any)[0].(map[string]any)[key] = nil
			body, _ = json.Marshal(m)
			if _, e := DecodeT3Receipt(body, s, "linux-x64", "0.0.45"); e == nil {
				t.Fatal("accepted null " + key)
			}
		})
	}
}

func TestT3RepairCaptureSelectedLauncher(t *testing.T) {
	for _, choice := range []string{"owned-path", "foreign-path", "owned-mise", "foreign-mise", "failed-mise", "relative-mise"} {
		t.Run(choice, func(t *testing.T) {
			home, prefix, spec, runner := t3RetainedFixture(t)
			selected := filepath.Join(prefix, "bin/t3")
			foreign := filepath.Join(t.TempDir(), "t3")
			if e := os.WriteFile(foreign, []byte("foreign"), 0755); e != nil {
				t.Fatal(e)
			}
			if choice == "foreign-path" {
				t.Setenv("PATH", filepath.Dir(foreign))
			}
			if strings.Contains(choice, "mise") {
				shimDir := t.TempDir()
				mise := filepath.Join(shimDir, "mise")
				if e := os.WriteFile(mise, []byte("fixture"), 0755); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(mise, filepath.Join(shimDir, "t3")); e != nil {
					t.Fatal(e)
				}
				t.Setenv("PATH", shimDir)
				original := runner
				runner = func(c procx.Command) procx.Result {
					if len(c.Argv) > 1 && c.Argv[1] == "which" {
						switch choice {
						case "foreign-mise":
							return procx.Result{Stdout: foreign}
						case "failed-mise":
							return procx.Result{Code: 1}
						case "relative-mise":
							return procx.Result{Stdout: "relative/t3"}
						}
						return procx.Result{Stdout: selected}
					}
					return original(c)
				}
			}
			pin, e := captureT3(runner, home, map[string]any{"path": ".local/bin/t3-steward", "version": "0.11.0"}, spec)
			good := choice == "owned-path" || choice == "owned-mise"
			if good && (e != nil || pin["artifact"] == nil) {
				t.Fatalf("valid selection refused: %v", e)
			}
			if !good && e == nil {
				t.Fatal("captured wrong selected launcher")
			}
		})
	}
}
