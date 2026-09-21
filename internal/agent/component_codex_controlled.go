package agent

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/iryzhkov/UpKeeper/internal/agentenv"
	"github.com/iryzhkov/UpKeeper/internal/release"
	"github.com/klauspost/compress/zstd"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const controlledCodexBinary = ".local/bin/codex-controlled"
const controlledArchiveLimit int64 = 512 << 20
const controlledExpandedLimit int64 = 2 << 30

func init() {
	Register(release.ControlledCodexComponent, Handler{Observe: controlledCodexObserve, Apply: controlledCodexApply})
}
func controlledCodexHost(s *release.ControlledCodex, h string) bool {
	for _, x := range s.Hosts {
		if x == h {
			return true
		}
	}
	return false
}
func controlledCodexRelease(c *Context, s *release.ControlledCodex, target string) string {
	return c.Path(".local/share/codex-controlled/releases", s.Tag+"-"+target+"-"+s.Assets[target].SHA256[:16])
}
func controlledCodexLink(c *Context) string { return c.Path(controlledCodexBinary) }
func controlledSafeLink(c *Context, p string) error {
	rel, e := filepath.Rel(c.Home, p)
	if e != nil {
		return e
	}
	if _, e = agentenv.TargetPath(c.Home, rel); e != nil {
		return e
	}
	i, e := os.Lstat(p)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if i.Mode()&os.ModeSymlink == 0 {
		return errors.New("refusing non-symlink controlled Codex entry point")
	}
	return nil
}
func controlledAtomicSymlink(target, link string) error {
	if e := os.MkdirAll(filepath.Dir(link), 0755); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(link), ".codex-controlled-link-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	if e = f.Close(); e != nil {
		os.Remove(tmp)
		return e
	}
	if e = os.Remove(tmp); e != nil {
		return e
	}
	defer os.Remove(tmp)
	if e = os.Symlink(target, tmp); e != nil {
		return e
	}
	return os.Rename(tmp, link)
}

func controlledStage(c *Context, prefix string) (string, error) {
	rel := ".local/state/dev-fleet/codex-controlled/staging"
	base, e := agentenv.TargetPath(c.Home, rel)
	if e != nil {
		return "", e
	}
	if e = os.MkdirAll(base, 0700); e != nil {
		return "", e
	}
	if base, e = agentenv.TargetPath(c.Home, rel); e != nil {
		return "", e
	}
	return os.MkdirTemp(base, prefix)
}

func controlledReadBounded(name string, limit int64) ([]byte, error) {
	i, e := os.Lstat(name)
	if e != nil || !i.Mode().IsRegular() || i.Size() > limit {
		return nil, errors.New("controlled Codex file missing, special, or too large")
	}
	return os.ReadFile(name)
}
func controlledExtract(archive []byte, dest string) error {
	if int64(len(archive)) > controlledArchiveLimit {
		return errors.New("controlled Codex archive too large")
	}
	z, e := zstd.NewReader(bytes.NewReader(archive), zstd.WithDecoderMaxMemory(uint64(controlledExpandedLimit)))
	if e != nil {
		return e
	}
	defer z.Close()
	tr := tar.NewReader(z)
	var total int64
	entries := 0
	seen := map[string]bool{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
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
		target := filepath.Join(dest, filepath.FromSlash(name))
		if rel, e := filepath.Rel(dest, target); e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return errors.New("controlled Codex archive escaped destination")
		}
		mode := fs.FileMode(h.Mode) & 0777
		switch h.Typeflag {
		case tar.TypeDir:
			if e = os.MkdirAll(target, mode); e != nil {
				return e
			}
			if e = os.Chmod(target, mode); e != nil {
				return e
			}
		case tar.TypeReg, tar.TypeRegA:
			total += h.Size
			if h.Size < 0 || total > controlledExpandedLimit {
				return errors.New("controlled Codex archive expands too large")
			}
			if e = os.MkdirAll(filepath.Dir(target), 0755); e != nil {
				return e
			}
			f, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			_, copyErr := io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if e = os.Chmod(target, mode); e != nil {
				return e
			}
		default:
			return errors.New("controlled Codex archive contains non-regular member")
		}
	}
	return nil
}

