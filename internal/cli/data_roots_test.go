package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type inspectionRunnerFunc func(context.Context, inspectionGitCommand) ([]byte, []byte, error)

func (f inspectionRunnerFunc) run(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
	return f(ctx, c)
}

func inspectionTestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	binary, err := inspectionSystemGit()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, append([]string{"--no-replace-objects", "-C", root}, args...)...)
	command.Env = append(inspectionEnvironment(), "GIT_AUTHOR_NAME=A1", "GIT_AUTHOR_EMAIL=a1@example.invalid", "GIT_COMMITTER_NAME=A1", "GIT_COMMITTER_EMAIL=a1@example.invalid")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture Git %v: %v: %s", args, err, out)
	}
	return strings.TrimSuffix(string(out), "\n")
}
func inspectionWrite(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func inspectionCommit(t *testing.T, root string) string {
	t.Helper()
	inspectionTestGit(t, root, "add", "-A")
	inspectionTestGit(t, root, "commit", "-qm", "fixture")
	return inspectionTestGit(t, root, "rev-parse", "HEAD")
}
func inspectionFixture(t *testing.T, code, data bool, origin string) string {
	t.Helper()
	root := t.TempDir()
	inspectionTestGit(t, root, "init", "-q")
	inspectionTestGit(t, root, "config", "remote.origin.url", origin)
	if code {
		inspectionWrite(t, root, "go.mod", "/* marker */\n module github.com/iryzhkov/UpKeeper // original\n\ngo 1.24\n")
		inspectionWrite(t, root, "cmd/upkeeper/main.go", "package main\n")
	}
	if data {
		for _, path := range []string{"inventory/hosts.yml", "desired/components.yml", "desired/release.json"} {
			inspectionWrite(t, root, path, "{}\n")
		}
	}
	inspectionWrite(t, root, "fixture-id", filepath.Base(root))
	inspectionCommit(t, root)
	return root
}

// Fingerprint every file (including config, index, refs/replace and reflogs) and
// symlink in the disposable fixture, not merely the worktree's Git status.
func inspectionState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			state[rel] = info.Mode().String()
			return nil
		}
		var content []byte
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			content = []byte(target)
		} else {
			content, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		state[rel] = fmt.Sprintf("%s:%x", info.Mode(), sha256.Sum256(content))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func inspectionRead(t *testing.T, code, data string, runner inspectionGitRunner, pass bool) rootPairObservation {
	t.Helper()
	beforeCode, beforeData := inspectionState(t, code), inspectionState(t, data)
	result, err := inspectRootPair(context.Background(), code, data, runner)
	if (err == nil) != pass {
		t.Fatalf("pass=%v result=%+v err=%v", pass, result, err)
	}
	if err != nil && result != (rootPairObservation{}) {
		t.Fatalf("error returned partial observation: %+v", result)
	}
	if !reflect.DeepEqual(beforeCode, inspectionState(t, code)) || !reflect.DeepEqual(beforeData, inspectionState(t, data)) {
		t.Fatal("inspection changed fixture bytes/modes/config/index/refs")
	}
	return result
}
func inspectionReal(t *testing.T) inspectionExecRunner {
	t.Helper()
	binary, err := inspectionSystemGit()
	if err != nil {
		t.Fatal(err)
	}
	return inspectionExecRunner{executable: binary}
}

func TestInspectionRootPairs(t *testing.T) {
	code := inspectionFixture(t, true, false, "git@code.example:Exact/Code.git")
	data := inspectionFixture(t, false, true, "https://data.example/Case/Data.git")
	result := inspectionRead(t, code, data, nil, true)
	if result.Mode != "split" || result.Code.HEAD == result.Data.HEAD ||
		result.Code.ObservedOrigin != "git@code.example:Exact/Code.git" || result.Data.ObservedOrigin != "https://data.example/Case/Data.git" ||
		result.Qualification != "unqualified" || result.MarkerSource != "selected_commit" || result.WorktreeQualification != "not_checked" {
		t.Fatalf("incorrect observations: %+v", result)
	}
	for _, root := range []inspectedRoot{result.Code, result.Data} {
		if root.HEAD != inspectionTestGit(t, root.Path, "rev-parse", "HEAD") || root.Tree != inspectionTestGit(t, root.Path, "rev-parse", root.HEAD+"^{tree}") {
			t.Fatal("selected commit/tree mismatch")
		}
	}
	both := inspectionFixture(t, true, true, "git@same.example:Repo")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(both, alias); err != nil {
		t.Fatal(err)
	}
	result = inspectionRead(t, both, alias, nil, true)
	if result.Mode != "colocated" || result.Code.Path != result.Data.Path {
		t.Fatal("alias not canonicalized")
	}
	worktree := filepath.Join(t.TempDir(), "linked")
	inspectionTestGit(t, both, "worktree", "add", "-q", "--detach", worktree)
	result = inspectionRead(t, both, worktree, nil, true)
	if result.Mode != "split" || result.Code.HEAD != result.Data.HEAD {
		t.Fatal("linked worktree observation")
	}
	clone := filepath.Join(t.TempDir(), "clone")
	inspectionTestGit(t, both, "clone", "-q", both, clone)
	result = inspectionRead(t, both, clone, nil, true)
	if result.Mode != "split" || result.Code.HEAD != result.Data.HEAD {
		t.Fatal("equal-content distinct checkout must stay split")
	}
}

func TestInspectionInvalidRoots(t *testing.T) {
	code := inspectionFixture(t, true, false, "git@code.example:Repo")
	data := inspectionFixture(t, false, true, "git@data.example:Repo")
	inspectionRead(t, data, code, nil, false)
	bare := t.TempDir()
	inspectionTestGit(t, bare, "init", "--bare", "-q")
	fake := t.TempDir()
	if err := os.Mkdir(filepath.Join(fake, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", "relative", filepath.Join(code, "cmd"), bare, fake, file, filepath.Join(t.TempDir(), "absent")} {
		result, err := inspectRootPair(context.Background(), root, data, nil)
		if err == nil || result != (rootPairObservation{}) {
			t.Fatalf("accepted invalid root %q", root)
		}
	}
	inspectionTestGit(t, code, "symbolic-ref", "HEAD", "refs/heads/unborn")
	inspectionRead(t, code, data, nil, false)
}

func TestInspectionCommittedMarkers(t *testing.T) {
	data := inspectionFixture(t, false, true, "git@data.example:Repo")
	t.Run("dirty staged deleted sparse markers are observations of HEAD", func(t *testing.T) {
		code := inspectionFixture(t, true, false, "git@code.example:Repo")
		inspectionWrite(t, code, "go.mod", "module wrong.invalid\n")
		inspectionTestGit(t, code, "add", "go.mod")
		if err := os.Remove(filepath.Join(code, "cmd/upkeeper/main.go")); err != nil {
			t.Fatal(err)
		}
		inspectionTestGit(t, code, "add", "-u")
		inspectionRead(t, code, data, nil, true)
		inspectionTestGit(t, code, "reset", "--hard", "-q")
		inspectionTestGit(t, code, "sparse-checkout", "init", "--cone")
		inspectionTestGit(t, code, "sparse-checkout", "set", "inventory")
		if _, err := os.Stat(filepath.Join(code, "cmd/upkeeper/main.go")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("fixture is not sparse")
		}
		inspectionRead(t, code, data, nil, true)
	})
	for _, module := range []string{"module wrong.invalid\n", "go 1.24\n", "module github.com/iryzhkov/UpKeeper\nmodule github.com/iryzhkov/UpKeeper\n",
		"module github.com/iryzhkov/UpKeeper extra\n", "require (\nmodule github.com/iryzhkov/UpKeeper\n)\n"} {
		t.Run(module, func(t *testing.T) {
			code := inspectionFixture(t, true, false, "git@code.example:Repo")
			inspectionWrite(t, code, "go.mod", module)
			inspectionCommit(t, code)
			inspectionWrite(t, code, "go.mod", "module github.com/iryzhkov/UpKeeper\n")
			inspectionRead(t, code, data, nil, false)
		})
	}
	for _, marker := range []string{"go.mod", "cmd/upkeeper/main.go"} {
		t.Run("untracked-"+marker, func(t *testing.T) {
			code := inspectionFixture(t, true, false, "git@code.example:Repo")
			inspectionTestGit(t, code, "rm", "--cached", marker)
			inspectionTestGit(t, code, "commit", "-qm", "remove marker")
			inspectionRead(t, code, data, nil, false)
			inspectionTestGit(t, code, "add", marker)
			inspectionRead(t, code, data, nil, false)
		})
		t.Run("symlink-"+marker, func(t *testing.T) {
			code := inspectionFixture(t, true, false, "git@code.example:Repo")
			if err := os.Remove(filepath.Join(code, marker)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("fixture-id", filepath.Join(code, marker)); err != nil {
				t.Fatal(err)
			}
			inspectionCommit(t, code)
			inspectionRead(t, code, data, nil, false)
		})
	}
	t.Run("submodule-marker", func(t *testing.T) {
		code := inspectionFixture(t, true, false, "git@code.example:Repo")
		head := inspectionTestGit(t, code, "rev-parse", "HEAD")
		inspectionTestGit(t, code, "update-index", "--add", "--cacheinfo", "160000", head, "cmd/upkeeper/main.go")
		inspectionTestGit(t, code, "commit", "-qm", "gitlink")
		inspectionRead(t, code, data, nil, false)
	})
	for _, marker := range []string{"inventory/hosts.yml", "desired/components.yml", "desired/release.json"} {
		t.Run("data-"+marker, func(t *testing.T) {
			code := inspectionFixture(t, true, false, "git@code.example:Repo")
			missing := inspectionFixture(t, false, true, "git@data.example:Repo")
			inspectionTestGit(t, missing, "rm", "--cached", marker)
			inspectionTestGit(t, missing, "commit", "-qm", "remove")
			inspectionRead(t, code, missing, nil, false)
		})
	}
	t.Run("oversized-real-blob", func(t *testing.T) {
		code := inspectionFixture(t, true, false, "git@code.example:Repo")
		inspectionWrite(t, code, "go.mod", "module github.com/iryzhkov/UpKeeper\n"+strings.Repeat(" ", inspectionOutputLimit))
		inspectionCommit(t, code)
		inspectionRead(t, code, data, nil, false)
	})
}

func TestInspectionOrigins(t *testing.T) {
	code := inspectionFixture(t, true, false, "git@code.example:Repo")
	data := inspectionFixture(t, false, true, "git@data.example:Repo")
	for _, origin := range []string{"https://user:SUPERSECRET@example.invalid/repo", "https://example.invalid/repo?token=SUPERSECRET", "https://example.invalid/repo#SUPERSECRET", "git:SUPERSECRET@example.invalid:repo", "one\ntwo", "a\x01b", ""} {
		inspectionTestGit(t, data, "config", "remote.origin.url", origin)
		result, err := inspectRootPair(context.Background(), code, data, nil)
		if err == nil || result != (rootPairObservation{}) || strings.Contains(err.Error(), "SUPERSECRET") || strings.Contains(err.Error(), origin) && origin != "" {
			t.Fatalf("origin leaked or accepted: %v", err)
		}
	}
	inspectionTestGit(t, data, "config", "--unset-all", "remote.origin.url")
	inspectionRead(t, code, data, nil, false)
	inspectionTestGit(t, data, "config", "--add", "remote.origin.url", "git@data.example:Repo")
	inspectionTestGit(t, data, "config", "--add", "remote.origin.url", "git@data.example:Other")
	inspectionRead(t, code, data, nil, false)
	inspectionTestGit(t, data, "config", "--unset-all", "remote.origin.url")
	inspectionTestGit(t, data, "config", "remote.origin.url", "https://EXAMPLE.invalid/Repo.git")
	inspectionTestGit(t, code, "config", "remote.origin.url", "https://example.invalid/Repo")
	result := inspectionRead(t, code, data, nil, true)
	if result.Code.ObservedOrigin == result.Data.ObservedOrigin || result.Data.ObservedOrigin != "https://EXAMPLE.invalid/Repo.git" {
		t.Fatal("origin normalized")
	}
}

func TestInspectionIsolationAndNoRetrieval(t *testing.T) {
	code := inspectionFixture(t, true, false, "git@code.example:Repo")
	data := inspectionFixture(t, false, true, "git@data.example:Repo")
	foreign := inspectionFixture(t, false, false, "git@foreign.example:Repo")
	home := t.TempDir()
	invocation := filepath.Join(t.TempDir(), "invoked")
	inspectionWrite(t, home, ".gitconfig", "[include]\n path = "+filepath.Join(foreign, ".git/config")+"\n[core]\n fsmonitor = "+invocation+"\n")
	fakebin := t.TempDir()
	inspectionWrite(t, fakebin, "git", "#!/bin/sh\necho invoked > '"+invocation+"'\nexit 99\n")
	if err := os.Chmod(filepath.Join(fakebin, "git"), 0700); err != nil {
		t.Fatal(err)
	}
	// Set these AFTER fixture setup, whose helper intentionally uses clean env.
	for key, value := range map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": home, "PATH": fakebin, "GIT_DIR": filepath.Join(foreign, ".git"),
		"GIT_WORK_TREE": foreign, "GIT_COMMON_DIR": filepath.Join(foreign, ".git"), "GIT_INDEX_FILE": filepath.Join(foreign, ".git/index"),
		"GIT_OBJECT_DIRECTORY": filepath.Join(foreign, ".git/objects"), "GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(foreign, ".git/objects"),
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "remote.origin.url", "GIT_CONFIG_VALUE_0": "SUPERSECRET",
		"GIT_CONFIG_PARAMETERS": "'remote.origin.url=SUPERSECRET'", "GIT_CONFIG_GLOBAL": filepath.Join(home, ".gitconfig"),
		"GIT_SSH_COMMAND": invocation, "HTTPS_PROXY": "http://invalid:1", "GIT_NO_REPLACE_OBJECTS": "0",
	} {
		t.Setenv(key, value)
	}
	inspectionRead(t, code, data, nil, true)
	if _, err := os.Stat(invocation); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("planted helper invoked")
	}
	for _, setting := range []struct{ key, value string }{{"extensions.partialClone", "origin"}, {"remote.origin.promisor", "true"}, {"remote.other.promisor", "yes"}, {"remote.origin.promisor", "nonsense"}} {
		inspectionTestGit(t, data, "config", setting.key, setting.value)
		real := inspectionReal(t)
		objectReads := 0
		wrapper := inspectionRunnerFunc(func(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
			if c.root == data && (c.operation == inspectionTree || c.operation >= inspectionMarkers) {
				objectReads++
			}
			return real.run(ctx, c)
		})
		inspectionRead(t, code, data, wrapper, false)
		if objectReads != 0 {
			t.Fatal("object read preceded partial/promisor refusal")
		}
		inspectionTestGit(t, data, "config", "--unset-all", setting.key)
	}
	// Real missing loose blob; an external remote helper is planted but cannot run.
	blob := inspectionTestGit(t, data, "rev-parse", "HEAD:desired/release.json")
	if err := os.Remove(filepath.Join(data, ".git/objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	inspectionTestGit(t, data, "config", "remote.origin.url", "ext::"+filepath.Join(fakebin, "git"))
	inspectionRead(t, code, data, nil, false)
	inspectionTestGit(t, data, "config", "remote.origin.promisor", "true")
	inspectionRead(t, code, data, nil, false)
	if _, err := os.Stat(invocation); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retrieval/helper ran")
	}
}

func TestInspectionOriginalObjects(t *testing.T) {
	for _, codeSide := range []bool{true, false} {
		for _, goodOriginal := range []bool{true, false} {
			t.Run(fmt.Sprintf("code=%v-good=%v", codeSide, goodOriginal), func(t *testing.T) {
				code := inspectionFixture(t, true, false, "git@code.example:Repo")
				data := inspectionFixture(t, false, true, "git@data.example:Repo")
				root, marker := data, "desired/release.json"
				if codeSide {
					root, marker = code, "go.mod"
				}
				original := inspectionTestGit(t, root, "rev-parse", "HEAD")
				if !goodOriginal {
					if codeSide {
						inspectionWrite(t, root, marker, "module wrong.invalid\n")
						inspectionCommit(t, root)
					} else {
						inspectionTestGit(t, root, "rm", marker)
						inspectionTestGit(t, root, "commit", "-qm", "missing")
					}
					original = inspectionTestGit(t, root, "rev-parse", "HEAD")
				}
				// Replacement commit has the opposite membership/module acceptance.
				if goodOriginal {
					inspectionTestGit(t, root, "rm", marker)
				} else if codeSide {
					inspectionWrite(t, root, marker, "module github.com/iryzhkov/UpKeeper\n")
				} else {
					inspectionWrite(t, root, marker, "{}\n")
				}
				replacement := inspectionCommit(t, root)
				inspectionTestGit(t, root, "reset", "--hard", "-q", original)
				inspectionTestGit(t, root, "replace", original, replacement)
				result := inspectionRead(t, code, data, nil, goodOriginal)
				if goodOriginal {
					observed := result.Data
					if codeSide {
						observed = result.Code
					}
					if observed.Tree != inspectionTestGit(t, root, "rev-parse", original+"^{tree}") {
						t.Fatal("reported replacement tree")
					}
				}
			})
		}
	}
	for _, goodOriginal := range []bool{true, false} {
		t.Run(fmt.Sprintf("blob-good=%v", goodOriginal), func(t *testing.T) {
			code := inspectionFixture(t, true, false, "git@code.example:Repo")
			data := inspectionFixture(t, false, true, "git@data.example:Repo")
			if !goodOriginal {
				inspectionWrite(t, code, "go.mod", "module wrong.invalid\n")
				inspectionCommit(t, code)
			}
			originalBlob := inspectionTestGit(t, code, "rev-parse", "HEAD:go.mod")
			original := inspectionTestGit(t, code, "rev-parse", "HEAD")
			next := "module wrong.invalid\n"
			if !goodOriginal {
				next = "module github.com/iryzhkov/UpKeeper\n"
			}
			inspectionWrite(t, code, "go.mod", next)
			inspectionCommit(t, code)
			replacement := inspectionTestGit(t, code, "rev-parse", "HEAD:go.mod")
			inspectionTestGit(t, code, "reset", "--hard", "-q", original)
			inspectionTestGit(t, code, "replace", originalBlob, replacement)
			inspectionRead(t, code, data, nil, goodOriginal)
		})
	}
	t.Run("data-blob-replacement-cannot-mask-oversize", func(t *testing.T) {
		code := inspectionFixture(t, true, false, "git@code.example:Repo")
		data := inspectionFixture(t, false, true, "git@data.example:Repo")
		good := inspectionTestGit(t, data, "rev-parse", "HEAD:desired/release.json")
		inspectionWrite(t, data, "desired/release.json", strings.Repeat("x", inspectionOutputLimit+1))
		inspectionCommit(t, data)
		large := inspectionTestGit(t, data, "rev-parse", "HEAD:desired/release.json")
		inspectionTestGit(t, data, "replace", large, good)
		inspectionRead(t, code, data, nil, false)
		inspectionTestGit(t, data, "replace", "-d", large)
		inspectionTestGit(t, data, "reset", "--hard", "-q", "HEAD~1")
		inspectionTestGit(t, data, "replace", good, large)
		inspectionRead(t, code, data, nil, true)
	})
}

func TestInspectionTypedPolicyAndFailures(t *testing.T) {
	code := inspectionFixture(t, true, false, "git@code.example:Repo")
	data := inspectionFixture(t, false, true, "git@data.example:Repo")
	real := inspectionReal(t)
	roots := map[string]bool{}
	policy := inspectionRunnerFunc(func(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
		args, err := c.argv()
		if err != nil {
			t.Fatal(err)
		}
		if len(args) < 7 || args[0] != "--no-replace-objects" || args[1] != "--no-pager" || args[2] != "-c" || args[3] != "core.fsmonitor=false" || args[4] != "-C" || args[5] != c.root {
			t.Fatal("missing mandatory argv policy")
		}
		roots[c.root] = true
		return real.run(ctx, c)
	})
	inspectionRead(t, code, data, policy, true)
	if len(roots) != 2 {
		t.Fatal("policy did not cover both roots")
	}
	for _, c := range []inspectionGitCommand{{root: code, operation: 255}, {root: code, operation: inspectionBlob, object: "HEAD"}, {root: "relative", operation: inspectionHEAD}} {
		if _, err := c.argv(); err == nil {
			t.Fatal("untyped command accepted")
		}
	}
	for _, tc := range []struct {
		name   string
		op     inspectionGitOperation
		value  string
		stderr bool
		err    error
	}{
		{"malformed-head", inspectionHEAD, "oops\n", false, nil},
		{"nonhex-head", inspectionHEAD, strings.Repeat("z", 40) + "\n", false, nil},
		{"bad-tree", inspectionTree, "oops\n", false, nil},
		{"wrong-type", inspectionType, "tag\n", false, nil},
		{"negative-size", inspectionSize, "-1\n", false, nil},
		{"large-size", inspectionSize, "65537\n", false, nil},
		{"malformed-size", inspectionSize, "01\n", false, nil},
		{"wrong-hash", inspectionBlob, "corrupt", false, nil},
		{"wrong-size", inspectionBlob, "", false, nil},
		{"large-stdout", inspectionHEAD, strings.Repeat("x", inspectionOutputLimit+1), false, nil},
		{"large-stderr", inspectionHEAD, strings.Repeat("x", inspectionOutputLimit+1), true, nil},
		{"git-error-redacted", inspectionHEAD, "SUPERSECRET", true, errors.New("SUPERSECRET")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapper := inspectionRunnerFunc(func(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
				if c.operation == tc.op {
					if tc.stderr {
						return nil, []byte(tc.value), tc.err
					}
					return []byte(tc.value), nil, tc.err
				}
				return real.run(ctx, c)
			})
			result, err := inspectRootPair(context.Background(), code, data, wrapper)
			if err == nil || result != (rootPairObservation{}) || strings.Contains(err.Error(), "SUPERSECRET") {
				t.Fatalf("invalid refusal: %v %+v", err, result)
			}
		})
	}
}

func TestInspectionHEADMovement(t *testing.T) {
	code := inspectionFixture(t, true, false, "git@code.example:Repo")
	data := inspectionFixture(t, false, true, "git@data.example:Repo")
	first := inspectionTestGit(t, code, "rev-parse", "HEAD")
	inspectionWrite(t, code, "movement", "next")
	second := inspectionCommit(t, code)
	inspectionTestGit(t, code, "reset", "--hard", "-q", first)
	real := inspectionReal(t)
	moved := false
	wrapper := inspectionRunnerFunc(func(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
		if c.root == data && c.operation == inspectionMarkers && !moved {
			moved = true
			inspectionTestGit(t, code, "update-ref", "HEAD", second)
		}
		return real.run(ctx, c)
	})
	result, err := inspectRootPair(context.Background(), code, data, wrapper)
	if !moved || err == nil || result != (rootPairObservation{}) || !strings.Contains(err.Error(), "HEAD moved") {
		t.Fatalf("movement not refused: %v %+v", err, result)
	}
}

func TestInspectionCancellationAndCumulativeBudget(t *testing.T) {
	code := inspectionFixture(t, true, false, "git@code.example:Repo")
	data := inspectionFixture(t, false, true, "git@data.example:Repo")
	calls := 0
	runner := inspectionRunnerFunc(func(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
		calls++
		return nil, nil, errors.New("unexpected")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := inspectRootPair(ctx, code, data, runner)
	if calls != 0 || !errors.Is(err, context.Canceled) || result != (rootPairObservation{}) {
		t.Fatal("canceled caller launched")
	}
	real := inspectionReal(t)
	var active atomic.Int32
	calls = 0
	started := time.Now()
	ctx, cancel = context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	runner = inspectionRunnerFunc(func(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
		calls++
		got, ok := ctx.Deadline()
		if !ok || got.After(deadline) {
			t.Fatal("caller deadline renewed")
		}
		active.Add(1)
		defer active.Add(-1)
		timer := time.NewTimer(65 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
		return real.run(ctx, c)
	})
	result, err = inspectRootPair(ctx, code, data, runner)
	if calls < 2 || !errors.Is(err, context.DeadlineExceeded) || result != (rootPairObservation{}) || active.Load() != 0 || time.Since(started) > time.Second {
		t.Fatalf("cumulative budget: calls=%d err=%v", calls, err)
	}
	// The inspector's own whole-operation budget is shared by every command even
	// without a caller deadline; a short caller test above proves actual expiry.
	var parentDeadline time.Time
	runner = inspectionRunnerFunc(func(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
		d, _ := ctx.Deadline()
		if time.Until(d) > 5*time.Second {
			t.Fatal("child budget exceeds five seconds")
		}
		if parentDeadline.IsZero() {
			parentDeadline = time.Now().Add(15 * time.Second)
		}
		if d.After(parentDeadline) {
			t.Fatal("child exceeds whole budget")
		}
		return real.run(ctx, c)
	})
	inspectionRead(t, code, data, runner, true)
}

// Exercise expiry of the real fixed whole budget, not only an earlier caller deadline.
func TestInspectionWholeBudget(t *testing.T) {
	code := inspectionFixture(t, true, false, "git@code.example:Repo")
	data := inspectionFixture(t, false, true, "git@data.example:Repo")
	real := inspectionReal(t)
	start := time.Now()
	calls := 0
	active := 0
	clipped := false
	runner := inspectionRunnerFunc(func(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
		calls++
		active++
		defer func() { active-- }()
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("unbounded child")
		}
		if time.Since(start) > 11*time.Second {
			clipped = true
			if deadline.After(start.Add(15*time.Second + 100*time.Millisecond)) {
				t.Fatal("whole budget renewed")
			}
		}
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
		return real.run(ctx, c)
	})
	result, err := inspectRootPair(context.Background(), code, data, runner)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || result != (rootPairObservation{}) || calls < 12 || !clipped || active != 0 || elapsed < 14*time.Second || elapsed > 17*time.Second {
		t.Fatalf("whole budget: elapsed=%v calls=%d active=%d clipped=%v err=%v", elapsed, calls, active, clipped, err)
	}
}

func TestInspectionProcessHelper(t *testing.T) {
	if len(os.Args) < 2 || !strings.HasPrefix(os.Args[len(os.Args)-1], "a1-helper:") {
		return
	}
	file := strings.TrimPrefix(os.Args[len(os.Args)-1], "a1-helper:")
	if err := os.WriteFile(file, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(91)
	}
	for {
		time.Sleep(time.Second)
	}
}
func inspectionShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func TestInspectionRealAdapterCancellation(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("PID/reaping assertion requires Linux procfs")
	}
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "owned-helper")
	inspectionWrite(t, dir, "owned-helper", "#!/bin/sh\nexec "+inspectionShellQuote(executable)+" -test.run='^TestInspectionProcessHelper$' -- "+inspectionShellQuote("a1-helper:"+pidfile)+"\n")
	if err := os.Chmod(helper, 0700); err != nil {
		t.Fatal(err)
	}
	code := inspectionFixture(t, true, false, "git@code.example:Repo")
	data := inspectionFixture(t, false, true, "git@data.example:Repo")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var result rootPairObservation
	var runErr error
	go func() {
		defer close(done)
		result, runErr = inspectRootPair(ctx, code, data, inspectionExecRunner{executable: helper})
	}()
	var pid int
	timeout := time.After(5 * time.Second)
ready:
	for {
		if raw, err := os.ReadFile(pidfile); err == nil {
			pid, err = strconv.Atoi(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			break ready
		}
		select {
		case <-done:
			t.Fatalf("helper ended before cancellation: %v", runErr)
		case <-timeout:
			cancel()
			<-done
			t.Fatal("helper did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("helper not alive before cancellation")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("adapter did not synchronously complete")
	}
	if !errors.Is(runErr, context.Canceled) || result != (rootPairObservation{}) {
		t.Fatalf("real cancellation: %v %+v", runErr, result)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("PID still exists: %v", err)
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("direct helper not reaped: %v", err)
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("helper still active")
	}
	if err := os.Remove(pidfile); err != nil {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	_, _, err = (inspectionExecRunner{executable: helper}).run(canceled, inspectionGitCommand{root: code, operation: inspectionTop})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(pidfile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("already canceled adapter launched")
	}
}

func TestInspectionRealOutputCaps(t *testing.T) {
	dir := t.TempDir()
	for _, stderr := range []bool{false, true} {
		suffix := ""
		if stderr {
			suffix = " >&2"
		}
		helper := filepath.Join(dir, "output-helper")
		inspectionWrite(t, dir, "output-helper", "#!/bin/sh\nexec /usr/bin/yes x"+suffix+"\n")
		if err := os.Chmod(helper, 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, errout, err := (inspectionExecRunner{executable: helper}).run(ctx, inspectionGitCommand{root: dir, operation: inspectionTop})
		cancel()
		if err == nil || len(out) > inspectionOutputLimit || len(errout) > inspectionOutputLimit || !strings.Contains(err.Error(), "output limit") {
			t.Fatalf("actual process cap: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	runner := inspectionRunnerFunc(func(ctx context.Context, c inspectionGitCommand) ([]byte, []byte, error) {
		cancel()
		return bytes.Repeat([]byte("x"), inspectionOutputLimit+1), nil, errors.New("raw")
	})
	_, err := inspectionRun(canceled, runner, inspectionGitCommand{root: dir, operation: inspectionTop})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cap obscured cancellation")
	}
}
