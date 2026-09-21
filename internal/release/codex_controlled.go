package release

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/iryzhkov/UpKeeper/internal/canonjson"
	"github.com/iryzhkov/UpKeeper/internal/procx"
	"github.com/iryzhkov/UpKeeper/internal/safety"
	"github.com/klauspost/compress/zstd"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const ControlledCodexComponent = "codex-controlled-runtime"

var controlledTagPattern = regexp.MustCompile(`^controlled-v([0-9]+\.[0-9]+\.[0-9]+)-rc\.[1-9][0-9]*$`)
var controlledAssetPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)

type ControlledCodexAssetSpec struct {
	Asset         string `json:"asset"`
	SHA256        string `json:"sha256"`
	ReceiptAsset  string `json:"receipt_asset"`
	ReceiptSHA256 string `json:"receipt_sha256"`
}
type ControlledCodex struct {
	Kind         string                              `json:"kind"`
	Repository   string                              `json:"repository"`
	Tag          string                              `json:"tag"`
	SourceCommit string                              `json:"source_commit"`
	SourceTree   string                              `json:"source_tree"`
	Assets       map[string]ControlledCodexAssetSpec `json:"assets"`
	Hosts        []string                            `json:"hosts"`
}
type ControlledCodexReceipt struct {
	Schema                  string            `json:"schema"`
	Repository              string            `json:"repository"`
	Tag                     string            `json:"tag"`
	SourceCommit            string            `json:"source_commit"`
	SourceTree              string            `json:"source_tree"`
	CargoVersion            string            `json:"cargo_version"`
	Target                  string            `json:"target"`
	VoiceTarget             string            `json:"voice_target"`
	RunnerArch              string            `json:"runner_arch"`
	Archive                 string            `json:"archive"`
	ArchiveSHA256           string            `json:"archive_sha256"`
	RustcVV                 string            `json:"rustc_vv"`
	CargoV                  string            `json:"cargo_v"`
	VoiceHostLDD            string            `json:"voice_host_ldd"`
	RustyV8ArchiveSHA256    string            `json:"rusty_v8_archive_sha256"`
	RustyV8BindingSHA256    string            `json:"rusty_v8_binding_sha256"`
	QualificationWorkflow   string            `json:"qualification_workflow"`
	QualificationRunID      string            `json:"qualification_run_id"`
	QualificationRunAttempt string            `json:"qualification_run_attempt"`
	FilesSHA256             map[string]string `json:"files_sha256"`
}

var ControlledCodexTargets = []string{"x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl"}