type controlledPackage struct {
	LayoutVersion int    `json:"layoutVersion"`
	Version       string `json:"version"`
	Variant       string `json:"variant"`
	Target        string `json:"target"`
	Entrypoint    string `json:"entrypoint"`
	ResourcesDir  string `json:"resourcesDir"`
	PathDir       string `json:"pathDir"`
}

func controlledVerifyTree(root, target, version string) error {
	info, e := os.Lstat(filepath.Join(root, "bin/codex"))
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("controlled Codex entrypoint missing or non-executable")
	}
	body, e := controlledReadBounded(filepath.Join(root, "codex-package.json"), 1<<20)
	if e != nil {
		return e
	}
	var p controlledPackage
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if e = d.Decode(&p); e != nil {
		return e
	}
	if p.LayoutVersion != 1 || p.Version != version || p.Variant != "codex" || p.Target != target || p.Entrypoint != "bin/codex" || p.ResourcesDir != "codex-resources" || p.PathDir != "codex-path" {
		return errors.New("controlled Codex package identity mismatch")
	}
	return filepath.WalkDir(root, func(_ string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("controlled Codex installed tree contains link")
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return errors.New("controlled Codex installed tree contains special file")
		}
		return nil
	})
}

func controlledFileDigest(name string, limit int64) (string, int64, error) {
	f, e := os.Open(name)
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() > limit {
		return "", 0, errors.New("controlled Codex file missing, special, or too large")
	}
	h := sha256.New()
	if _, e = io.Copy(h, io.LimitReader(f, limit+1)); e != nil {
		return "", 0, e
	}
	return hex.EncodeToString(h.Sum(nil)), i.Size(), nil
}

func controlledTreesEqual(left, right string) error {
	compare := func(root, other string) error {
		return filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(root, p)
			if e != nil {
				return e
			}
			if rel == ".artifact.tar.zst" || rel == ".receipt.json" {
				return nil
			}
			q := filepath.Join(other, rel)
			oi, e := os.Lstat(q)
			if e != nil {
				return errors.New("controlled Codex installed tree differs from artifact")
			}
			i, e := d.Info()
			if e != nil {
				return e
			}
			if i.IsDir() != oi.IsDir() || i.Mode().Perm() != oi.Mode().Perm() {
				return errors.New("controlled Codex installed tree mode differs from artifact")
			}
			if i.Mode().IsRegular() {
				if i.Size() != oi.Size() || i.Size() > controlledExpandedLimit {
					return errors.New("controlled Codex installed tree size differs from artifact")
				}
				a, _, digestErr := controlledFileDigest(p, controlledExpandedLimit)
				b, _, otherErr := controlledFileDigest(q, controlledExpandedLimit)
				if digestErr != nil || otherErr != nil || a != b {
					return errors.New("controlled Codex installed tree content differs from artifact")
				}
			}
			return nil
		})
	}
	if e := compare(left, right); e != nil {
		return e
	}
	return compare(right, left)
}
func controlledVerifyReceiptFiles(root string, receipt *release.ControlledCodexReceipt) error {
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		name := filepath.ToSlash(rel)
		if name == ".artifact.tar.zst" || name == ".receipt.json" {
			return nil
		}
		if !d.Type().IsRegular() {
			return errors.New("controlled Codex installed tree contains special file")
		}
		digest, _, digestErr := controlledFileDigest(p, controlledExpandedLimit)
		if digestErr != nil {
			return digestErr
		}
		wanted, ok := receipt.FilesSHA256[name]
		if !ok || digest != wanted {
			return errors.New("controlled Codex file differs from receipt")
		}
		seen[name] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(receipt.FilesSHA256) {
		return errors.New("controlled Codex receipt file set differs")
	}
	return nil
}

