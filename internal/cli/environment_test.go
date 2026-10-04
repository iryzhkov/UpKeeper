//go:build linux

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func environmentCandidate(t *testing.T) ([]string, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("DEV_FLEET_CONFIG_ROOT", filepath.Join(home, "must-not-resolve"))
	t.Setenv("PATH", "")
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if e := os.Mkdir(root, 0755); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile("../toolcatalog/testdata/synthetic.json")
	if e != nil {
		t.Fatal(e)
	}
	payload, e := os.ReadFile("../toolcatalog/testdata/payload.txt")
	if e != nil {
		t.Fatal(e)
	}
	meta := filepath.Join(dir, "metadata.json")
	if e := os.WriteFile(meta, raw, 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "payload.txt"), payload, 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Join(root, "payload.txt"), 0644); e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(raw)
	return []string{"environment", "tools", "--metadata", meta, "--root", root, "--expect-sha256", hex.EncodeToString(h[:])}, meta, home
}
func TestEnvironmentSyntaxAndHelpBeforeInput(t *testing.T) {
	argv, meta, _ := environmentCandidate(t)
	spool := useSpool(t)
	if e := os.Remove(meta); e != nil {
		t.Fatal(e)
	}
	if e := syscall.Mkfifo(meta, 0600); e != nil {
		t.Fatal(e)
	}
	cases := [][]string{
		{"environment"}, {"environment", "unknown", "--help"}, {"environment", "--help", "tail"},
		{"environment", "tools", "--help", "--json"},
		append(append([]string{}, argv...), "--help"),
		append(append([]string{}, argv...), "--json", "--json"),
		append(append([]string{}, argv...), "--platform", "debian"),
		append(append([]string{}, argv...), "tail"),
		append(append([]string{}, argv...), "--root", filepath.Dir(meta)),
		{"environment", "tools", "--metadata=" + meta},
		{"environment", "tools", "--metadata", meta, "--root", "relative", "--expect-sha256", strings.Repeat("0", 64)},
		{"environment", "tools", "--metadata", meta, "--root", "/", "--expect-sha256", strings.Repeat("A", 64)},
		append(append([]string{}, argv...), "--platform", ""),
		append(append([]string{}, argv...), "--j"),
	}
	for _, a := range cases {
		code, out, errOut := runSpooled(a)
		if code != 2 || out != "" || !strings.HasPrefix(errOut, "usage: upkeeper COMMAND") {
			t.Fatalf("%v: %d %q %q", a, code, out, errOut)
		}
		if strings.Contains(errOut, meta) {
			t.Fatal("usage leaked caller path")
		}
	}
	for _, a := range [][]string{{"environment", "--help"}, {"environment", "tools", "--help"}, {"environment", "tools", "-h"}, {"--help"}} {
		code, out, _ := runSpooled(a)
		if code != 0 || out == "" {
			t.Fatal(code, out)
		}
		if a[0] == "environment" && len(strings.Split(out, "\n")) > 40 {
			t.Fatal("help exceeds bound")
		}
	}
	// Root help keeps its previous spool policy; clear its record is unnecessary:
	// inspect the namespace first with a separate spool below.
	entries, e := os.ReadDir(spool)
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) == 0 {
		t.Fatal("root help unexpectedly bypassed existing spool policy")
	}
}
func TestEnvironmentNoEffectsAndExistingSpool(t *testing.T) {
	argv, _, home := environmentCandidate(t)
	spool := useSpool(t)
	for _, a := range [][]string{argv, append(append([]string{}, argv...), "--json"), {"environment", "tools", "--help"}, {"environment", "bad"}, {"environment", "tools", "--metadata", "/absent", "--root", "/absent", "--expect-sha256", strings.Repeat("0", 64), "--json"}} {
		code, _, _ := runSpooled(a)
		if code != 0 && code != 2 && code != 30 {
			t.Fatal(code)
		}
		entries, e := os.ReadDir(spool)
		if e != nil || len(entries) != 0 {
			t.Fatal("environment wrote spool", entries, e)
		}
		entries, e = os.ReadDir(home)
		if e != nil || len(entries) != 0 {
			t.Fatal("environment changed disposable home", entries, e)
		}
	}
	code, _, _ := runSpooled([]string{"version"})
	if code != 0 || len(spoolFiles(t, spool)) == 0 {
		t.Fatal("version telemetry changed")
	}
}
func TestEnvironmentOutputTruthAndFailureNoLeaks(t *testing.T) {
	argv, meta, _ := environmentCandidate(t)
	for _, jsonMode := range []bool{false, true} {
		a := append([]string{}, argv...)
		a = append(a, "--platform", "linux-amd64")
		if jsonMode {
			a = append(a, "--json")
		}
		code, out, errOut := runSpooled(a)
		if code != 0 || errOut != "" {
			t.Fatal(code, out, errOut)
		}
		for _, want := range []string{"candidate-inert", "declaration-only", "unknown", "unqualified", "unassessed", "compatible", "incompatible"} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %s in %s", want, out)
			}
		}
		if strings.Contains(out, "synthetic offline payload") || strings.Contains(out, meta) {
			t.Fatal("content or path leak")
		}
		if jsonMode {
			var m map[string]any
			if e := json.Unmarshal([]byte(out), &m); e != nil {
				t.Fatal(e)
			}
			if m["exit_code"] != float64(0) || m["command"] != "environment-tools" {
				t.Fatal(m)
			}
		}
	}
	if e := os.WriteFile(meta, []byte("sk-live-supersecret-payload-0123456789"), 0644); e != nil {
		t.Fatal(e)
	}
	code, out, errOut := runSpooled(append(append([]string{}, argv...), "--json"))
	if code != 30 || errOut != "" || strings.Contains(out, "supersecret") || strings.Contains(out, meta) || strings.Contains(out, "purpose") || strings.Contains(out, "generation_sha256") {
		t.Fatal(code, out, errOut)
	}
	var m map[string]any
	if e := json.Unmarshal([]byte(out), &m); e != nil {
		t.Fatal(e)
	}
	detail := m["error"].(map[string]any)
	if len(detail) != 2 || detail["code"] == nil || detail["stage"] == nil {
		t.Fatal(detail)
	}
	code, out, errOut = runSpooled(argv)
	if code != 30 || out != "" || !strings.Contains(errOut, "catalog refused") || strings.Contains(errOut, meta) {
		t.Fatal(code, out, errOut)
	}
}

