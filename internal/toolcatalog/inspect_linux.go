//go:build linux

package toolcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Linux O_PATH acquires an inode without opening it for data or device access.
const oPath = 0x200000
const procMagic = 0x9fa0

// Hooks are package-private deterministic fault points; production uses nil.
type hooks struct {
	afterCapture  func(string)
	dataOpen      func(string)
	beforeFinal   func()
	afterRead     func(string)
	afterDataRead func(string)
	procPath      string
}
type inspector struct {
	hooks hooks
	proc  int
	total int64
}

func Inspect(o Options) (map[string]any, error) { return inspect(o, hooks{}) }
func inspect(o Options, h hooks) (map[string]any, error) {
	pin := ""
	fail := func(stage string) (map[string]any, error) { e := refusal(stage); return Failure(e, pin), e }
	if !filepath.IsAbs(o.Metadata) || !filepath.IsAbs(o.Root) || !Pin(o.SHA256, 64) || o.Platform != "" && !Platform(o.Platform) {
		return fail("arguments")
	}
	x := &inspector{hooks: h, proc: -1}
	// Validate procfs before any candidate data open; hold its directory throughout.
	proc := h.procPath
	if proc == "" {
		proc = "/proc/self/fd"
	}
	fd, e := syscall.Open(proc, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if e != nil {
		return fail("procfs-unavailable")
	}
	defer syscall.Close(fd)
	var fs syscall.Statfs_t
	var pst syscall.Stat_t
	if syscall.Fstatfs(fd, &fs) != nil || fs.Type != procMagic || syscall.Fstat(fd, &pst) != nil || pst.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fail("procfs-unavailable")
	}
	x.proc = fd
	// Preserve caller ancestors, including dot segments: cleaning first could hide a symlink.
	meta, e := absoluteHeld(o.Metadata, false)
	if e != nil {
		return fail("metadata-path")
	}
	defer syscall.Close(meta)
	raw, ms, e := x.readHeld(meta, o.Metadata, metadataLimit, false)
	if e != nil {
		return fail("metadata")
	}
	if digest(raw) != o.SHA256 {
		return fail("metadata-pin")
	}
	pin = o.SHA256
	d, e := decode(raw)
	if e != nil {
		return fail("schema")
	}
	root, e := absoluteHeld(o.Root, true)
	if e != nil {
		return fail("root-path")
	}
	defer syscall.Close(root)
	var rootStat syscall.Stat_t
	if syscall.Fstat(root, &rootStat) != nil {
		return fail("root-path")
	}
	observations := []map[string]any{}
	missing := map[string]bool{}
	for _, a := range d.Artifacts {
		if filepath.Clean(o.Metadata) == filepath.Join(filepath.Clean(o.Root), filepath.FromSlash(a.Path)) {
			return fail("artifact-path")
		}
		held, e := relativeHeld(root, a.Path, false)
		if e != nil {
			if errors.Is(e, syscall.ENOENT) && !a.Required {
				missing[a.ID] = true
				observations = append(observations, map[string]any{"id": a.ID, "content_state": "optional-missing"})
				continue
			}
			return fail("artifact-path")
		}
		b, st, readErr := x.readHeld(held, a.Path, fileLimit, true)
		if readErr != nil {
			syscall.Close(held)
			return fail("artifact")
		}
		if st.Mode&07777 != uint32(a.Mode) || hex.EncodeToString(b) != a.SHA256 {
			syscall.Close(held)
			return fail("artifact-integrity")
		}
		if h.afterRead != nil {
			h.afterRead(a.Path)
		}
		current, e := relativeHeld(root, a.Path, false)
		if e != nil {
			syscall.Close(held)
			return fail("artifact-binding")
		}
		var now syscall.Stat_t
		e = syscall.Fstat(current, &now)
		syscall.Close(current)
		syscall.Close(held)
		if e != nil || !same(st, now) {
			return fail("artifact-binding")
		}
		if sameInode(st, ms) || sameInode(st, rootStat) {
			return fail("artifact-path")
		}
		observations = append(observations, map[string]any{"id": a.ID, "content_state": "verified"})
	}
	rootNow, e := absoluteHeld(o.Root, true)
	if e != nil {
		return fail("root-binding")
	}
	var rst syscall.Stat_t
	e = syscall.Fstat(rootNow, &rst)
	syscall.Close(rootNow)
	if e != nil || !sameInode(rootStat, rst) {
		return fail("root-binding")
	}
	if h.beforeFinal != nil {
		h.beforeFinal()
	}
	current, e := absoluteHeld(o.Metadata, false)
	if e != nil {
		return fail("metadata-binding")
	}
	defer syscall.Close(current)
	var now syscall.Stat_t
	if syscall.Fstat(current, &now) != nil || !same(ms, now) {
		return fail("metadata-binding")
	}
	end, es, e := x.readHeld(meta, o.Metadata, metadataLimit, false)
	if e != nil || !same(ms, es) || digest(end) != pin {
		return fail("metadata-binding")
	}
	// Bind the current metadata path again after the end read.
	last, e := absoluteHeld(o.Metadata, false)
	if e != nil {
		return fail("metadata-binding")
	}
	e = syscall.Fstat(last, &now)
	syscall.Close(last)
	if e != nil || !same(ms, now) {
		return fail("metadata-binding")
	}
	return success(d, pin, o.Platform, observations, missing), nil
}
func sameInode(a, b syscall.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode&syscall.S_IFMT == b.Mode&syscall.S_IFMT
}
func same(a, b syscall.Stat_t) bool {
	return sameInode(a, b) && a.Mode == b.Mode && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}
