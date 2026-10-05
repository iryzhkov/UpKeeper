package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/iryzhkov/UpKeeper/internal/canonjson"
)

// Limits apply before acquisition and allocation and while consuming the gzip stream.
const (
	T3ArchiveLimit  int64 = 512 << 20
	T3ExpandedLimit int64 = 2 << 30
	T3FileLimit     int64 = 512 << 20
	T3ReceiptLimit  int64 = 16 << 20
	T3EntryLimit          = 100000
	T3StatePath           = ".local/state/dev-fleet/t3-controlled"
)

type T3Asset struct {
	Asset         string `json:"asset"`
	SHA256        string `json:"sha256"`
	ReceiptAsset  string `json:"receipt_asset"`
	ReceiptSHA256 string `json:"receipt_sha256"`
}
type T3Artifact struct {
	Repository   string             `json:"repository"`
	Tag          string             `json:"tag"`
	SourceCommit string             `json:"source_commit"`
	SourceTree   string             `json:"source_tree"`
	Assets       map[string]T3Asset `json:"assets"`
}
type T3File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   int64  `json:"mode"`
}
type T3Receipt struct {
	Schema        string   `json:"schema"`
	SourceCommit  string   `json:"source_commit"`
	SourceTree    string   `json:"source_tree"`
	Platform      string   `json:"platform"`
	BaseVersion   string   `json:"base_version"`
	Repository    string   `json:"repository"`
	Archive       string   `json:"archive"`
	ArchiveSHA256 string   `json:"archive_sha256"`
	Files         []T3File `json:"files"`
}

var t3Hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)
var t3Tag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func T3Target() string {
	if runtime.GOOS == "linux" {
		switch runtime.GOARCH {
		case "amd64":
			return "linux-x64"
		case "arm64":
			return "linux-arm64"
		}
	}
	return ""
}
func T3Digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func t3Decode(b []byte, v any) error {
	if _, e := canonjson.Decode(b); e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func DecodeT3Artifact(raw any, version string) (*T3Artifact, error) {
	m, ok := raw.(map[string]any)
	if !ok || !hasExactKeys(m, "repository", "tag", "source_commit", "source_tree", "assets") {
		return nil, errors.New("invalid T3 artifact fields")
	}
	b, e := canonjson.Compact(m)
	if e != nil {
		return nil, e
	}
	var s T3Artifact
	if e = t3Decode(b, &s); e != nil {
		return nil, e
	}
	if s.Repository != "iryzhkov/t3code" || !t3Tag.MatchString(s.Tag) ||
		!t3Hex40.MatchString(s.SourceCommit) || !t3Hex40.MatchString(s.SourceTree) || !numericVersionPattern.MatchString(version) {
		return nil, errors.New("invalid controlled T3 identity")
	}
	assets, ok := m["assets"].(map[string]any)
	if !ok || !hasExactKeys(assets, "linux-x64", "linux-arm64") {
		return nil, errors.New("T3 requires both exact platform assets")
	}
	for _, target := range []string{"linux-x64", "linux-arm64"} {
		entry, ok := assets[target].(map[string]any)
		a := s.Assets[target]
		name := "t3-fleet-" + version + "-" + target + ".tar.gz"
		if !ok || !hasExactKeys(entry, "asset", "sha256", "receipt_asset", "receipt_sha256") ||
			a.Asset != name || a.ReceiptAsset != strings.TrimSuffix(name, ".tar.gz")+".receipt.json" ||
			!sha256Pattern.MatchString(a.SHA256) || !sha256Pattern.MatchString(a.ReceiptSHA256) {
			return nil, errors.New("invalid controlled T3 asset")
		}
	}
	return &s, nil
}
func t3CanonicalPath(p string) bool {
	return p != "" && p != "." && path.Clean(p) == p && !strings.HasPrefix(p, "/") &&
		!strings.Contains(p, "\\") && !strings.Contains(p, "\x00") && p != ".." && !strings.HasPrefix(p, "../")
}
func DecodeT3Receipt(b []byte, s *T3Artifact, target, version string) (*T3Receipt, error) {
	if int64(len(b)) > T3ReceiptLimit || s == nil || s.Assets[target].Asset == "" {
		return nil, errors.New("invalid T3 receipt target or limit")
	}
	m, e := canonjson.DecodeObject(b)
	if e != nil {
		return nil, e
	}
	if !hasExactKeys(m, "schema", "source_commit", "source_tree", "platform", "base_version", "repository", "archive", "archive_sha256", "files") {
		return nil, errors.New("invalid T3 receipt fields")
	}
	var r T3Receipt
	if e = t3Decode(b, &r); e != nil {
		return nil, e
	}
	a := s.Assets[target]
	if r.Schema != "upkeeper-t3-receipt/v1" || r.SourceCommit != s.SourceCommit || r.SourceTree != s.SourceTree ||
		r.Platform != target || r.BaseVersion != version || r.Repository != s.Repository || r.Archive != a.Asset || r.ArchiveSHA256 != a.SHA256 {
		return nil, errors.New("T3 receipt identity differs")
	}
	raw, ok := m["files"].([]any)
	if !ok || len(raw) == 0 || len(raw) > T3EntryLimit {
		return nil, errors.New("invalid T3 receipt file count")
	}
	seen := map[string]bool{}
	var total int64
	for i, f := range r.Files {
		fm, ok := raw[i].(map[string]any)
		if !ok || !hasExactKeys(fm, "path", "sha256", "size", "mode") || !t3CanonicalPath(f.Path) || seen[f.Path] ||
			!sha256Pattern.MatchString(f.SHA256) || f.Size < 0 || f.Size > T3FileLimit || f.Mode < 0 || f.Mode > 0777 {
			return nil, errors.New("invalid T3 receipt file")
		}
		// json decoding into int64 rejects fractional numbers, booleans and exponents.
		seen[f.Path] = true
		total += f.Size
		if total > T3ExpandedLimit {
			return nil, errors.New("T3 payload too large")
		}
	}
	for p := range seen {
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return nil, errors.New("T3 file/directory conflict")
			}
		}
	}
	return &r, nil
}
func t3Directories(r *T3Receipt) map[string]bool {
	dirs := map[string]bool{".": true}
	for _, f := range r.Files {
		for p := path.Dir(f.Path); p != "."; p = path.Dir(p) {
			dirs[p] = true
		}
	}
	return dirs
}