func ControlledCodexTarget() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	if runtime.GOARCH == "amd64" {
		return ControlledCodexTargets[0]
	}
	if runtime.GOARCH == "arm64" {
		return ControlledCodexTargets[1]
	}
	return ""
}
func DecodeControlledCodex(raw map[string]any) (*ControlledCodex, error) {
	if !hasExactKeys(raw, "kind", "repository", "tag", "source_commit", "source_tree", "assets", "hosts") {
		return nil, errors.New("unknown codex-controlled-runtime component fields")
	}
	b, e := canonjson.Compact(raw)
	if e != nil {
		return nil, e
	}
	var s ControlledCodex
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&s); e != nil {
		return nil, e
	}
	if s.Kind != "github-release" || s.Repository != "iryzhkov/codex" || !controlledTagPattern.MatchString(s.Tag) {
		return nil, errors.New("invalid controlled Codex release source or tag")
	}
	if _, e = safety.ValidateGitCommit(s.SourceCommit); e != nil {
		return nil, e
	}
	if _, e = safety.ValidateGitCommit(s.SourceTree); e != nil {
		return nil, e
	}
	if len(s.Assets) != 2 {
		return nil, errors.New("controlled Codex requires exactly two target assets")
	}
	for _, target := range ControlledCodexTargets {
		a, ok := s.Assets[target]
		if !ok || !controlledAssetPattern.MatchString(a.Asset) || !controlledAssetPattern.MatchString(a.ReceiptAsset) || a.ReceiptAsset != strings.TrimSuffix(a.Asset, ".tar.zst")+".receipt.json" || !sha256Pattern.MatchString(a.SHA256) || !sha256Pattern.MatchString(a.ReceiptSHA256) {
			return nil, errors.New("invalid controlled Codex target asset")
		}
	}
	if len(s.Hosts) == 0 {
		return nil, errors.New("controlled Codex needs explicit hosts")
	}
	seen := map[string]bool{}
	for _, h := range s.Hosts {
		if _, e = safety.ValidateHostName(h, nil); e != nil {
			return nil, e
		}
		if seen[h] {
			return nil, errors.New("duplicate controlled Codex host")
		}
		seen[h] = true
	}
	return &s, nil
}
func DecodeControlledCodexReceipt(body []byte, s *ControlledCodex, target string) (*ControlledCodexReceipt, error) {
	var r ControlledCodexReceipt
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if e := d.Decode(&r); e != nil {
		return nil, e
	}
	a := s.Assets[target]
	tagParts := controlledTagPattern.FindStringSubmatch(s.Tag)
	version := ""
	if len(tagParts) > 1 {
		version = tagParts[1]
	}
	voiceTarget, runnerArch := "x86_64-unknown-linux-gnu", "X64"
	if target == "aarch64-unknown-linux-musl" {
		voiceTarget, runnerArch = "aarch64-unknown-linux-gnu", "ARM64"
	}
	if r.Schema != "codex-controlled-runtime-receipt-v1" || r.Repository != s.Repository || r.Tag != s.Tag || r.SourceCommit != s.SourceCommit || r.SourceTree != s.SourceTree || r.Target != target || r.CargoVersion != version || r.VoiceTarget != voiceTarget || !strings.EqualFold(r.RunnerArch, runnerArch) || r.Archive != a.Asset || r.ArchiveSHA256 != a.SHA256 {
		return nil, errors.New("controlled Codex receipt does not match release")
	}
	if r.CargoVersion == "" || r.VoiceTarget == "" || r.RunnerArch == "" || r.RustcVV == "" || r.CargoV == "" || r.VoiceHostLDD == "" || r.QualificationWorkflow != "controlled-runtime-release" || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(r.QualificationRunID) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(r.QualificationRunAttempt) || !sha256Pattern.MatchString(r.RustyV8ArchiveSHA256) || !sha256Pattern.MatchString(r.RustyV8BindingSHA256) || len(r.FilesSHA256) == 0 {
		return nil, errors.New("incomplete controlled Codex receipt")
	}
	for name, digest := range r.FilesSHA256 {
		clean := path.Clean(name)
		if clean == "." || clean != name || path.IsAbs(name) || strings.HasPrefix(clean, "../") || !sha256Pattern.MatchString(digest) {
			return nil, errors.New("invalid controlled Codex receipt file digest")
		}
	}
	return &r, nil
}
func checksumFor(body []byte, name string) string {
	found := ""
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if found != "" {
				return ""
			}
			found = f[0]
		}
	}
	return found
}

type controlledCapturePackage struct {
	LayoutVersion int    `json:"layoutVersion"`
	Version       string `json:"version"`
	Target        string `json:"target"`
	Variant       string `json:"variant"`
	Entrypoint    string `json:"entrypoint"`
	ResourcesDir  string `json:"resourcesDir"`
	PathDir       string `json:"pathDir"`
}

func controlledCapturePackageIdentity(root, target, version string) error {
	body, e := controlledReadFile(filepath.Join(root, "codex-package.json"), 1<<20)
	if e != nil {
		return e
	}
	var packageIdentity controlledCapturePackage
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&packageIdentity); e != nil {
		return e
	}
	if packageIdentity.LayoutVersion != 1 || packageIdentity.Version != version ||
		packageIdentity.Target != target || packageIdentity.Variant != "codex" ||
		packageIdentity.Entrypoint != "bin/codex" ||
		packageIdentity.ResourcesDir != "codex-resources" ||
		packageIdentity.PathDir != "codex-path" {
		return errors.New("installed controlled Codex package identity mismatch")
	}
	entrypoint, e := os.Lstat(filepath.Join(root, packageIdentity.Entrypoint))
	if e != nil || !entrypoint.Mode().IsRegular() || entrypoint.Mode().Perm()&0111 == 0 {
		return errors.New("installed controlled Codex entrypoint missing or non-executable")
	}
	return nil
}