func absoluteHeld(p string, dir bool) (int, error) {
	if !filepath.IsAbs(p) {
		return -1, refusal("path")
	}
	root, e := syscall.Open("/", oPath|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return -1, e
	}
	defer syscall.Close(root)
	parts := []string{}
	for _, part := range strings.Split(p, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		if !dir {
			return -1, refusal("path")
		}
		return dupHeld(root)
	}
	return walkHeld(root, parts, dir)
}
func dupHeld(fd int) (int, error) {
	n, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_DUPFD_CLOEXEC, 0)
	if e != 0 {
		return -1, e
	}
	return int(n), nil
}
func relativeHeld(root int, p string, dir bool) (int, error) {
	if !validPath(p) {
		return -1, refusal("path")
	}
	return walkHeld(root, strings.Split(p, "/"), dir)
}
func walkHeld(root int, parts []string, dir bool) (int, error) {
	parent, e := dupHeld(root)
	if e != nil {
		return -1, e
	}
	for i, part := range parts {
		fd, e := syscall.Openat(parent, part, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(parent)
		if e != nil {
			return -1, e
		}
		var st syscall.Stat_t
		if e = syscall.Fstat(fd, &st); e != nil {
			syscall.Close(fd)
			return -1, e
		}
		want := uint32(syscall.S_IFDIR)
		if i == len(parts)-1 && !dir {
			want = syscall.S_IFREG
		}
		if st.Mode&syscall.S_IFMT != want {
			syscall.Close(fd)
			return -1, refusal("file-type")
		}
		parent = fd
	}
	return parent, nil
}
func (x *inspector) readHeld(held int, label string, limit int64, aggregate bool) ([]byte, syscall.Stat_t, error) {
	var before, opened, after syscall.Stat_t
	if syscall.Fstat(held, &before) != nil || before.Mode&syscall.S_IFMT != syscall.S_IFREG || before.Size < 0 || before.Size > limit {
		return nil, before, refusal("file-type-or-size")
	}
	if x.hooks.afterCapture != nil {
		x.hooks.afterCapture(label)
	}
	if x.hooks.dataOpen != nil {
		x.hooks.dataOpen(label)
	}
	// This is the only data-open: the validated procfs directory and our held fd.
	fd, e := syscall.Openat(x.proc, strconv.Itoa(held), syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, before, refusal("descriptor-open")
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("held-%d", held))
	defer f.Close()
	if syscall.Fstat(fd, &opened) != nil || !same(before, opened) {
		return nil, before, refusal("descriptor-binding")
	}
	bound := limit
	if aggregate && aggregateLimit-x.total < bound {
		bound = aggregateLimit - x.total
	}
	if bound < 0 {
		return nil, before, refusal("aggregate-limit")
	}
	var b []byte
	var n int64
	if aggregate {
		hash := sha256.New()
		n, e = io.Copy(hash, io.LimitReader(f, bound))
		b = hash.Sum(nil)
		x.total += n
	} else {
		b, e = io.ReadAll(io.LimitReader(f, bound))
		n = int64(len(b))
	}
	if x.hooks.afterDataRead != nil {
		x.hooks.afterDataRead(label)
	}
	if e != nil || n > bound || n != before.Size || syscall.Fstat(fd, &after) != nil || !same(before, after) {
		return nil, before, refusal("read-integrity")
	}
	return b, after, nil
}