func controlledVerifyInstalled(c *Context, root, target, digest string, s *release.ControlledCodex) error {
	archive, e := controlledReadBounded(filepath.Join(root, ".artifact.tar.zst"), controlledArchiveLimit)
	if e != nil || agentenv.Digest(archive) != digest {
		return errors.New("controlled Codex retained artifact differs")
	}
	receiptBody, e := controlledReadBounded(filepath.Join(root, ".receipt.json"), 16<<20)
	if e != nil || agentenv.Digest(receiptBody) != s.Assets[target].ReceiptSHA256 {
		return errors.New("controlled Codex retained receipt differs")
	}
	receipt, e := release.DecodeControlledCodexReceipt(receiptBody, s, target)
	if e != nil {
		return e
	}
	stage, e := controlledStage(c, "verify-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	if e = controlledExtract(archive, stage); e != nil {
		return e
	}
	if e = controlledVerifyTree(stage, target, receipt.CargoVersion); e != nil {
		return e
	}
	if e = controlledVerifyReceiptFiles(stage, receipt); e != nil {
		return e
	}
	return controlledTreesEqual(stage, root)
}

func controlledCodexObserve(c *Context, raw map[string]any) (map[string]any, error) {
	s, e := release.DecodeControlledCodex(raw)
	if e != nil {
		return nil, e
	}
	if !controlledCodexHost(s, c.Host.Name) {
		return map[string]any{"converged": true, "configured": false}, nil
	}
	target := release.ControlledCodexTarget()
	if target == "" {
		return nil, errors.New("unsupported controlled Codex platform")
	}
	root := controlledCodexRelease(c, s, target)
	wanted := filepath.Join(root, "bin/codex")
	link := controlledCodexLink(c)
	actual, e := os.Readlink(link)
	ok := e == nil && actual == wanted && controlledVerifyInstalled(c, root, target, s.Assets[target].SHA256, s) == nil
	return map[string]any{"converged": ok, "configured": true, "tag": s.Tag, "target": target}, nil
}

func controlledRestoreReplacedRoot(root, backup string) error {
	if backup == "" {
		return nil
	}
	if e := os.RemoveAll(root); e != nil {
		return e
	}
	return os.Rename(backup, root)
}

var controlledCodexPostActivate = controlledCodexObserve