type brokenEnvironmentWriter struct{}

func (brokenEnvironmentWriter) Write([]byte) (int, error) {
	return 0, errors.New("SECRET output failure")
}
func TestEnvironmentOutputFailureIsOne(t *testing.T) {
	argv, _, _ := environmentCandidate(t)
	var errOut bytes.Buffer
	for _, a := range [][]string{argv, append(append([]string{}, argv...), "--json"), {"environment", "tools", "--help"}} {
		errOut.Reset()
		code := Main("upkeeper", a, Streams{Out: brokenEnvironmentWriter{}, Err: &errOut, In: strings.NewReader("")})
		if code != 1 || strings.Contains(errOut.String(), "SECRET") {
			t.Fatal(code, errOut.String())
		}
	}
}
func TestEnvironmentApprovedDescriptionRedaction(t *testing.T) {
	argv, meta, _ := environmentCandidate(t)
	b, e := os.ReadFile(meta)
	if e != nil {
		t.Fatal(e)
	}
	b = bytes.Replace(b, []byte("Synthetic CLI declaration"), []byte("token=supersecret"), 1)
	if e := os.WriteFile(meta, b, 0644); e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(b)
	argv[7] = hex.EncodeToString(h[:])
	for _, a := range [][]string{argv, append(append([]string{}, argv...), "--json")} {
		code, out, _ := runSpooled(a)
		if code != 0 || strings.Contains(out, "supersecret") {
			t.Fatal(code, out)
		}
	}
}
