package author_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/author"
)

func sharedFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	source := filepath.Join("..", "..", author.SharedDirectory)
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		write(t, filepath.Join(f.root, author.SharedDirectory, relative), string(raw), info.Mode().Perm())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.root, "remote", "add", "origin", "https://github.com/iryzhkov/UpKeeper.git")
	return f, f.commit(t, f.root, "shared source")
}

func TestSharedDeterministicScratchAndRefresh(t *testing.T) {
	f, commit := sharedFixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	write(t, filepath.Join(home, ".claude/CLAUDE.md"), "# This machine\npersonal\n", 0600)
	write(t, filepath.Join(home, ".codex/AGENTS.md"), "personal Codex\n", 0600)
	// SSH cannot be reached even if the preparation accidentally adds a transport.
	t.Setenv("SSH_AUTH_SOCK", "must-not-use")
	before, original := f.manifest(t)
	first, err := author.PrepareShared(f.root, commit)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Even the standalone generator's archive bytes are stable.
	generator := filepath.Join("..", "..", author.SharedDirectory, author.Generator)
	sourceRoot := filepath.Join("..", "..", author.SharedDirectory)
	export := func(mask string) []byte {
		cmd := exec.Command("bash", "-c", `umask "$1"; exec bash "$2" --scratch-only --root "$3"`, "export", mask, generator, sourceRoot)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	archive := export("022")
	for _, mask := range []string{"022", "077", "002"} {
		if !bytes.Equal(archive, export(mask)) {
			t.Fatalf("generator archive depends on caller umask %s", mask)
		}
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		want := int64(0644)
		if header.Typeflag == tar.TypeDir {
			want = 0755
		}
		if header.Mode != want {
			t.Fatalf("export mode for %s: %o, want %o", header.Name, header.Mode, want)
		}
	}
	// Working tree poison cannot enter derivation of the same immutable input.
	write(t, filepath.Join(f.root, author.SharedDirectory, "config/claude/CLAUDE.md"), "poison", 0644)
	second, err := author.PrepareShared(f.root, commit)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if !reflect.DeepEqual(first.OutputSHA256, second.OutputSHA256) || !reflect.DeepEqual(first.SourceSHA256, second.SourceSHA256) {
		t.Fatal("nondeterministic derivation")
	}
	if first.Commit != commit || len(first.SourceSHA256) != len(first.Owner.SourceFiles)+1 {
		t.Fatal("missing provenance")
	}
	for path, file := range first.Sources.Instructions {
		if strings.Contains(string(file.Content), "agent99") || strings.Contains(string(file.Content), "poison") {
			t.Fatalf("advertisement or host bytes at %s", path)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(home, ".claude/CLAUDE.md")); string(raw) != "# This machine\npersonal\n" {
		t.Fatal("home was modified")
	}
	if raw, _ := os.ReadFile(filepath.Join(home, ".codex/AGENTS.md")); string(raw) != "personal Codex\n" {
		t.Fatal("Codex preferences modified")
	}
	_, after := f.manifest(t)
	if !bytes.Equal(original, after) {
		t.Fatal("preparation changed desired release")
	}
	component := before["components"].(map[string]any)["agent-environment"].(map[string]any)
	refreshed, changes, err := agentenv.RefreshDerived(component, first.Sources, commit)
	if err != nil || len(changes) == 0 {
		t.Fatalf("scratch refresh: %v", err)
	}
	noOp, changes, err := agentenv.RefreshDerived(refreshed, second.Sources, commit)
	if err != nil || len(changes) != 0 || !reflect.DeepEqual(noOp, refreshed) {
		t.Fatalf("idempotence: %v", err)
	}
	previous, err := agentenv.DecodeComponent(component)
	if err != nil {
		t.Fatal(err)
	}
	next, err := agentenv.DecodeComponent(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range previous.Files {
		if strings.Contains(old.Path, "/skills/tdd/") || old.Path == ".claude/CLAUDE.md" {
			found := false
			for _, current := range next.Files {
				if current.Path == old.Path {
					found = reflect.DeepEqual(current, old)
				}
			}
			if !found {
				t.Fatalf("personal or unrelated projection changed: %s", old.Path)
			}
		}
	}
	// Dropped generated files get tombstones, without changing unowned preferences.
	delete(second.Sources.Instructions, ".config/agents/jocasta.md")
	retired, _, err := agentenv.RefreshDerived(refreshed, second.Sources, commit)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := agentenv.DecodeComponent(retired)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range decoded.RetiredFiles {
		if file.Path == ".config/agents/jocasta.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("managed retirement missing")
	}
}

func TestSharedIgnoresGitReplacementObjects(t *testing.T) {
	for _, kind := range []string{"blob", "commit"} {
		t.Run(kind, func(t *testing.T) {
			f, commit := sharedFixture(t)
			t.Setenv("GIT_NO_REPLACE_OBJECTS", "")
			if err := os.Unsetenv("GIT_NO_REPLACE_OBJECTS"); err != nil {
				t.Fatal(err)
			}
			before, err := author.PrepareShared(f.root, commit)
			if err != nil {
				t.Fatal(err)
			}
			defer before.Close()
			path := author.SharedDirectory + "/config/claude/CLAUDE.md"
			original := gitIn(t, f.root, "rev-parse", commit+":"+path)
			poison := "# Global agent instructions\nreplacement poison\n"
			write(t, filepath.Join(f.root, path), poison, 0644)
			replacement := f.commit(t, f.root, "replacement source")
			if kind == "blob" {
				replaced := gitIn(t, f.root, "rev-parse", replacement+":"+path)
				gitIn(t, f.root, "replace", original, replaced)
			} else {
				gitIn(t, f.root, "replace", commit, replacement)
			}
			// Establish that ordinary Git reads really are contaminated.
			if got := gitIn(t, f.root, "show", commit+":"+path); got != strings.TrimSpace(poison) {
				t.Fatalf("replacement fixture did not change source: %s", got)
			}
			after, err := author.PrepareShared(f.root, commit)
			if err != nil {
				t.Fatal(err)
			}
			defer after.Close()
			if after.Commit != commit || !reflect.DeepEqual(before.SourceSHA256, after.SourceSHA256) ||
				!reflect.DeepEqual(before.OutputSHA256, after.OutputSHA256) {
				t.Fatal("replacement objects changed pinned preparation")
			}
		})
	}
}

func TestSharedRefusesIdentityInventoryAndTransition(t *testing.T) {
	f, commit := sharedFixture(t)
	for _, ref := range []string{"main", commit[:12], strings.Repeat("0", 40)} {
		if p, err := author.PrepareShared(f.root, ref); err == nil {
			p.Close()
			t.Fatalf("accepted %s", ref)
		}
	}
	gitIn(t, f.root, "remote", "set-url", "origin", "https://example.invalid/UpKeeper.git")
	if p, err := author.PrepareShared(f.root, commit); err == nil {
		p.Close()
		t.Fatal("wrong repository accepted")
	}
	gitIn(t, f.root, "remote", "set-url", "origin", "https://github.com/iryzhkov/UpKeeper.git")
	descriptor := filepath.Join(f.root, author.SharedDirectory, "owner.json")
	original, err := os.ReadFile(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"active", "pending", "interrupted", "corrupt", "rollback"} {
		var owner map[string]any
		if err := json.Unmarshal(original, &owner); err != nil {
			t.Fatal(err)
		}
		owner["state"] = state
		raw, _ := json.Marshal(owner)
		write(t, descriptor, string(raw), 0644)
		bad := f.commit(t, f.root, state)
		if p, err := author.PrepareShared(f.root, bad); err == nil {
			p.Close()
			t.Fatal("transition accepted")
		}
		if author.RequireLegacyWriter(state) == nil {
			t.Fatal("future fence interface accepted transition")
		}
	}
	if author.RequireLegacyWriter("legacy") != nil {
		t.Fatal("old uncut host rejected")
	}
	write(t, descriptor, string(original), 0644)
	if err := os.Remove(filepath.Join(f.root, author.SharedDirectory, "config/claude/skills/t3-wait/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	missing := f.commit(t, f.root, "missing source")
	if p, err := author.PrepareShared(f.root, missing); err == nil {
		p.Close()
		t.Fatal("missing source accepted")
	}
	// Direct invocation cannot install, even with a complete source tree.
	cmd := exec.Command("bash", filepath.Join("..", "..", author.SharedDirectory, author.Generator), "--root", filepath.Join("..", "..", author.SharedDirectory))
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "requires --scratch-only") {
		t.Fatalf("direct writer not refused: %s", out)
	}
}

func TestSharedCurrentReleaseDoesNotSelectCandidate(t *testing.T) {
	for _, path := range []string{"desired/release.json", "desired/components.yml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(author.SharedDirectory)) || bytes.Contains(raw, []byte("upkeeper-shared-prepare")) {
			t.Fatal("active release selects inert preparation")
		}
	}
}