// VerifyT3Bundle optionally extracts into an empty private directory. It never
// accepts archive metadata as ownership, a symlink, or an unlisted payload.
func VerifyT3Bundle(archive, receipt []byte, s *T3Artifact, target, version, dest string) (*T3Receipt, error) {
	if int64(len(archive)) > T3ArchiveLimit || s == nil {
		return nil, errors.New("T3 archive limit or identity")
	}
	a := s.Assets[target]
	if T3Digest(archive) != a.SHA256 || T3Digest(receipt) != a.ReceiptSHA256 {
		return nil, errors.New("T3 download digest differs")
	}
	r, e := DecodeT3Receipt(receipt, s, target, version)
	if e != nil {
		return nil, e
	}
	gz, e := gzip.NewReader(bytes.NewReader(archive))
	if e != nil {
		return nil, e
	}
	defer gz.Close()
	// gzip/tar headers and padding are bounded as well as listed file sizes.
	limited := &io.LimitedReader{R: gz, N: T3ExpandedLimit + 1}
	tr := tar.NewReader(limited)
	files := map[string]T3File{}
	for _, f := range r.Files {
		files[f.Path] = f
	}
	dirs := t3Directories(r)
	seen := map[string]bool{}
	n := 0
	if dest != "" {
		entries, e := os.ReadDir(dest)
		if e != nil || len(entries) != 0 {
			return nil, errors.New("T3 extraction destination must be empty")
		}
	}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		n++
		if n > T3EntryLimit {
			return nil, errors.New("T3 archive entry limit")
		}
		name := h.Name
		if h.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if name != "t3" && (!strings.HasPrefix(name, "t3/") || !t3CanonicalPath(strings.TrimPrefix(name, "t3/"))) {
			return nil, errors.New("noncanonical T3 archive path")
		}
		rel := strings.TrimPrefix(name, "t3/")
		if name == "t3" {
			rel = "."
		}
		if seen[rel] {
			return nil, errors.New("duplicate T3 archive entry")
		}
		seen[rel] = true
		if h.Mode < 0 || h.Mode > 0777 || h.Linkname != "" {
			return nil, errors.New("unsafe T3 archive mode or link")
		}
		if h.Typeflag == tar.TypeDir {
			if !dirs[rel] || h.Size != 0 {
				return nil, errors.New("unlisted T3 directory")
			}
			if dest != "" {
				if e = os.MkdirAll(filepath.Join(dest, filepath.FromSlash(rel)), 0755); e != nil {
					return nil, e
				}
			}
			continue
		}
		f, ok := files[rel]
		if !ok || h.Typeflag != tar.TypeReg || h.Size != f.Size || h.Mode != f.Mode {
			return nil, errors.New("T3 archive file identity differs")
		}
		hash := sha256.New()
		var out *os.File
		var writer io.Writer = hash
		if dest != "" {
			p := filepath.Join(dest, filepath.FromSlash(rel))
			if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
				return nil, e
			}
			out, e = os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return nil, e
			}
			writer = io.MultiWriter(hash, out)
		}
		count, err := io.CopyN(writer, tr, f.Size)
		if out != nil {
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err == nil {
				err = os.Chmod(out.Name(), os.FileMode(f.Mode))
			}
		}
		if err != nil || count != f.Size || hex.EncodeToString(hash.Sum(nil)) != f.SHA256 {
			return nil, errors.New("T3 archive content differs")
		}
	}
	// Consume to EOF to check gzip checksum and reject nonzero trailing tar data.
	buffer := make([]byte, 65536)
	invalidTail := false
	for {
		n, err := limited.Read(buffer)
		if len(bytes.Trim(buffer[:n], "\x00")) != 0 {
			invalidTail = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if limited.N == 0 || invalidTail {
		return nil, errors.New("T3 expanded limit or trailing archive data")
	}
	for p := range files {
		if !seen[p] {
			return nil, errors.New("missing T3 archive file")
		}
	}
	if !seen["."] {
		return nil, errors.New("missing canonical T3 root")
	}
	return r, nil
}
func T3ReadFile(p string, limit int64) ([]byte, error) {
	i, e := os.Lstat(p)
	if e != nil || !i.Mode().IsRegular() || i.Size() > limit {
		return nil, fmt.Errorf("T3 evidence missing, special or oversized: %s", p)
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("T3 file limit")
	}
	return b, e
}
func VerifyT3Tree(root string, r *T3Receipt, s *T3Artifact) error {
	dirs := t3Directories(r)
	files := map[string]T3File{}
	for _, f := range r.Files {
		files[f.Path] = f
	}
	seen := 0
	e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if !dirs[rel] {
				return errors.New("extra T3 installed directory")
			}
			return nil
		}
		f, ok := files[rel]
		if !ok || !d.Type().IsRegular() {
			return errors.New("extra or special installed T3 file")
		}
		i, e := d.Info()
		if e != nil {
			return e
		}
		b, e := T3ReadFile(p, T3FileLimit)
		if e != nil || i.Size() != f.Size || int64(i.Mode().Perm()) != f.Mode || i.Mode()&os.ModeType != 0 || i.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || T3Digest(b) != f.SHA256 {
			return errors.New("installed T3 content differs")
		}
		seen++
		return nil
	})
	if e != nil {
		return e
	}
	if seen != len(files) {
		return errors.New("missing installed T3 file")
	}
	// Complete modern launcher and nested dependency, never ancestor resolution.
	pkgBody, e := T3ReadFile(filepath.Join(root, "package.json"), T3ReceiptLimit)
	if e != nil {
		return e
	}
	pkg, e := canonjson.DecodeObject(pkgBody)
	if e != nil {
		return e
	}
	metadata, ok := pkg["upkeeperControlledBuild"].(map[string]any)
	if !ok || !hasExactKeys(metadata, "schema", "source_commit", "source_tree", "platform", "base_version") ||
		metadata["schema"] != "upkeeper-t3-build/v1" || metadata["source_commit"] != s.SourceCommit || metadata["source_tree"] != s.SourceTree ||
		metadata["platform"] != r.Platform || metadata["base_version"] != r.BaseVersion || pkg["name"] != "t3" || pkg["version"] != r.BaseVersion {
		return errors.New("T3 build metadata differs")
	}
	bin, _ := pkg["bin"].(map[string]any)
	optional, _ := pkg["optionalDependencies"].(map[string]any)
	platform := "@t3code/t3-" + r.Platform
	native := path.Join("node_modules", platform, "t3")
	if bin["t3"] != "./bin/t3.js" || optional[platform] != r.BaseVersion || files["bin/t3.js"].Mode&0111 == 0 || files[native].Mode&0111 == 0 {
		return errors.New("incomplete controlled T3 launcher or binary")
	}
	body, e := T3ReadFile(filepath.Join(root, "node_modules", platform, "package.json"), T3ReceiptLimit)
	if e != nil {
		return e
	}
	p, e := canonjson.DecodeObject(body)
	if e != nil {
		return e
	}
	osList, _ := p["os"].([]any)
	cpu, _ := p["cpu"].([]any)
	arch := strings.TrimPrefix(r.Platform, "linux-")
	if p["name"] != platform || p["version"] != r.BaseVersion || len(osList) != 1 || osList[0] != "linux" || len(cpu) != 1 || cpu[0] != arch {
		return errors.New("controlled T3 platform manifest differs")
	}
	return nil
}

