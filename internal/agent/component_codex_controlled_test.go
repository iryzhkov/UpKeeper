package agent

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/klauspost/compress/zstd"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func controlledArchive(t *testing.T, bad string) []byte {
	var b bytes.Buffer
	z, _ := zstd.NewWriter(&b)
	w := tar.NewWriter(z)
	add := func(n string, body []byte, mode int64, kind byte) {
		h := &tar.Header{Name: n, Mode: mode, Size: int64(len(body)), Typeflag: kind, Format: tar.FormatPAX}
		if kind == tar.TypeSymlink {
			h.Linkname = "/tmp/escape"
			h.Size = 0
		}
		if e := w.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if kind == tar.TypeReg {
			w.Write(body)
		}
	}
	if bad == "symlink" {
		add("bin/codex", nil, 0755, tar.TypeSymlink)
	} else if bad == "duplicate" {
		add("bin/", nil, 0755, tar.TypeDir)
		add("bin/", nil, 0755, tar.TypeDir)
	} else if bad != "" {
		add(bad, []byte("bad"), 0644, tar.TypeReg)
	} else {
		add("bin/", nil, 0755, tar.TypeDir)
		add("codex-path/", nil, 0755, tar.TypeDir)
		add("codex-resources/", nil, 0755, tar.TypeDir)
		add("bin/codex", []byte("#!/bin/sh\n"), 0755, tar.TypeReg)
		pkg, _ := json.Marshal(map[string]any{"layoutVersion": 1, "version": "0.155.1", "variant": "codex", "target": release.ControlledCodexTarget(), "entrypoint": "bin/codex", "resourcesDir": "codex-resources", "pathDir": "codex-path"})
		add("codex-package.json", pkg, 0644, tar.TypeReg)
	}
	w.Close()
	z.Close()
	return b.Bytes()
}
func controlledFixture(t *testing.T, archive []byte) (*Context, map[string]any, *adapterTestRunner) {
	r := &adapterTestRunner{}
	c := adapterTestContext(t, r)
	target := release.ControlledCodexTarget()
	asset := "codex-controlled-0.155.1-rc.1-" + target + ".tar.zst"
	tag := "controlled-v0.155.1-rc.1"
	commit := strings.Repeat("c", 40)
	tree := strings.Repeat("d", 40)
	ad := agentenv.Digest(archive)
	packageBody, _ := json.Marshal(map[string]any{"layoutVersion": 1, "version": "0.155.1", "variant": "codex", "target": target, "entrypoint": "bin/codex", "resourcesDir": "codex-resources", "pathDir": "codex-path"})
	receipt := release.ControlledCodexReceipt{Schema: "codex-controlled-runtime-receipt-v1", Repository: "iryzhkov/codex", Tag: tag, SourceCommit: commit, SourceTree: tree, CargoVersion: "0.155.1", Target: target, VoiceTarget: map[string]string{"x86_64-unknown-linux-musl": "x86_64-unknown-linux-gnu", "aarch64-unknown-linux-musl": "aarch64-unknown-linux-gnu"}[target], RunnerArch: map[string]string{"x86_64-unknown-linux-musl": "X64", "aarch64-unknown-linux-musl": "ARM64"}[target], Archive: asset, ArchiveSHA256: ad, RustcVV: "rustc", CargoV: "cargo", VoiceHostLDD: "ok", RustyV8ArchiveSHA256: strings.Repeat("e", 64), RustyV8BindingSHA256: strings.Repeat("f", 64), QualificationWorkflow: "controlled-runtime-release", QualificationRunID: "123", QualificationRunAttempt: "1", FilesSHA256: map[string]string{"bin/codex": agentenv.Digest([]byte("#!/bin/sh\n")), "codex-package.json": agentenv.Digest(packageBody)}}
	rb, _ := json.Marshal(receipt)
	one := func(t string) map[string]any {
		n := "codex-controlled-0.155.1-rc.1-" + t + ".tar.zst"
		sha := strings.Repeat("a", 64)
		rsha := strings.Repeat("b", 64)
		if t == target {
			sha = ad
			rsha = agentenv.Digest(rb)
		}
		return map[string]any{"asset": n, "sha256": sha, "receipt_asset": strings.TrimSuffix(n, ".tar.zst") + ".receipt.json", "receipt_sha256": rsha}
	}
	s := map[string]any{"kind": "github-release", "repository": "iryzhkov/codex", "tag": tag, "source_commit": commit, "source_tree": tree, "assets": map[string]any{release.ControlledCodexTargets[0]: one(release.ControlledCodexTargets[0]), release.ControlledCodexTargets[1]: one(release.ControlledCodexTargets[1])}, "hosts": []any{"testhost"}}
	r.handle = func(cmd procx.Command) procx.Result {
		name := cmd.Argv[len(cmd.Argv)-3]
		dir := cmd.Argv[len(cmd.Argv)-1]
		body := archive
		if strings.HasSuffix(name, ".json") {
			body = rb
		}
		if e := os.WriteFile(filepath.Join(dir, name), body, 0600); e != nil {
			t.Fatal(e)
		}
		return procx.Result{}
	}
	return c, s, r
}
func TestControlledCodexInstallIdempotentAndIsolated(t *testing.T) {
	c, s, _ := controlledFixture(t, controlledArchive(t, ""))
	normal := c.Path(".local/share/mise/shims/codex")
	adapterTestWrite(t, normal, "normal")
	before, _ := controlledCodexObserve(c, s)
	a := []string{}
	if state, e := controlledCodexApply(c, s, before, &a); e != nil || state != "updated" {
		t.Fatalf("%s %v", state, e)
	}
	after, e := controlledCodexObserve(c, s)
	if e != nil || after["converged"] != true {
		t.Fatalf("%v %v", after, e)
	}
	got, _ := os.ReadFile(normal)
	if string(got) != "normal" {
		t.Fatal("default Codex changed")
	}
	if state, e := controlledCodexApply(c, s, after, &a); e != nil || state != "converged" {
		t.Fatalf("%s %v", state, e)
	}
}
func TestControlledCodexDryRunAndUnsafeArchive(t *testing.T) {
	c, s, _ := controlledFixture(t, controlledArchive(t, "../escape"))
	c.DryRun = true
	b, _ := controlledCodexObserve(c, s)
	a := []string{}
	controlledCodexApply(c, s, b, &a)
	if _, e := os.Lstat(c.Path(controlledCodexBinary)); !os.IsNotExist(e) {
		t.Fatal("dry run wrote")
	}
	for _, bad := range []string{"../escape", "symlink", "duplicate"} {
		t.Run(bad, func(t *testing.T) {
			c, s, _ := controlledFixture(t, controlledArchive(t, bad))
			b, _ := controlledCodexObserve(c, s)
			if _, e := controlledCodexApply(c, s, b, &[]string{}); e == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
}
func TestControlledCodexRefusesNonSymlinkEntryPoint(t *testing.T) {
	c, s, _ := controlledFixture(t, controlledArchive(t, ""))
	adapterTestWrite(t, c.Path(controlledCodexBinary), "prior")
	b, _ := controlledCodexObserve(c, s)
	if _, e := controlledCodexApply(c, s, b, &[]string{}); e == nil {
		t.Fatal("replaced non-symlink")
	}
	got, _ := os.ReadFile(c.Path(controlledCodexBinary))
	if string(got) != "prior" {
		t.Fatal("changed prior")
	}
}
func TestControlledCodexActivationFailureRestoresPriorLink(t *testing.T) {
	c, s, _ := controlledFixture(t, controlledArchive(t, ""))
	prior := c.Path(".local/share/prior-codex")
	adapterTestWrite(t, prior, "prior")
	os.MkdirAll(filepath.Dir(c.Path(controlledCodexBinary)), 0755)
	os.Symlink(prior, c.Path(controlledCodexBinary))
	saved := controlledCodexPostActivate
	controlledCodexPostActivate = func(*Context, map[string]any) (map[string]any, error) { return nil, errors.New("probe failed") }
	t.Cleanup(func() { controlledCodexPostActivate = saved })
	b, _ := controlledCodexObserve(c, s)
	if _, e := controlledCodexApply(c, s, b, &[]string{}); e == nil {
		t.Fatal("activation unexpectedly succeeded")
	}
	got, _ := os.Readlink(c.Path(controlledCodexBinary))
	if got != prior {
		t.Fatalf("rollback link %q", got)
	}
}
func TestControlledCodexConcurrentActivationIsPreserved(t *testing.T) {
	c, s, _ := controlledFixture(t, controlledArchive(t, ""))
	other := c.Path(".local/share/concurrent-codex")
	adapterTestWrite(t, other, "other")
	saved := controlledCodexPostActivate
	controlledCodexPostActivate = func(c *Context, _ map[string]any) (map[string]any, error) {
		os.Remove(c.Path(controlledCodexBinary))
		os.Symlink(other, c.Path(controlledCodexBinary))
		return nil, errors.New("probe failed")
	}
	t.Cleanup(func() { controlledCodexPostActivate = saved })
	b, _ := controlledCodexObserve(c, s)
	if _, e := controlledCodexApply(c, s, b, &[]string{}); e == nil {
		t.Fatal("activation unexpectedly succeeded")
	}
	got, _ := os.Readlink(c.Path(controlledCodexBinary))
	if got != other {
		t.Fatalf("concurrent link not preserved: %q", got)
	}
}
func TestControlledCodexObservationDetectsTreeTamper(t *testing.T) {
	c, s, _ := controlledFixture(t, controlledArchive(t, ""))
	b, _ := controlledCodexObserve(c, s)
	if _, e := controlledCodexApply(c, s, b, &[]string{}); e != nil {
		t.Fatal(e)
	}
	link, _ := os.Readlink(c.Path(controlledCodexBinary))
	if e := os.WriteFile(link, []byte("tampered"), 0755); e != nil {
		t.Fatal(e)
	}
	after, e := controlledCodexObserve(c, s)
	if e != nil || after["converged"] == true {
		t.Fatalf("tamper not detected: %v %v", after, e)
	}
}

func TestControlledCodexRepairsCorruptPinnedRoot(t *testing.T) {
	c, s, _ := controlledFixture(t, controlledArchive(t, ""))
	before, _ := controlledCodexObserve(c, s)
	if _, e := controlledCodexApply(c, s, before, &[]string{}); e != nil {
		t.Fatal(e)
	}
	link, _ := os.Readlink(c.Path(controlledCodexBinary))
	if e := os.WriteFile(link, []byte("corrupt"), 0755); e != nil {
		t.Fatal(e)
	}
	before, _ = controlledCodexObserve(c, s)
	if _, e := controlledCodexApply(c, s, before, &[]string{}); e != nil {
		t.Fatal(e)
	}
	body, _ := os.ReadFile(link)
	if string(body) != "#!/bin/sh\n" {
		t.Fatalf("corrupt pinned root was not repaired: %q", body)
	}
}