func controlledCodexApply(c *Context, raw, before map[string]any, actions *[]string) (string, error) {
	s, e := release.DecodeControlledCodex(raw)
	if e != nil {
		return "", e
	}
	if before["converged"] == true {
		return "converged", nil
	}
	if !controlledCodexHost(s, c.Host.Name) {
		return "converged", nil
	}
	if c.DryRun {
		*actions = append(*actions, "would install checksum-pinned controlled Codex runtime bundle")
		return "updated", nil
	}
	target := release.ControlledCodexTarget()
	if target == "" {
		return "", errors.New("unsupported controlled Codex platform")
	}
	a := s.Assets[target]
	stage, e := controlledStage(c, "download-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(stage)
	for _, asset := range []string{a.Asset, a.ReceiptAsset} {
		r := c.Run([]string{"gh", "release", "download", s.Tag, "--repo", s.Repository, "--pattern", asset, "--dir", stage}, 2*time.Minute, c.Home)
		if !r.OK {
			return "", errors.New("controlled Codex release download failed")
		}
	}
	archive, e := controlledReadBounded(filepath.Join(stage, a.Asset), controlledArchiveLimit)
	if e != nil || int64(len(archive)) > controlledArchiveLimit || agentenv.Digest(archive) != a.SHA256 {
		return "", errors.New("controlled Codex archive digest or size mismatch")
	}
	receiptBody, e := controlledReadBounded(filepath.Join(stage, a.ReceiptAsset), 16<<20)
	if e != nil || agentenv.Digest(receiptBody) != a.ReceiptSHA256 {
		return "", errors.New("controlled Codex receipt digest mismatch")
	}
	receipt, e := release.DecodeControlledCodexReceipt(receiptBody, s, target)
	if e != nil {
		return "", e
	}
	root := controlledCodexRelease(c, s, target)
	parent := filepath.Dir(root)
	if _, e = agentenv.TargetPath(c.Home, filepath.Join(".local/share/codex-controlled/releases", filepath.Base(root))); e != nil {
		return "", e
	}
	if e = os.MkdirAll(parent, 0755); e != nil {
		return "", e
	}
	candidate, e := os.MkdirTemp(parent, ".staging-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(candidate)
	if e = controlledExtract(archive, candidate); e != nil {
		return "", e
	}
	if e = controlledVerifyTree(candidate, target, receipt.CargoVersion); e != nil {
		return "", e
	}
	if e = controlledVerifyReceiptFiles(candidate, receipt); e != nil {
		return "", e
	}
	if e = os.WriteFile(filepath.Join(candidate, ".artifact.tar.zst"), archive, 0600); e != nil {
		return "", e
	}
	if e = os.WriteFile(filepath.Join(candidate, ".receipt.json"), receiptBody, 0600); e != nil {
		return "", e
	}
	link := controlledCodexLink(c)
	if e = controlledSafeLink(c, link); e != nil {
		return "", e
	}
	previous, previousErr := os.Readlink(link)
	hadPrevious := previousErr == nil
	backup := ""
	if _, e = os.Lstat(root); os.IsNotExist(e) {
		if e = os.Rename(candidate, root); e != nil {
			return "", e
		}
	} else if e != nil {
		return "", e
	} else if e = controlledVerifyInstalled(c, root, target, a.SHA256, s); e != nil {
		backup = root + ".replaced-" + c.RunID
		if _, statErr := os.Lstat(backup); !os.IsNotExist(statErr) {
			return "", errors.New("controlled Codex repair backup already exists")
		}
		if e = os.Rename(root, backup); e != nil {
			return "", e
		}
		if e = os.Rename(candidate, root); e != nil {
			_ = os.Rename(backup, root)
			return "", e
		}
	}
	wanted := filepath.Join(root, "bin/codex")
	currentBefore, currentErr := os.Readlink(link)
	if hadPrevious {
		if currentErr != nil || currentBefore != previous {
			if restoreErr := controlledRestoreReplacedRoot(root, backup); restoreErr != nil {
				return "", Failf("controlled Codex pre-activation tree rollback failed")
			}
			return "", errors.New("controlled Codex entry point changed before activation")
		}
	} else if _, statErr := os.Lstat(link); !os.IsNotExist(statErr) {
		if restoreErr := controlledRestoreReplacedRoot(root, backup); restoreErr != nil {
			return "", Failf("controlled Codex pre-activation tree rollback failed")
		}
		return "", errors.New("controlled Codex entry point appeared before activation")
	}
	if e = controlledAtomicSymlink(wanted, link); e != nil {
		if restoreErr := controlledRestoreReplacedRoot(root, backup); restoreErr != nil {
			return "", Failf("controlled Codex pre-activation tree rollback failed")
		}
		return "", e
	}
	after, e := controlledCodexPostActivate(c, raw)
	verificationErr := e
	if verificationErr == nil && after["converged"] != true {
		verificationErr = errors.New("post-activation observation was not converged")
	}
	if verificationErr == nil {
		if backup != "" {
			if removeErr := os.RemoveAll(backup); removeErr != nil {
				return "", Failf("controlled Codex installed but obsolete corrupt tree cleanup failed")
			}
		}
		*actions = append(*actions, "installed verified controlled Codex runtime bundle")
		return "updated", nil
	}
	current, _ := os.Readlink(link)
	if current != wanted {
		return "", Failf("controlled Codex activation failed; concurrent entry point preserved")
	}
	if hadPrevious {
		e = controlledAtomicSymlink(previous, link)
	} else {
		e = os.Remove(link)
	}
	if e != nil {
		return "", Failf("controlled Codex rollback failed")
	}
	if backup != "" {
		if removeErr := os.RemoveAll(root); removeErr != nil || os.Rename(backup, root) != nil {
			return "", Failf("controlled Codex tree rollback failed")
		}
	}
	return "", RolledBackf("controlled Codex activation reverted after verification failure: %s", fmt.Sprint(verificationErr))
}
