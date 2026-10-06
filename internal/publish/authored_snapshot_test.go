package publish_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/publish"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

// Exercise the real temporary index and commit machinery, with no remotes.
func TestAuthoredNoncanonicalRealGit(t *testing.T) {
	for _, clean := range []bool{false, true} {
		name := "working-tree"
		if clean {
			name = "already-committed"
		}
		t.Run(name, func(t *testing.T) {
			for _, mutation := range []string{"none", "format", "semantic", "companion", "index", "branch", "head", "mode", "after-load-format"} {
				t.Run(mutation, func(t *testing.T) {
					if clean && mutation == "index" {
						t.Skip("no-change publication does not reconcile or write the index; index race fence is exercised with authored edits")
					}
					f, _, _ := prepareAuthoredFixture(t)
					base, err := release.Load(f.root)
					if err != nil {
						t.Fatal(err)
					}
					base.Component("agent-config")["files"] = []any{map[string]any{
						"path": ".codex/AGENTS.md", "store": "snapshot.txt",
						"sha256": "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
					}}
					components := base.Components()
					delete(components, "agent-environment")
					delete(components, "t3")
					delete(components, "worker-configuration")
					components["t3-steward"].(map[string]any)["installer"] = "t3-update"
					delete(components["omarchy-setup"].(map[string]any), "modules")
					if err := os.MkdirAll(filepath.Join(f.root, release.AgentConfigDir), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(f.root, release.AgentConfigDir, "snapshot.txt"), []byte("foo"), 0o644); err != nil {
						t.Fatal(err)
					}
					if _, err := release.Write(f.root, base); err != nil {
						t.Fatal(err)
					}
					canonical := mustRead(t, filepath.Join(f.root, release.File))
					if err := os.RemoveAll(filepath.Join(f.root, ".git")); err != nil {
						t.Fatal(err)
					}
					run := func(args ...string) string {
						t.Helper()
						r := procx.Run(procx.Command{Argv: append([]string{"git", "-C", f.root}, args...)})
						if !r.OK() {
							t.Fatalf("git %v: %s", args, r.Output())
						}
						return r.Stdout
					}
					run("init", "--quiet")
					run("config", "user.name", "UpKeeper Test")
					run("config", "user.email", "upkeeper@example.invalid")
					run("config", "gc.auto", "0")
					run("config", "maintenance.auto", "false")
					run("add", ".")
					run("commit", "--quiet", "-m", "baseline")
					var pretty bytes.Buffer
					if err := json.Indent(&pretty, canonical, "", "  "); err != nil {
						t.Fatal(err)
					}
					reviewed := append([]byte(" \n"), pretty.Bytes()...)
					reviewed = append(reviewed, '\n', '\n')
					path := filepath.Join(f.root, release.File)
					if err := os.WriteFile(path, reviewed, 0o644); err != nil {
						t.Fatal(err)
					}
					if clean {
						run("add", release.File)
						run("commit", "--quiet", "-m", "reviewed serialization")
					}
					head := strings.TrimSpace(run("rev-parse", "HEAD"))
					branch := strings.TrimSpace(run("symbolic-ref", "HEAD"))
					value, err := release.Load(f.root)
					if err != nil {
						t.Fatal(err)
					}
					digest := value.Digest()
					if err = os.WriteFile(filepath.Join(f.root, "unrelated"), []byte("staged\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					run("add", "unrelated")
					if err = os.WriteFile(filepath.Join(f.root, "unrelated"), []byte("working\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					originalIndex := mustRead(t, filepath.Join(f.root, ".git/index"))
					injected := false
					runner := func(c procx.Command) procx.Result {
						if c.Argv[0] != "git" {
							t.Fatalf("unexpected command: %v", c.Argv)
						}
						if c.Argv[3] == "push" {
							t.Fatal("test attempted remote push")
						}
						if mutation == "after-load-format" && !injected && c.Argv[3] == "show" && c.Argv[4] == head+":"+release.File {
							injected = true
							if err := os.WriteFile(path, canonical, 0o644); err != nil {
								t.Fatal(err)
							}
						}
						if c.Argv[3] == "add" && !injected {
							injected = true
							switch mutation {
							case "format":
								if err := os.WriteFile(path, canonical, 0o644); err != nil {
									t.Fatal(err)
								}
							case "semantic":
								value.Manifest["captured_from"] = "changed-during-add"
								if _, err := release.Write(f.root, value); err != nil {
									t.Fatal(err)
								}
							case "companion":
								store := value.Files()[0]["store"].(string)
								if err := os.WriteFile(filepath.Join(f.root, release.AgentConfigDir, store), []byte("mutated"), 0o644); err != nil {
									t.Fatal(err)
								}
							case "index":
								run("add", "unrelated")
							case "branch":
								run("symbolic-ref", "HEAD", "refs/heads/raced")
								run("update-ref", "refs/heads/raced", head)
							case "head":
								tree := strings.TrimSpace(run("rev-parse", "HEAD^{tree}"))
								next := strings.TrimSpace(run("commit-tree", tree, "-p", head, "-m", "race"))
								run("update-ref", branch, next, head)
							case "mode":
								if err := os.Chmod(path, 0o755); err != nil {
									t.Fatal(err)
								}
							}
						}
						return procx.Run(c)
					}
					options := f.options()
					options.Runner, options.Remotes, options.Push = runner, nil, false
					options.Authored, options.ExpectedHead, options.ExpectedReleaseSHA256 = true, head, digest
					report, err := publish.Release(f.root, options)
					if mutation != "none" {
						want := map[string]string{"after-load-format": "changed while staging", "format": "changed while staging", "semantic": "changed while staging", "companion": "companion file changed", "index": "index changed", "branch": "branch changed", "head": "HEAD changed", "mode": "requires a regular"}[mutation]
						if err == nil || !strings.Contains(err.Error(), want) {
							t.Fatalf("%s race: %v, want %q", mutation, err, want)
						}
						if mutation != "head" && mutation != "branch" && strings.TrimSpace(run("rev-parse", "HEAD")) != head {
							t.Fatal("refusal advanced HEAD")
						}
						if mutation != "index" && !bytes.Equal(mustRead(t, filepath.Join(f.root, ".git/index")), originalIndex) {
							t.Fatal("refusal changed index")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if report["release_sha256"] != digest {
						t.Fatal("canonical identity changed")
					}
					if !bytes.Equal([]byte(run("show", "HEAD:"+release.File)), reviewed) || !bytes.Equal(mustRead(t, path), reviewed) {
						t.Fatal("reviewed bytes or timestamp changed")
					}
					if clean && strings.TrimSpace(run("rev-parse", "HEAD")) != head {
						t.Fatal("clean publication advanced HEAD")
					}
					if strings.TrimSpace(run("show", ":unrelated")) != "staged" || string(mustRead(t, filepath.Join(f.root, "unrelated"))) != "working\n" {
						t.Fatal("unrelated state changed")
					}
				})
			}
		})
	}
}
