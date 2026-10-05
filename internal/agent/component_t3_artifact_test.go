package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
)

func t3AgentFixture(t *testing.T, target string, identity ...string) (*release.T3Artifact, []byte, []byte) {
	t.Helper()
	version := "0.0.45"
	s := &release.T3Artifact{Repository: "iryzhkov/t3code", Tag: "fleet-fixture", SourceCommit: strings.Repeat("a", 40), SourceTree: strings.Repeat("b", 40), Assets: map[string]release.T3Asset{}}
	if len(identity) > 0 {
		s.SourceCommit = identity[0]
		s.Tag = "fleet-fixture-b"
	}
	for _, p := range []string{"linux-x64", "linux-arm64"} {
		name := "t3-fleet-" + version + "-" + p
		s.Assets[p] = release.T3Asset{Asset: name + ".tar.gz", SHA256: strings.Repeat("c", 64), ReceiptAsset: name + ".receipt.json", ReceiptSHA256: strings.Repeat("d", 64)}
	}
	metadata := map[string]any{"schema": "upkeeper-t3-build/v1", "source_commit": s.SourceCommit, "source_tree": s.SourceTree, "platform": target, "base_version": version}
	pkg, _ := json.Marshal(map[string]any{"name": "t3", "version": version, "bin": map[string]any{"t3": "./bin/t3.js"}, "optionalDependencies": map[string]any{"@t3code/t3-" + target: version}, "upkeeperControlledBuild": metadata})
	native, _ := json.Marshal(map[string]any{"name": "@t3code/t3-" + target, "version": version, "os": []string{"linux"}, "cpu": []string{strings.TrimPrefix(target, "linux-")}})
	files := map[string][]byte{"package.json": pkg, "bin/t3.js": []byte("#!/usr/bin/env node\n"), "node_modules/@t3code/t3-" + target + "/package.json": native, "node_modules/@t3code/t3-" + target + "/t3": []byte("fixture-native-not-qualified-" + s.SourceCommit)}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if e := tw.WriteHeader(&tar.Header{Name: "t3/", Typeflag: tar.TypeDir, Mode: 0755}); e != nil {
		t.Fatal(e)
	}
	r := release.T3Receipt{Schema: "upkeeper-t3-receipt/v1", SourceCommit: s.SourceCommit, SourceTree: s.SourceTree, Platform: target, BaseVersion: version, Repository: s.Repository, Archive: s.Assets[target].Asset}
	// Stable fixture order independent of Go map iteration.
	for _, p := range []string{"package.json", "bin/t3.js", "node_modules/@t3code/t3-" + target + "/package.json", "node_modules/@t3code/t3-" + target + "/t3"} {
		b := files[p]
		mode := int64(0644)
		if strings.HasSuffix(p, "/t3") || p == "bin/t3.js" {
			mode = 0755
		}
		r.Files = append(r.Files, release.T3File{Path: p, SHA256: release.T3Digest(b), Size: int64(len(b)), Mode: mode})
		if e := tw.WriteHeader(&tar.Header{Name: "t3/" + p, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(b))}); e != nil {
			t.Fatal(e)
		}
		if _, e := tw.Write(b); e != nil {
			t.Fatal(e)
		}
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	if e := gz.Close(); e != nil {
		t.Fatal(e)
	}
	r.ArchiveSHA256 = release.T3Digest(archive.Bytes())
	body, _ := json.Marshal(r)
	a := s.Assets[target]
	a.SHA256 = r.ArchiveSHA256
	a.ReceiptSHA256 = release.T3Digest(body)
	s.Assets[target] = a
	return s, archive.Bytes(), body
}