func controlledCaptureArchiveModes(root string, archive []byte, receipt *ControlledCodexReceipt) error {
	z, e := zstd.NewReader(bytes.NewReader(archive), zstd.WithDecoderMaxMemory(2<<30))
	if e != nil {
		return e
	}
	defer z.Close()
	tr := tar.NewReader(z)
	seen := map[string]bool{}
	var expanded int64
	entries := 0
	for {
		h, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nextErr
		}
		entries++
		if entries > 100000 {
			return errors.New("too many controlled Codex archive entries")
		}
		raw := h.Name
		name := path.Clean(raw)
		if h.Typeflag == tar.TypeDir && strings.HasSuffix(raw, "/") && strings.TrimSuffix(raw, "/") == name {
			raw = name
		}
		if name == "." || path.IsAbs(name) || raw != name || strings.HasPrefix(name, "../") {
			return errors.New("unsafe controlled Codex archive path")
		}
		if seen[name] {
			return errors.New("duplicate controlled Codex archive member")
		}
		seen[name] = true
		installed := filepath.Join(root, filepath.FromSlash(name))
		if rel, relErr := filepath.Rel(root, installed); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return errors.New("controlled Codex archive escaped installed root")
		}
		info, statErr := os.Lstat(installed)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != os.FileMode(h.Mode)&0777 {
			return errors.New("installed controlled Codex type or mode differs from archive")
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if !info.IsDir() {
				return errors.New("installed controlled Codex type differs from archive")
			}
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || h.Size > (2<<30)-expanded {
				return errors.New("controlled Codex archive expands too large")
			}
			expanded += h.Size
			if !info.Mode().IsRegular() || info.Size() != h.Size {
				return errors.New("installed controlled Codex size differs from archive")
			}
			hash := sha256.New()
			if _, copyErr := io.Copy(hash, io.LimitReader(tr, h.Size+1)); copyErr != nil {
				return copyErr
			}
			if hex.EncodeToString(hash.Sum(nil)) != receipt.FilesSHA256[name] {
				return errors.New("controlled Codex archive differs from receipt")
			}
		default:
			return errors.New("controlled Codex archive contains non-regular member")
		}
	}
	return nil
}

func controlledCaptureInstalled(home string, s *ControlledCodex) error {
	target := ControlledCodexTarget()
	if target == "" {
		return errors.New("unsupported controlled Codex platform")
	}
	a := s.Assets[target]
	root := filepath.Join(home, ".local/share/codex-controlled/releases", s.Tag+"-"+target+"-"+a.SHA256[:16])
	wanted := filepath.Join(root, "bin/codex")
	actual, e := os.Readlink(filepath.Join(home, ".local/bin/codex-controlled"))
	if e != nil || actual != wanted {
		return errors.New("controlled Codex is not activated at the pinned release")
	}
	archive, e := controlledReadFile(filepath.Join(root, ".artifact.tar.zst"), 512<<20)
	if e != nil || digestOf(archive) != a.SHA256 {
		return errors.New("installed controlled Codex artifact differs")
	}
	receiptBody, e := controlledReadFile(filepath.Join(root, ".receipt.json"), 16<<20)
	if e != nil || digestOf(receiptBody) != a.ReceiptSHA256 {
		return errors.New("installed controlled Codex receipt differs")
	}
	receipt, e := DecodeControlledCodexReceipt(receiptBody, s, target)
	if e != nil {
		return e
	}
	if e = controlledCaptureArchiveModes(root, archive, receipt); e != nil {
		return e
	}
	if e = controlledCapturePackageIdentity(root, target, receipt.CargoVersion); e != nil {
		return e
	}
	for name, digest := range receipt.FilesSHA256 {
		clean := path.Clean(name)
		body, readErr := controlledReadFile(filepath.Join(root, filepath.FromSlash(clean)), 512<<20)
		if readErr != nil || digestOf(body) != digest {
			return errors.New("installed controlled Codex tree differs from receipt")
		}
	}
	seen := 0
	e = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		name := filepath.ToSlash(rel)
		if name == ".artifact.tar.zst" || name == ".receipt.json" {
			return nil
		}
		if !d.Type().IsRegular() || receipt.FilesSHA256[name] == "" {
			return errors.New("installed controlled Codex file set differs from receipt")
		}
		seen++
		return nil
	})
	if e != nil || seen != len(receipt.FilesSHA256) {
		return errors.New("installed controlled Codex file set differs from receipt")
	}
	return nil
}