// T3Retained stores verifier inputs outside the payload namespace.
type T3Retained struct {
	Spec     map[string]any
	Artifact *T3Artifact
	Receipt  *T3Receipt
	Prefix   string
	Link     string
}

func ReadT3Retained(home string) (*T3Retained, error) {
	state := filepath.Join(home, T3StatePath, "current")
	realState, e := filepath.EvalSymlinks(state)
	if e != nil || realState != state {
		return nil, errors.New("T3 retained state redirects")
	}
	entries, e := os.ReadDir(state)
	if e != nil || len(entries) != 4 {
		return nil, errors.New("T3 retained evidence set differs")
	}
	b, e := T3ReadFile(filepath.Join(state, "pin.json"), T3ReceiptLimit)
	if e != nil {
		return nil, e
	}
	spec, e := canonjson.DecodeObject(b)
	if e != nil {
		return nil, e
	}
	if !hasExactKeys(spec, "kind", "package", "version", "compatibility", "service", "artifact") || spec["kind"] != "npm-package" || spec["package"] != "t3" || spec["service"] != "t3code.service" {
		return nil, errors.New("invalid retained T3 pin")
	}
	version, _ := spec["version"].(string)
	s, e := DecodeT3Artifact(spec["artifact"], version)
	if e != nil {
		return nil, e
	}
	loc, e := T3ReadFile(filepath.Join(state, "location.json"), T3ReceiptLimit)
	if e != nil {
		return nil, e
	}
	m, e := canonjson.DecodeObject(loc)
	if e != nil || !hasExactKeys(m, "prefix", "link") {
		return nil, errors.New("invalid T3 retained location")
	}
	prefix, _ := m["prefix"].(string)
	link, _ := m["link"].(string)
	real, e := filepath.EvalSymlinks(prefix)
	if e != nil || real != prefix || !filepath.IsAbs(prefix) {
		return nil, errors.New("T3 prefix changed")
	}
	root := filepath.Join(prefix, "lib/node_modules/t3")
	resolved, e := filepath.EvalSymlinks(filepath.Join(prefix, "bin/t3"))
	if e != nil || resolved != filepath.Join(root, "bin/t3.js") {
		return nil, errors.New("T3 launcher mapping differs")
	}
	actual, e := os.Readlink(filepath.Join(prefix, "bin/t3"))
	if e != nil || actual != link {
		return nil, errors.New("T3 link changed")
	}
	archive, e := T3ReadFile(filepath.Join(state, "archive.tar.gz"), T3ArchiveLimit)
	if e != nil {
		return nil, e
	}
	receipt, e := T3ReadFile(filepath.Join(state, "receipt.json"), T3ReceiptLimit)
	if e != nil {
		return nil, e
	}
	r, e := VerifyT3Bundle(archive, receipt, s, T3Target(), version, "")
	if e != nil {
		return nil, e
	}
	if e = VerifyT3Tree(root, r, s); e != nil {
		return nil, e
	}
	return &T3Retained{spec, s, r, prefix, link}, nil
}

