package release

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/klauspost/compress/zstd"
)

func controlledSpec() map[string]any {
	asset := func(t string) map[string]any {
		n := "codex-controlled-0.155.1-rc.1-" + t + ".tar.zst"
		return map[string]any{"asset": n, "sha256": strings.Repeat("a", 64), "receipt_asset": strings.TrimSuffix(n, ".tar.zst") + ".receipt.json", "receipt_sha256": strings.Repeat("b", 64)}
	}
	return map[string]any{"kind": "github-release", "repository": "iryzhkov/codex", "tag": "controlled-v0.155.1-rc.1", "source_commit": strings.Repeat("c", 40), "source_tree": strings.Repeat("d", 40), "assets": map[string]any{ControlledCodexTargets[0]: asset(ControlledCodexTargets[0]), ControlledCodexTargets[1]: asset(ControlledCodexTargets[1])}, "hosts": []any{"laptop"}}
}

func controlledReceipt(s *ControlledCodex, target string) ControlledCodexReceipt {
	a := s.Assets[target]
	return ControlledCodexReceipt{Schema: "codex-controlled-runtime-receipt-v1", Repository: s.Repository, Tag: s.Tag, SourceCommit: s.SourceCommit, SourceTree: s.SourceTree, CargoVersion: "0.155.1", Target: target, VoiceTarget: "x86_64-unknown-linux-gnu", RunnerArch: "X64", Archive: a.Asset, ArchiveSHA256: a.SHA256, RustcVV: "rustc", CargoV: "cargo", VoiceHostLDD: "ok", RustyV8ArchiveSHA256: strings.Repeat("e", 64), RustyV8BindingSHA256: strings.Repeat("f", 64), QualificationWorkflow: "controlled-runtime-release", QualificationRunID: "123", QualificationRunAttempt: "1", FilesSHA256: map[string]string{"bin/codex": strings.Repeat("1", 64)}}
}

func TestControlledCodexClosedContract(t *testing.T) {
	if _, e := DecodeControlledCodex(controlledSpec()); e != nil {
		t.Fatal(e)
	}
	for _, f := range []func(map[string]any){func(m map[string]any) { m["extra"] = true }, func(m map[string]any) { m["tag"] = "v0.1.0" }, func(m map[string]any) { m["hosts"] = []any{} }, func(m map[string]any) { m["source_tree"] = "bad" }} {
		m := controlledSpec()
		f(m)
		if _, e := DecodeControlledCodex(m); e == nil {
			t.Fatal("accepted invalid contract")
		}
	}
}

func TestControlledCodexReceiptContract(t *testing.T) {
	s, _ := DecodeControlledCodex(controlledSpec())
	target := ControlledCodexTargets[0]
	r := controlledReceipt(s, target)
	body, _ := json.Marshal(r)
	if _, e := DecodeControlledCodexReceipt(body, s, target); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*ControlledCodexReceipt){
		func(r *ControlledCodexReceipt) { r.SourceTree = strings.Repeat("0", 40) },
		func(r *ControlledCodexReceipt) { r.CargoVersion = "0.155.2" },
		func(r *ControlledCodexReceipt) { r.VoiceTarget = "aarch64-unknown-linux-gnu" },
		func(r *ControlledCodexReceipt) { r.RunnerArch = "ARM64" },
		func(r *ControlledCodexReceipt) { r.QualificationWorkflow = "other" },
	} {
		r := controlledReceipt(s, target)
		mutate(&r)
		body, _ = json.Marshal(r)
		if _, e := DecodeControlledCodexReceipt(body, s, target); e == nil {
			t.Fatal("accepted mismatched receipt")
		}
	}
}

