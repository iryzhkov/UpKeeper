package units_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/units"
)

// Exercise the installed launcher's environment without a login shell or a user
// systemd manager. The only npm that can produce this result lives in mise shims.
func TestServiceLaunchersResolveMiseRuntimeTools(t *testing.T) {
	for _, group := range [][]units.Definition{units.SelfTimer(), units.FleetTimer()} {
		for _, definition := range group {
			if !strings.HasSuffix(definition.Name, ".service") {
				continue
			}
			t.Run(definition.Name, func(t *testing.T) {
				home := t.TempDir()
				writeExecutable := func(relative, contents string) {
					t.Helper()
					path := filepath.Join(home, relative)
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(contents), 0700); err != nil {
						t.Fatal(err)
					}
				}
				writeExecutable(".local/share/mise/shims/npm", "#!/bin/sh\nprintf 'isolated-mise-npm\\n'\n")
				writeExecutable(units.BinaryPath, "#!/bin/sh\ncase \"$1\" in self-pull|scheduled-pull) exec npm --version ;; *) exit 42 ;; esac\n")
				directives := map[string]string{}
				for _, line := range strings.Split(definition.Content, "\n") {
					key, value, ok := strings.Cut(line, "=")
					if ok {
						directives[key] = strings.ReplaceAll(value, "%h", home)
					}
				}
				if err := os.MkdirAll(directives["WorkingDirectory"], 0700); err != nil {
					t.Fatal(err)
				}
				argv := strings.Fields(directives["ExecStart"])
				if len(argv) != 2 {
					t.Fatalf("unexpected launcher: %q", argv)
				}
				command := exec.Command(argv[0], argv[1:]...)
				command.Dir = directives["WorkingDirectory"]
				command.Env = []string{"HOME=" + home, directives["Environment"]}
				output, err := command.CombinedOutput()
				if err != nil || string(output) != "isolated-mise-npm\n" {
					t.Fatalf("launcher did not resolve isolated mise npm: error=%v output=%q", err, output)
				}
			})
		}
	}
}