func controlledReadFile(name string, limit int64) ([]byte, error) {
	i, e := os.Lstat(name)
	if e != nil || !i.Mode().IsRegular() || i.Size() > limit {
		return nil, errors.New("controlled Codex file missing, special, or too large")
	}
	return os.ReadFile(name)
}

func controlledCaptureStage(home string) (string, error) {
	base := filepath.Join(home, ".local/state/dev-fleet/codex-controlled/capture")
	for p := filepath.Dir(base); p != home && p != filepath.Dir(p); p = filepath.Dir(p) {
		if i, e := os.Lstat(p); e == nil && i.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("refusing controlled Codex capture under symlink parent")
		}
	}
	if e := os.MkdirAll(base, 0700); e != nil {
		return "", e
	}
	if i, e := os.Lstat(base); e != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("refusing unsafe controlled Codex capture staging directory")
	}
	return os.MkdirTemp(base, "capture-")
}

func CaptureControlledCodex(home string, runner procx.Runner, raw map[string]any) (map[string]any, error) {
	s, e := DecodeControlledCodex(raw)
	if e != nil {
		return nil, e
	}
	if e = controlledCaptureInstalled(home, s); e != nil {
		return nil, e
	}
	stage, e := controlledCaptureStage(home)
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(stage)
	args := []string{"gh", "release", "download", s.Tag, "--repo", s.Repository, "--pattern", "SHA256SUMS", "--dir", stage}
	if r := runner(procx.Command{Argv: args, Timeout: 90 * time.Second}); !r.OK() {
		return nil, errors.New("cannot fetch controlled Codex checksums")
	}
	sums, e := controlledReadFile(filepath.Join(stage, "SHA256SUMS"), 1<<20)
	if e != nil {
		return nil, e
	}
	for target, a := range s.Assets {
		if checksumFor(sums, a.Asset) != a.SHA256 || checksumFor(sums, a.ReceiptAsset) != a.ReceiptSHA256 {
			return nil, errors.New("controlled Codex manifest differs from SHA256SUMS")
		}
		args = []string{"gh", "release", "download", s.Tag, "--repo", s.Repository, "--pattern", a.ReceiptAsset, "--dir", stage}
		if r := runner(procx.Command{Argv: args, Timeout: 90 * time.Second}); !r.OK() {
			return nil, errors.New("cannot fetch controlled Codex receipt")
		}
		body, e := controlledReadFile(filepath.Join(stage, a.ReceiptAsset), 16<<20)
		if e != nil || digestOf(body) != a.ReceiptSHA256 {
			return nil, errors.New("controlled Codex receipt digest mismatch")
		}
		if _, e = DecodeControlledCodexReceipt(body, s, target); e != nil {
			return nil, e
		}
	}
	encoded, e := canonjson.Compact(s)
	if e != nil {
		return nil, e
	}
	out, e := canonjson.DecodeObject(encoded)
	if e != nil {
		return nil, e
	}
	if _, e = DecodeControlledCodex(out); e != nil {
		return nil, fmt.Errorf("captured controlled Codex: %w", e)
	}
	return out, nil
}