func TestControlledCaptureArchiveModesRejectsUnsafeNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		member   string
		typeflag byte
	}{
		{name: "parent directory", member: "../", typeflag: tar.TypeDir},
		{name: "repeated separator", member: "bin//codex", typeflag: tar.TypeReg},
		{name: "regular trailing slash", member: "bin/codex/", typeflag: tar.TypeReg},
		{name: "absolute", member: "/bin/codex", typeflag: tar.TypeReg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw bytes.Buffer
			tw := tar.NewWriter(&raw)
			size := int64(0)
			headerType := tc.typeflag
			if tc.typeflag == tar.TypeReg {
				size = 1
			}
			if tc.member == "bin/codex/" && tc.typeflag == tar.TypeReg {
				headerType, size = tar.TypeDir, 0
			}
			if e := tw.WriteHeader(&tar.Header{Name: tc.member, Typeflag: headerType, Mode: 0755, Size: size}); e != nil {
				t.Fatal(e)
			}
			if size != 0 {
				if _, e := tw.Write([]byte("x")); e != nil {
					t.Fatal(e)
				}
			}
			tw.Close()
			rawBytes := raw.Bytes()
			if tc.member == "bin/codex/" && tc.typeflag == tar.TypeReg {
				rawBytes[156] = tar.TypeReg
				for i := 148; i < 156; i++ {
					rawBytes[i] = ' '
				}
				sum := 0
				for _, b := range rawBytes[:512] {
					sum += int(b)
				}
				copy(rawBytes[148:156], []byte(fmt.Sprintf("%06o\x00 ", sum)))
			}
			var compressed bytes.Buffer
			zw, _ := zstd.NewWriter(&compressed)
			if _, e := zw.Write(rawBytes); e != nil {
				t.Fatal(e)
			}
			zw.Close()
			if e := controlledCaptureArchiveModes(t.TempDir(), compressed.Bytes(), &ControlledCodexReceipt{FilesSHA256: map[string]string{}}); e == nil || (!strings.Contains(e.Error(), "unsafe controlled Codex archive path") && !strings.Contains(e.Error(), "controlled Codex archive escaped installed root")) {
				t.Fatalf("expected canonical-path refusal for %q, got %v", tc.member, e)
			}
		})
	}
}