func t3TxPrepare(t *testing.T, existing bool) (*Context, *t3Transaction, string, string) {
	t.Helper()
	c := adapterTestContext(t, &adapterTestRunner{})
	prefix := c.Path(".local/t3-prefix")
	for _, p := range []string{filepath.Join(prefix, "bin"), filepath.Join(prefix, "lib/node_modules")} {
		if e := os.MkdirAll(p, 0755); e != nil {
			t.Fatal(e)
		}
	}
	if existing {
		p := filepath.Join(prefix, "lib/node_modules/t3/bin/t3.js")
		adapterTestWrite(t, p, "previous-exact")
		if e := os.Chmod(p, 0711); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink("../lib/node_modules/t3/bin/t3.js", filepath.Join(prefix, "bin/t3")); e != nil {
			t.Fatal(e)
		}
		adapterTestWrite(t, c.Path(release.T3StatePath, "current", "evidence"), "previous-provenance")
	}
	tx, e := t3Begin(c, prefix)
	if e != nil {
		t.Fatal(e)
	}
	stage := filepath.Join(tx.dir, "stage")
	adapterTestWrite(t, filepath.Join(stage, "bin/t3.js"), "new-bytes")
	stateStage, e := os.MkdirTemp(filepath.Dir(tx.state), ".test-stage-")
	if e != nil {
		t.Fatal(e)
	}
	adapterTestWrite(t, filepath.Join(stateStage, "pin"), "new-provenance")
	return c, tx, stage, stateStage
}
func TestT3TransactionEveryPartialRollback(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, phase := range []string{"before-package-move", "package-moved", "package-activated", "link-activated", "state-moved", "state-activated"} {
			t.Run(fmt.Sprintf("%t/%s", existing, phase), func(t *testing.T) {
				_, tx, stage, state := t3TxPrepare(t, existing)
				tx.fault = func(got string) error {
					if got == phase {
						return errors.New("injected transition failure")
					}
					return nil
				}
				if e := tx.activate(stage, state); e == nil {
					t.Fatal("fault did not fire")
				}
				if e := tx.restore(); e != nil {
					t.Fatal(e)
				}
				if existing {
					b, e := os.ReadFile(filepath.Join(tx.packagePath, "bin/t3.js"))
					if e != nil || string(b) != "previous-exact" {
						t.Fatal("bytes not restored")
					}
					i, e := os.Stat(filepath.Join(tx.packagePath, "bin/t3.js"))
					if e != nil || i.Mode().Perm() != 0711 {
						t.Fatal("mode not restored")
					}
					d, e := release.T3TreeDigest(tx.state)
					if e != nil || d != tx.oldStateDigest {
						t.Fatal("provenance not restored")
					}
				} else {
					if _, e := os.Lstat(tx.packagePath); !os.IsNotExist(e) {
						t.Fatal("original absence not restored")
					}
				}
				link, e := t3LinkValue(tx.link)
				if e != nil || link != tx.oldLink {
					t.Fatal("launcher mapping not restored")
				}
			})
		}
	}
}
func TestT3TransactionRefusesInterveningChanges(t *testing.T) {
	for _, boundary := range []string{"package", "link", "state", "backup", "prefix"} {
		t.Run(boundary, func(t *testing.T) {
			_, tx, stage, state := t3TxPrepare(t, true)
			if e := tx.activate(stage, state); e != nil {
				t.Fatal(e)
			}
			switch boundary {
			case "package":
				adapterTestWrite(t, filepath.Join(tx.packagePath, "foreign"), "foreign")
			case "link":
				if e := t3SwapLink("/foreign", tx.link); e != nil {
					t.Fatal(e)
				}
			case "state":
				adapterTestWrite(t, filepath.Join(tx.state, "foreign"), "foreign")
			case "backup":
				adapterTestWrite(t, filepath.Join(tx.dir, "previous/foreign"), "foreign")
			case "prefix":
				if e := os.Rename(tx.prefix, tx.prefix+"-moved"); e != nil {
					t.Fatal(e)
				}
			}
			if e := tx.restore(); e == nil {
				t.Fatal("overwrote intervening actor")
			}
		})
	}
	_, tx, _, _ := t3TxPrepare(t, true)
	adapterTestWrite(t, filepath.Join(tx.packagePath, "foreign"), "foreign")
	if e := tx.unchanged(); e == nil {
		t.Fatal("staging change missed")
	}
}
func TestT3TransactionRefusesForeignLauncher(t *testing.T) {
	c, tx, _, _ := t3TxPrepare(t, true)
	if e := t3SwapLink("/usr/bin/mise", tx.link); e != nil {
		t.Fatal(e)
	}
	if _, e := t3Begin(c, tx.prefix); e == nil {
		t.Fatal("accepted a mise/foreign link")
	}
}
func t3FakeProcess(t *testing.T, pid, exe string) {
	t.Helper()
	base := filepath.Join(t3ProcRoot, pid)
	adapterTestWrite(t, filepath.Join(base, "stat"), pid+" (t3) S "+strings.Repeat("0 ", 18)+"123\n")
	adapterTestWrite(t, filepath.Join(base, "cmdline"), exe+"\x00start\x00")
	adapterTestWrite(t, filepath.Join(base, "maps"), "")
	if e := os.Symlink(exe, filepath.Join(base, "exe")); e != nil {
		t.Fatal(e)
	}
}
func t3ArtifactHost(t *testing.T, failQualification, failRestart bool, idleAfterStage, idleAfterActivation int) (*Context, map[string]any, *adapterTestRunner, string) {
	t.Helper()
	s, archive, receipt := t3AgentFixture(t, release.T3Target())
	artifactBody, _ := json.Marshal(s)
	var raw map[string]any
	_ = json.Unmarshal(artifactBody, &raw)
	spec := map[string]any{"kind": "npm-package", "package": "t3", "service": "t3code.service", "version": "0.0.45", "artifact": raw, "compatibility": map[string]any{"min": "0.0.38", "max": "0.0.45", "steward_version": "0.11.0"}}
	runner := &adapterTestRunner{}
	c := adapterTestContext(t, runner)
	c.Components["t3-steward"] = stewardTestSpec(false)
	prefix := c.Path(".local/t3-prefix")
	packagePath := filepath.Join(prefix, "lib/node_modules/t3")
	adapterTestWrite(t, filepath.Join(packagePath, "bin/t3.js"), "old-launcher")
	adapterTestWrite(t, filepath.Join(packagePath, "package.json"), `{"name":"t3","version":"0.0.45"}`)
	adapterTestWrite(t, filepath.Join(prefix, "bin/node"), "node")
	if e := os.Chmod(filepath.Join(prefix, "bin/node"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll(filepath.Join(prefix, "bin"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("../lib/node_modules/t3/bin/t3.js", filepath.Join(prefix, "bin/t3")); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Join(packagePath, "bin/t3.js"), 0755); e != nil {
		t.Fatal(e)
	}
	c.Env = []string{"PATH=" + filepath.Join(prefix, "bin")}
	oldProc := t3ProcRoot
	t3ProcRoot = t.TempDir()
	t.Cleanup(func() { t3ProcRoot = oldProc })
	oldExe := c.Path("old-native")
	adapterTestWrite(t, oldExe, "old-native")
	t3FakeProcess(t, "123", oldExe)
	native := filepath.Join(packagePath, "node_modules/@t3code/t3-"+release.T3Target()+"/t3")
	pid := "123"
	qualified := false
	restarts := 0
	runner.handle = func(cmd procx.Command) procx.Result {
		argv := cmd.Argv
		last := argv[len(argv)-1]
		switch {
		case last == "version":
			return procx.Result{Stdout: stewardTestReport}
		case last == "check":
			count := 0
			_, activated := os.Stat(filepath.Join(packagePath, "node_modules"))
			if qualified {
				count = idleAfterStage
			}
			if activated == nil {
				count = idleAfterActivation
			}
			return procx.Result{Stdout: fmt.Sprintf("T3 server 0.0.45 serving\nshell snapshot: 4 threads, %d running", count)}
		case adapterTestMatches(argv, "npm", "ls"):
			return procx.Result{Stdout: `{"dependencies":{"t3":{"version":"0.0.45"}}}`}
		case adapterTestMatches(argv, "npm", "prefix"):
			return procx.Result{Stdout: prefix}
		case adapterTestMatches(argv, "is-active"):
			return procx.Result{Stdout: "active"}
		case adapterTestMatches(argv, "MainPID"):
			return procx.Result{Stdout: pid}
		case adapterTestMatches(argv, "ExecStart"):
			return procx.Result{Stdout: "{ path=" + filepath.Join(prefix, "bin/t3") + " ; argv[]=t3 start ; }"}
		case argv[0] == "python3":
			// Acquisition is deterministic fixture bytes; no gh/network is run.
			name := argv[7]
			body := archive
			if strings.HasSuffix(name, ".json") {
				body = receipt
			}
			if e := os.WriteFile(argv[8], body, 0600); e != nil {
				t.Fatal(e)
			}
			return procx.Result{}
		case len(argv) >= 3 && filepath.Base(argv[0]) == "node" && argv[1] == "-e":
			if strings.Contains(argv[2], "t3PackageLayout") {
				entry := argv[3]
				root := filepath.Dir(filepath.Dir(entry))
				b, _ := json.Marshal(map[string]any{"kind": "executable", "executable": filepath.Join(root, "node_modules/@t3code/t3-"+release.T3Target()+"/t3")})
				return procx.Result{Stdout: string(b)}
			}
			return procx.Result{}
		case last == "--version":
			return procx.Result{Stdout: "t3 v0.0.45"}
		case adapterTestMatches(argv, t3QualificationSupervisorArg):
			if qualified && failQualification {
				return procx.Result{Code: 1, Stderr: "isolated startup qualification failed"}
			}
			qualified = true
			return procx.Result{Stdout: "isolated startup and PTY qualified; cleanup complete"}
		case adapterTestMatches(argv, "restart"):
			restarts++
			if failRestart && restarts == 1 {
				return procx.Result{Code: 1, Stderr: "injected restart failure"}
			}
			if _, e := os.Stat(native); e == nil {
				pid = "124"
				if _, e := os.Stat(filepath.Join(t3ProcRoot, pid)); os.IsNotExist(e) {
					t3FakeProcess(t, pid, native)
				}
			}
			return procx.Result{}
		}
		return procx.Result{}
	}
	return c, spec, runner, prefix
}
func TestT3ControlledIdleAndFailurePaths(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		failQual, failRestart    bool
		stageIdle, activatedIdle int
		want                     string
	}{
		{"idle-changed-before-activation", false, false, 2, 0, "deferred"},
		{"unknown-before-activation", false, false, -1, 0, "deferred"},
		{"idle-changed-before-restart", false, false, 0, 2, "deferred"},
		{"qualification-rollback", true, false, 0, 0, "rolled-back"},
		{"restart-rollback", false, true, 0, 0, "rolled-back"},
		{"verified-install", false, false, 0, 0, "updated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, spec, runner, prefix := t3ArtifactHost(t, tc.failQual, tc.failRestart, tc.stageIdle, tc.activatedIdle)
			old, e := release.T3TreeDigest(filepath.Join(prefix, "lib/node_modules/t3"))
			if e != nil {
				t.Fatal(e)
			}
			before, e := t3Observe(c, spec)
			if e != nil {
				t.Fatal(e)
			}
			actions := []string{}
			got, e := t3Apply(c, spec, before, &actions)
			if tc.want == "rolled-back" {
				var rollback *RolledBack
				if !errors.As(e, &rollback) {
					t.Fatalf("want rollback, got %s %v", got, e)
				}
				digest, e := release.T3TreeDigest(filepath.Join(prefix, "lib/node_modules/t3"))
				if e != nil || digest != old {
					t.Fatal("handler did not restore exact old tree")
				}
			} else if e != nil || got != tc.want {
				t.Fatalf("got %q %v; actions %v", got, e, actions)
			}
			if tc.stageIdle != 0 {
				digest, e := release.T3TreeDigest(filepath.Join(prefix, "lib/node_modules/t3"))
				if e != nil || digest != old {
					t.Fatal("busy activation changed bytes")
				}
			}
			if tc.want == "deferred" {
				for _, cmd := range runner.calls {
					if len(cmd) > 2 && cmd[0] == "systemctl" && cmd[2] == "restart" {
						t.Fatal("restart despite changed idle")
					}
				}
			}
			if tc.want == "updated" {
				after, e := t3Observe(c, spec)
				if e != nil {
					t.Fatal(e)
				}
				if after["artifact_installed"] != true || after["artifact_served"] != true {
					t.Fatalf("identity unverified: %v", after)
				}
				got, e = t3Apply(c, spec, after, &actions)
				if e != nil || got != "converged" {
					t.Fatalf("not converged: %s %v", got, e)
				}
				raw := spec["artifact"].(map[string]any)
				raw["source_commit"] = strings.Repeat("e", 40)
				after, e = t3Observe(c, spec)
				if e != nil {
					t.Fatal(e)
				}
				if after["artifact_installed"] == true {
					t.Fatal("same version different identity converged")
				}
				delete(spec, "artifact")
				after, e = t3Observe(c, spec)
				if e != nil {
					t.Fatal(e)
				}
				if after["artifact_installed"] == true {
					t.Fatal("controlled same-version installation treated as upstream")
				}
			}
		})
	}
}
func TestT3ServedRejectsOldInodeAndChangedUnit(t *testing.T) {
	c, spec, runner, prefix := t3ArtifactHost(t, false, false, 0, 0)
	before, _ := t3Observe(c, spec)
	actions := []string{}
	if _, e := t3Apply(c, spec, before, &actions); e != nil {
		t.Fatal(e)
	}
	native := filepath.Join(prefix, "lib/node_modules/t3/node_modules/@t3code/t3-"+release.T3Target()+"/t3")
	// Kernel evidence of an old replaced inode must not become current just
	// because a newly installed pathname contains the same version.
	old := native + ".old"
	if e := os.Rename(native, old); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(old)
	if e := os.WriteFile(native, b, 0755); e != nil {
		t.Fatal(e)
	}
	exe := filepath.Join(t3ProcRoot, "124/exe")
	if e := os.Remove(exe); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(old+" (deleted)", exe); e != nil {
		t.Fatal(e)
	}
	after, _ := t3Observe(c, spec)
	if after["artifact_served"] == true {
		t.Fatal("old inode served as current")
	}
	original := runner.handle
	runner.handle = func(cmd procx.Command) procx.Result {
		if adapterTestMatches(cmd.Argv, "ExecStart") {
			return procx.Result{Stdout: "{ path=/foreign ; }"}
		}
		return original(cmd)
	}
	after, _ = t3Observe(c, spec)
	if after["artifact_served"] == true {
		t.Fatal("foreign unit launcher accepted")
	}
}