// Evidence detection includes retained state and package metadata independently.
// A caller with a previous artifact must require retained verification even if
// these local witnesses have disappeared.
func T3ControlledEvidence(home, prefix string) bool {
	if _, e := os.Lstat(filepath.Join(home, T3StatePath)); !os.IsNotExist(e) {
		return true
	}
	if prefix != "" {
		b, e := T3ReadFile(filepath.Join(prefix, "lib/node_modules/t3/package.json"), T3ReceiptLimit)
		if e == nil {
			m, e := canonjson.DecodeObject(b)
			if e != nil {
				return true
			}
			_, ok := m["upkeeperControlledBuild"]
			return ok
		}
	}
	return false
}

// T3TreeDigest is an exact regular-tree snapshot including directory and file
// modes. Unusual existing symlink packages are refused before activation.
func T3TreeDigest(root string) (string, error) {
	if _, e := os.Lstat(root); e != nil {
		return "", e
	}
	h := sha256.New()
	n := 0
	var total int64
	e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		n++
		if n > T3EntryLimit {
			return errors.New("T3 snapshot entry limit")
		}
		i, e := d.Info()
		if e != nil {
			return e
		}
		if i.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || (!i.IsDir() && !i.Mode().IsRegular()) {
			return errors.New("unusual T3 snapshot entry")
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", filepath.ToSlash(rel), i.Mode(), i.Size())
		if i.Mode().IsRegular() {
			if i.Size() > T3FileLimit {
				return errors.New("T3 snapshot file limit")
			}
			total += i.Size()
			if total > T3ExpandedLimit {
				return errors.New("T3 snapshot expanded limit")
			}
			f, e := os.Open(p)
			if e != nil {
				return e
			}
			count, e := io.Copy(h, io.LimitReader(f, T3FileLimit+1))
			closeErr := f.Close()
			if e != nil {
				return e
			}
			if closeErr != nil {
				return closeErr
			}
			if count != i.Size() {
				return errors.New("T3 snapshot changed while read")
			}
		}
		return nil
	})
	if e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func VerifyT3Upstream(home, prefix, version string) error {
	b, e := T3ReadFile(filepath.Join(home, T3StatePath, "current/upstream.json"), T3ReceiptLimit)
	if e != nil {
		return e
	}
	m, e := canonjson.DecodeObject(b)
	if e != nil || !hasExactKeys(m, "schema", "prefix", "version", "tree_sha256", "link") ||
		m["schema"] != "upkeeper-t3-upstream/v1" || m["prefix"] != prefix || m["version"] != version {
		return errors.New("T3 upstream provenance differs")
	}
	real, e := filepath.EvalSymlinks(prefix)
	if e != nil || real != prefix {
		return errors.New("T3 upstream prefix changed")
	}
	link, e := os.Readlink(filepath.Join(prefix, "bin/t3"))
	if e != nil || link != m["link"] {
		return errors.New("T3 upstream link differs")
	}
	resolved, e := filepath.EvalSymlinks(filepath.Join(prefix, "bin/t3"))
	if e != nil || resolved != filepath.Join(prefix, "lib/node_modules/t3/bin/t3.js") {
		return errors.New("T3 upstream launcher differs")
	}
	digest, e := T3TreeDigest(filepath.Join(prefix, "lib/node_modules/t3"))
	if e != nil || digest != m["tree_sha256"] {
		return errors.New("T3 upstream bytes differ")
	}
	entries, e := os.ReadDir(filepath.Join(home, T3StatePath, "current"))
	if e != nil || len(entries) != 1 {
		return errors.New("T3 conflicting upstream provenance")
	}
	return nil
}