func TestControlledCaptureArchiveModesRejectsModeTamper(t *testing.T) {
	var compressed bytes.Buffer
	zw, _ := zstd.NewWriter(&compressed)
	tw := tar.NewWriter(zw)
	body := []byte("#!/bin/sh\n")
	if e := tw.WriteHeader(&tar.Header{Name: "bin/codex", Typeflag: tar.TypeReg, Mode: 0755, Size: int64(len(body))}); e != nil {
		t.Fatal(e)
	}
	if _, e := tw.Write(body); e != nil {
		t.Fatal(e)
	}
	tw.Close()
	zw.Close()
	root := t.TempDir()
	if e := os.MkdirAll(filepath.Join(root, "bin"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "bin/codex"), body, 0755); e != nil {
		t.Fatal(e)
	}
	r := &ControlledCodexReceipt{FilesSHA256: map[string]string{"bin/codex": digestOf(body)}}
	if e := controlledCaptureArchiveModes(root, compressed.Bytes(), r); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Join(root, "bin/codex"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := controlledCaptureArchiveModes(root, compressed.Bytes(), r); e == nil {
		t.Fatal("accepted executable-mode tamper")
	}
}

func TestControlledCodexCaptureSucceedsForInstalledProducerFixture(t *testing.T) {
	target := ControlledCodexTarget()
	fixture := "../agent/testdata/codex-controlled-canonical-x86_64.tar.zst"
	if target == ControlledCodexTargets[1] {
		fixture = "../agent/testdata/codex-controlled-canonical-aarch64.tar.zst"
	}
	archive, e := os.ReadFile(fixture)
	if e != nil {
		t.Fatal(e)
	}
	archiveDigest := digestOf(archive)
	tag := "controlled-v0.155.1-rc.1"
	repository := "iryzhkov/codex"
	commit, tree := strings.Repeat("c", 40), strings.Repeat("d", 40)
	receipts := map[string][]byte{}
	assets := map[string]any{}
	files := map[string]string{}
	zr, _ := zstd.NewReader(bytes.NewReader(archive))
	tr := tar.NewReader(zr)
	home := t.TempDir()
	currentAsset := ""
	root := ""
	for {
		h, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		name := strings.TrimSuffix(h.Name, "/")
		if targetPath := filepath.Join(home, "fixture", filepath.FromSlash(name)); h.Typeflag == tar.TypeDir {
			if e = os.MkdirAll(targetPath, os.FileMode(h.Mode)&0777); e != nil {
				t.Fatal(e)
			}
		} else {
			body, readErr := io.ReadAll(tr)
			if readErr != nil {
				t.Fatal(readErr)
			}
			files[name] = digestOf(body)
		}
	}
	zr.Close()
	for _, receiptTarget := range ControlledCodexTargets {
		asset := "codex-controlled-0.155.1-rc.1-" + receiptTarget + ".tar.zst"
		assetDigest := strings.Repeat("a", 64)
		if receiptTarget == target {
			assetDigest, currentAsset = archiveDigest, asset
		}
		voice, arch := "x86_64-unknown-linux-gnu", "X64"
		if receiptTarget == ControlledCodexTargets[1] {
			voice, arch = "aarch64-unknown-linux-gnu", "ARM64"
		}
		receipt := ControlledCodexReceipt{Schema: "codex-controlled-runtime-receipt-v1", Repository: repository, Tag: tag, SourceCommit: commit, SourceTree: tree, CargoVersion: "0.155.1", Target: receiptTarget, VoiceTarget: voice, RunnerArch: arch, Archive: asset, ArchiveSHA256: assetDigest, RustcVV: "rustc", CargoV: "cargo", VoiceHostLDD: "ok", RustyV8ArchiveSHA256: strings.Repeat("e", 64), RustyV8BindingSHA256: strings.Repeat("f", 64), QualificationWorkflow: "controlled-runtime-release", QualificationRunID: "123", QualificationRunAttempt: "1", FilesSHA256: files}
		body, _ := json.Marshal(receipt)
		receiptName := strings.TrimSuffix(asset, ".tar.zst") + ".receipt.json"
		receipts[receiptName] = body
		assets[receiptTarget] = map[string]any{"asset": asset, "sha256": assetDigest, "receipt_asset": receiptName, "receipt_sha256": digestOf(body)}
	}
	raw := map[string]any{"kind": "github-release", "repository": repository, "tag": tag, "source_commit": commit, "source_tree": tree, "assets": assets, "hosts": []any{"testhost"}}
	root = filepath.Join(home, ".local/share/codex-controlled/releases", tag+"-"+target+"-"+archiveDigest[:16])
	zr, _ = zstd.NewReader(bytes.NewReader(archive))
	tr = tar.NewReader(zr)
	for {
		h, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		name := strings.TrimSuffix(h.Name, "/")
		p := filepath.Join(root, filepath.FromSlash(name))
		if h.Typeflag == tar.TypeDir {
			if e = os.MkdirAll(p, os.FileMode(h.Mode)&0777); e != nil {
				t.Fatal(e)
			}
			continue
		}
		body, _ := io.ReadAll(tr)
		if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(p, body, os.FileMode(h.Mode)&0777); e != nil {
			t.Fatal(e)
		}
	}
	zr.Close()
	if e = os.WriteFile(filepath.Join(root, ".artifact.tar.zst"), archive, 0600); e != nil {
		t.Fatal(e)
	}
	currentReceipt := receipts[strings.TrimSuffix(currentAsset, ".tar.zst")+".receipt.json"]
	if e = os.WriteFile(filepath.Join(root, ".receipt.json"), currentReceipt, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Join(home, ".local/bin"), 0755); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(filepath.Join(root, "bin/codex"), filepath.Join(home, ".local/bin/codex-controlled")); e != nil {
		t.Fatal(e)
	}
	runner := func(cmd procx.Command) procx.Result {
		pattern, dir := cmd.Argv[len(cmd.Argv)-3], cmd.Argv[len(cmd.Argv)-1]
		var body []byte
		if pattern == "SHA256SUMS" {
			for _, a := range assets {
				m := a.(map[string]any)
				body = append(body, []byte(m["sha256"].(string)+"  "+m["asset"].(string)+"\n"+m["receipt_sha256"].(string)+"  "+m["receipt_asset"].(string)+"\n")...)
			}
		} else {
			body = receipts[pattern]
		}
		if writeErr := os.WriteFile(filepath.Join(dir, pattern), body, 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
		return procx.Result{}
	}
	if _, e = CaptureControlledCodex(home, runner, raw); e != nil {
		t.Fatal(e)
	}
}

func TestControlledCodexCaptureRequiresInstalledIdentity(t *testing.T) {
	_, e := CaptureControlledCodex(t.TempDir(), func(procx.Command) procx.Result {
		t.Fatal("capture contacted release host before verifying installation")
		return procx.Result{}
	}, controlledSpec())
	if e == nil {
		t.Fatal("captured uninstalled controlled runtime")
	}
}
