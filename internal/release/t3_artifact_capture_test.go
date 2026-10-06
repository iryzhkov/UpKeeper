package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
)

func t3RetainedFixture(t *testing.T) (string, string, map[string]any, procx.Runner) {
	t.Helper()
	home := t.TempDir()
	prefix := filepath.Join(home, ".local/t3-prefix")
	root := filepath.Join(prefix, "lib/node_modules/t3")
	if e := os.MkdirAll(root, 0755); e != nil {
		t.Fatal(e)
	}
	s, archive, receipt := t3Fixture(t, T3Target())
	r, e := VerifyT3Bundle(archive, receipt, s, T3Target(), "0.0.45", root)
	if e != nil {
		t.Fatal(e)
	}
	if e = VerifyT3Tree(root, r, s); e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Join(prefix, "bin"), 0755); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink("../lib/node_modules/t3/bin/t3.js", filepath.Join(prefix, "bin/t3")); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", filepath.Join(prefix, "bin"))
	spec := map[string]any{"kind": "npm-package", "package": "t3", "version": "0.0.45", "service": "t3code.service", "artifact": t3ArtifactMap(s), "compatibility": map[string]any{"min": "0.0.38", "max": "0.0.45", "steward_version": "0.11.0"}}
	pin, _ := json.Marshal(spec)
	location, _ := json.Marshal(map[string]any{"prefix": prefix, "link": "../lib/node_modules/t3/bin/t3.js"})
	state := filepath.Join(home, T3StatePath, "current")
	if e = os.MkdirAll(state, 0700); e != nil {
		t.Fatal(e)
	}
	for name, b := range map[string][]byte{"pin.json": pin, "location.json": location, "archive.tar.gz": archive, "receipt.json": receipt} {
		if e = os.WriteFile(filepath.Join(state, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	runner := func(c procx.Command) procx.Result {
		if c.Argv[len(c.Argv)-1] == "version" {
			return procx.Result{Stdout: "t3-steward 0.11.0 (tested with T3 0.0.38..0.0.45)"}
		}
		if len(c.Argv) > 1 && c.Argv[1] == "prefix" {
			return procx.Result{Stdout: prefix}
		}
		return procx.Result{Stdout: `{"dependencies":{"t3":{"version":"0.0.45"}}}`}
	}
	return home, prefix, spec, runner
}
func TestT3CaptureRoundTripAndProvenanceRefusals(t *testing.T) {
	steward := map[string]any{"path": ".local/bin/t3-steward", "version": "0.11.0"}
	home, _, spec, runner := t3RetainedFixture(t)
	got, e := captureT3(runner, home, steward, spec)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := json.Marshal(got["artifact"])
	b, _ := json.Marshal(spec["artifact"])
	if string(a) != string(b) {
		t.Fatal("capture changed immutable pin")
	}
	for _, tc := range []string{"receipt", "archive", "pin", "payload", "extra", "missing-metadata", "missing-retention", "conflict"} {
		t.Run(tc, func(t *testing.T) {
			home, prefix, spec, runner := t3RetainedFixture(t)
			state := filepath.Join(home, T3StatePath, "current")
			root := filepath.Join(prefix, "lib/node_modules/t3")
			switch tc {
			case "receipt":
				if e := os.Remove(filepath.Join(state, "receipt.json")); e != nil {
					t.Fatal(e)
				}
			case "archive":
				if e := os.WriteFile(filepath.Join(state, "archive.tar.gz"), []byte("corrupt"), 0600); e != nil {
					t.Fatal(e)
				}
			case "pin":
				if e := os.WriteFile(filepath.Join(state, "pin.json"), []byte("{}"), 0600); e != nil {
					t.Fatal(e)
				}
			case "payload":
				if e := os.WriteFile(filepath.Join(root, "bin/t3.js"), []byte("changed"), 0755); e != nil {
					t.Fatal(e)
				}
			case "extra":
				if e := os.WriteFile(filepath.Join(root, "extra"), []byte("changed"), 0600); e != nil {
					t.Fatal(e)
				}
			case "missing-metadata":
				if e := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"t3","version":"0.0.45"}`), 0644); e != nil {
					t.Fatal(e)
				}
			case "missing-retention":
				if e := os.RemoveAll(filepath.Join(home, T3StatePath)); e != nil {
					t.Fatal(e)
				}
			case "conflict":
				spec["artifact"].(map[string]any)["tag"] = "different-tag"
			}
			if _, e := captureT3(runner, home, steward, spec); e == nil {
				t.Fatal("capture laundered controlled origin")
			}
			if tc != "conflict" {
				if _, e := captureT3(runner, home, steward); e == nil {
					t.Fatal("capture without previous pin laundered controlled origin")
				}
			}
		})
	}
}
func TestT3CaptureRefusesCompleteLossOfOrigin(t *testing.T) {
	home, _, _, runner := t3RetainedFixture(t)
	if e := os.RemoveAll(filepath.Join(home, T3StatePath)); e != nil {
		t.Fatal(e)
	}
	ambiguous := func(c procx.Command) procx.Result {
		result := runner(c)
		if c.Argv[0] == "npm" && c.Argv[1] == "ls" {
			result.Stdout = `{"dependencies":{"t3":{"version":"0.0.45"}}}`
		}
		return result
	}
	// Even after all marker/management inputs disappear, numeric equality alone
	// cannot establish registry origin.
	if _, e := captureT3(ambiguous, home, map[string]any{"path": "steward", "version": "0.11.0"}); e == nil || !strings.Contains(e.Error(), "provenance") {
		t.Fatalf("origin not refused: %v", e)
	}
}
