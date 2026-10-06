package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func t3Fixture(t *testing.T, target string) (*T3Artifact, []byte, []byte) {
	t.Helper()
	version := "0.0.45"
	s := &T3Artifact{Repository: "iryzhkov/t3code", Tag: "fleet-fixture", SourceCommit: strings.Repeat("a", 40), SourceTree: strings.Repeat("b", 40), Assets: map[string]T3Asset{}}
	for _, p := range []string{"linux-x64", "linux-arm64"} {
		name := "t3-fleet-" + version + "-" + p
		s.Assets[p] = T3Asset{name + ".tar.gz", strings.Repeat("c", 64), name + ".receipt.json", strings.Repeat("d", 64)}
	}
	metadata := map[string]any{"schema": "upkeeper-t3-build/v1", "source_commit": s.SourceCommit, "source_tree": s.SourceTree, "platform": target, "base_version": version}
	pkg, _ := json.Marshal(map[string]any{"name": "t3", "version": version, "bin": map[string]any{"t3": "./bin/t3.js"}, "optionalDependencies": map[string]any{"@t3code/t3-" + target: version}, "upkeeperControlledBuild": metadata})
	native, _ := json.Marshal(map[string]any{"name": "@t3code/t3-" + target, "version": version, "os": []string{"linux"}, "cpu": []string{strings.TrimPrefix(target, "linux-")}})
	files := map[string][]byte{"package.json": pkg, "bin/t3.js": []byte("#!/usr/bin/env node\n"), "node_modules/@t3code/t3-" + target + "/package.json": native, "node_modules/@t3code/t3-" + target + "/t3": []byte("fixture-native-not-qualified")}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if e := tw.WriteHeader(&tar.Header{Name: "t3/", Typeflag: tar.TypeDir, Mode: 0755}); e != nil {
		t.Fatal(e)
	}
	r := T3Receipt{Schema: "upkeeper-t3-receipt/v1", SourceCommit: s.SourceCommit, SourceTree: s.SourceTree, Platform: target, BaseVersion: version, Repository: s.Repository, Archive: s.Assets[target].Asset}
	// Stable fixture order independent of Go map iteration.
	for _, p := range []string{"package.json", "bin/t3.js", "node_modules/@t3code/t3-" + target + "/package.json", "node_modules/@t3code/t3-" + target + "/t3"} {
		b := files[p]
		mode := int64(0644)
		if strings.HasSuffix(p, "/t3") || p == "bin/t3.js" {
			mode = 0755
		}
		r.Files = append(r.Files, T3File{p, T3Digest(b), int64(len(b)), mode})
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
	r.ArchiveSHA256 = T3Digest(archive.Bytes())
	body, _ := json.Marshal(r)
	a := s.Assets[target]
	a.SHA256 = r.ArchiveSHA256
	a.ReceiptSHA256 = T3Digest(body)
	s.Assets[target] = a
	return s, archive.Bytes(), body
}
func t3ArtifactMap(s *T3Artifact) map[string]any {
	b, _ := json.Marshal(s)
	m, _ := canonT3(b)
	return m
}
func canonT3(b []byte) (map[string]any, error) {
	var m map[string]any
	e := json.Unmarshal(b, &m)
	return m, e
}

func TestT3ArtifactStrictSchema(t *testing.T) {
	s, _, _ := t3Fixture(t, "linux-x64")
	base := t3ArtifactMap(s)
	if _, e := DecodeT3Artifact(base, "0.0.45"); e != nil {
		t.Fatal(e)
	}
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"null", func(m map[string]any) { m["assets"] = nil }},
		{"extra", func(m map[string]any) { m["url"] = "https://example.invalid" }},
		{"repository", func(m map[string]any) { m["repository"] = "other/t3code" }},
		{"short-commit", func(m map[string]any) { m["source_commit"] = "abc" }},
		{"uppercase", func(m map[string]any) { m["source_tree"] = strings.Repeat("A", 40) }},
		{"tag-path", func(m map[string]any) { m["tag"] = "a/b" }},
		{"tag-option", func(m map[string]any) { m["tag"] = "--latest" }},
		{"target", func(m map[string]any) { m["assets"].(map[string]any)["darwin-x64"] = map[string]any{} }},
		{"missing-target", func(m map[string]any) { delete(m["assets"].(map[string]any), "linux-arm64") }},
		{"asset-extra", func(m map[string]any) { m["assets"].(map[string]any)["linux-x64"].(map[string]any)["url"] = "x" }},
		{"asset-name", func(m map[string]any) { m["assets"].(map[string]any)["linux-x64"].(map[string]any)["asset"] = "../a" }},
		{"digest", func(m map[string]any) {
			m["assets"].(map[string]any)["linux-x64"].(map[string]any)["sha256"] = "pending"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := t3ArtifactMap(s)
			tc.change(m)
			if _, e := DecodeT3Artifact(m, "0.0.45"); e == nil {
				t.Fatal("accepted invalid schema")
			}
		})
	}
	if _, e := DecodeT3Artifact(nil, "0.0.45"); e == nil {
		t.Fatal("accepted null artifact")
	}
}
func TestT3ReceiptStrictSchema(t *testing.T) {
	s, _, body := t3Fixture(t, "linux-x64")
	if _, e := DecodeT3Receipt(body, s, "linux-x64", "0.0.45"); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{
		strings.Replace(string(body), `"schema":`, `"schema":"duplicate","schema":`, 1),
		string(body) + " {}",
		strings.Replace(string(body), `"size":`, `"extra":1,"size":`, 1),
		strings.Replace(string(body), `"size":`, `"size":true,"ignored":`, 1),
		strings.Replace(string(body), `"mode":420`, `"mode":420.0`, 1),
		strings.Replace(string(body), `"mode":420`, `"mode":4095`, 1),
		strings.Replace(string(body), `"path":"package.json"`, `"path":"../package.json"`, 1),
		strings.Replace(string(body), `"platform":"linux-x64"`, `"platform":"linux-arm64"`, 1),
	} {
		if _, e := DecodeT3Receipt([]byte(bad), s, "linux-x64", "0.0.45"); e == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	var r T3Receipt
	_ = json.Unmarshal(body, &r)
	r.Files = append(r.Files, r.Files[0])
	bad, _ := json.Marshal(r)
	if _, e := DecodeT3Receipt(bad, s, "linux-x64", "0.0.45"); e == nil {
		t.Fatal("accepted duplicate files")
	}
}
func TestT3BundleAndInstalledProvenance(t *testing.T) {
	for _, target := range []string{"linux-x64", "linux-arm64"} {
		t.Run(target, func(t *testing.T) {
			s, archive, receipt := t3Fixture(t, target)
			root := t.TempDir()
			r, e := VerifyT3Bundle(archive, receipt, s, target, "0.0.45", root)
			if e != nil {
				t.Fatal(e)
			}
			if e = VerifyT3Tree(root, r, s); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(root, "extra"), []byte("foreign"), 0600); e != nil {
				t.Fatal(e)
			}
			if e = VerifyT3Tree(root, r, s); e == nil {
				t.Fatal("accepted extra payload")
			}
			if e = os.Remove(filepath.Join(root, "extra")); e != nil {
				t.Fatal(e)
			}
			if e = os.Chmod(filepath.Join(root, "bin/t3.js"), 0644); e != nil {
				t.Fatal(e)
			}
			if e = VerifyT3Tree(root, r, s); e == nil {
				t.Fatal("accepted wrong mode")
			}
			if _, e = VerifyT3Bundle(append([]byte{0}, archive...), receipt, s, target, "0.0.45", ""); e == nil {
				t.Fatal("accepted corrupt archive")
			}
			if _, e = VerifyT3Bundle(archive, receipt, s, "unsupported", "0.0.45", ""); e == nil {
				t.Fatal("accepted unsupported platform")
			}
		})
	}
}
func TestT3MaliciousArchives(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		kind       byte
		mode       int64
	}{
		{"traversal", "t3/../escape", tar.TypeReg, 0644},
		{"absolute", "/t3/package.json", tar.TypeReg, 0644},
		{"alias", "t3/./package.json", tar.TypeReg, 0644},
		{"link", "t3/package.json", tar.TypeSymlink, 0644},
		{"hardlink", "t3/package.json", tar.TypeLink, 0644},
		{"fifo", "t3/package.json", tar.TypeFifo, 0644},
		{"privileged", "t3/package.json", tar.TypeReg, 04755},
		{"extra-directory", "t3/unlisted/", tar.TypeDir, 0755},
		{"extra-file", "t3/unlisted", tar.TypeReg, 0644},
		{"duplicate-root", "t3/", tar.TypeDir, 0755},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, body := t3Fixture(t, "linux-x64")
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			_ = tw.WriteHeader(&tar.Header{Name: "t3/", Typeflag: tar.TypeDir, Mode: 0755})
			h := &tar.Header{Name: tc.path, Typeflag: tc.kind, Mode: tc.mode}
			if tc.kind == tar.TypeSymlink || tc.kind == tar.TypeLink {
				h.Linkname = "outside"
			}
			if e := tw.WriteHeader(h); e != nil {
				t.Fatal(e)
			}
			_ = tw.Close()
			_ = gz.Close()
			var r T3Receipt
			_ = json.Unmarshal(body, &r)
			r.ArchiveSHA256 = T3Digest(buf.Bytes())
			body, _ = json.Marshal(r)
			a := s.Assets["linux-x64"]
			a.SHA256 = r.ArchiveSHA256
			a.ReceiptSHA256 = T3Digest(body)
			s.Assets["linux-x64"] = a
			if _, e := VerifyT3Bundle(buf.Bytes(), body, s, "linux-x64", "0.0.45", t.TempDir()); e == nil {
				t.Fatal("accepted malicious archive")
			}
		})
	}
}
