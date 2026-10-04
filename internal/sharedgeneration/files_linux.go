//go:build linux

package sharedgeneration

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

const oPath = 0x200000
const procMagic = 0x9fa0

// Test hooks never confer approval or initialization ownership.
type hooks struct {
	fault        func(string) error
	event        func(string)
	prepared     func()
	afterCapture func(string)
	afterRead    func(string)
	dataOpen     func(string)
	procPath     string
}
type filesystem struct {
	proc int
	h    hooks
}

func newFilesystem(h hooks) (*filesystem, error) {
	p := h.procPath
	if p == "" {
		p = "/proc/self/fd"
	}
	fd, e := syscall.Open(p, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, errors.New("procfs unavailable")
	}
	var st syscall.Statfs_t
	if syscall.Fstatfs(fd, &st) != nil || st.Type != procMagic {
		syscall.Close(fd)
		return nil, errors.New("procfs unavailable")
	}
	return &filesystem{fd, h}, nil
}
func (x *filesystem) close() { syscall.Close(x.proc) }
func (x *filesystem) point(label string) error {
	if x.h.event != nil {
		x.h.event(label)
	}
	if x.h.fault != nil {
		return x.h.fault(label)
	}
	return nil
}
func sameInode(a, b syscall.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode&syscall.S_IFMT == b.Mode&syscall.S_IFMT
}
func same(a, b syscall.Stat_t) bool {
	return sameInode(a, b) && a.Mode == b.Mode && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim && a.Uid == b.Uid && a.Nlink == b.Nlink
}
func stat(fd int) (syscall.Stat_t, error) {
	var s syscall.Stat_t
	e := syscall.Fstat(fd, &s)
	return s, e
}
func dup(fd int) (int, error) {
	n, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_DUPFD_CLOEXEC, 0)
	if e != 0 {
		return -1, e
	}
	return int(n), nil
}
func absolute(p string, dir bool) (int, error) {
	if !filepath.IsAbs(p) || p == "/" || strings.HasSuffix(p, "/") || !validPath(strings.TrimPrefix(p, "/")) {
		return -1, errors.New("absolute physical path required")
	}
	root, e := syscall.Open("/", oPath|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return -1, e
	}
	defer syscall.Close(root)
	return relative(root, strings.TrimPrefix(p, "/"), dir)
}
func relative(root int, p string, dir bool) (int, error) {
	if !validPath(p) {
		return -1, errors.New("invalid relative path")
	}
	parent, e := dup(root)
	if e != nil {
		return -1, e
	}
	parts := strings.Split(p, "/")
	for i, s := range parts {
		next, e := syscall.Openat(parent, s, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(parent)
		if e != nil {
			return -1, e
		}
		st, e := stat(next)
		want := uint32(syscall.S_IFDIR)
		if i == len(parts)-1 && !dir {
			want = syscall.S_IFREG
		}
		if e != nil || st.Mode&syscall.S_IFMT != want {
			syscall.Close(next)
			return -1, errors.New("physical path type")
		}
		parent = next
	}
	return parent, nil
}
func owned(st syscall.Stat_t, mode uint32, dir bool) error {
	want := uint32(syscall.S_IFREG)
	if dir {
		want = syscall.S_IFDIR
	}
	if st.Mode&syscall.S_IFMT != want || st.Mode&07777 != mode || st.Uid != uint32(os.Getuid()) || !dir && st.Nlink != 1 {
		return errors.New("ownership, mode or link count")
	}
	return nil
}
func (x *filesystem) bridge(held int, flags int) (*os.File, error) {
	before, e := stat(held)
	if e != nil {
		return nil, e
	}
	fd, e := syscall.Openat(x.proc, strconv.Itoa(held), flags|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), "held")
	now, e := stat(fd)
	if e != nil || !same(before, now) {
		f.Close()
		return nil, errors.New("descriptor binding")
	}
	return f, nil
}
func (x *filesystem) read(root int, p string, limit int64, mode uint32) ([]byte, syscall.Stat_t, error) {
	held, e := relative(root, p, false)
	if e != nil {
		return nil, syscall.Stat_t{}, e
	}
	defer syscall.Close(held)
	before, e := stat(held)
	if e != nil {
		return nil, before, e
	}
	if e = owned(before, mode, false); e != nil {
		return nil, before, e
	}
	if before.Size < 0 || before.Size > limit {
		return nil, before, errors.New("file size")
	}
	if x.h.afterCapture != nil {
		x.h.afterCapture(p)
	}
	if x.h.dataOpen != nil {
		x.h.dataOpen(p)
	}
	f, e := x.bridge(held, syscall.O_RDONLY)
	if e != nil {
		return nil, before, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if x.h.afterRead != nil {
		x.h.afterRead(p)
	}
	after, se := stat(int(f.Fd()))
	if e != nil || se != nil || !same(before, after) || int64(len(b)) != before.Size || int64(len(b)) > limit {
		return nil, before, errors.New("read integrity")
	}
	end, e := relative(root, p, false)
	if e != nil {
		return nil, before, e
	}
	defer syscall.Close(end)
	bound, e := stat(end)
	if e != nil || !same(before, bound) {
		return nil, before, errors.New("final path binding")
	}
	return b, before, nil
}
func (x *filesystem) names(fd int) ([]string, error) {
	f, e := x.bridge(fd, syscall.O_RDONLY|syscall.O_DIRECTORY)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	entries, e := f.ReadDir(1025)
	if e != nil && e != io.EOF {
		return nil, e
	}
	if len(entries) > 1024 {
		return nil, errors.New("directory limit")
	}
	names := make([]string, 0, len(entries))
	for _, en := range entries {
		names = append(names, en.Name())
	}
	// ReadDir(n) may return a full page without EOF. Ensure no hidden tail.
	if _, e = f.ReadDir(1); e != io.EOF {
		return nil, errors.New("directory limit")
	}
	return names, nil
}
func (x *filesystem) sync(fd int, label string) error {
	if e := x.point("fsync:" + label); e != nil {
		return e
	}
	f, e := x.bridge(fd, syscall.O_RDONLY)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = f.Sync(); e != nil {
		return e
	}
	return x.point("after-fsync:" + label)
}
func (x *filesystem) mkdir(root int, p string) error {
	if e := x.point("mkdir:" + p); e != nil {
		return e
	}
	parent, e := parentHeld(root, p)
	if e != nil {
		return e
	}
	defer syscall.Close(parent)
	if e := syscall.Mkdirat(parent, filepath.Base(p), 0700); e != nil {
		return e
	}
	return x.point("after-mkdir:" + p)
}
func (x *filesystem) write(root int, p string, b []byte, mode uint32) error {
	if e := x.point("write:" + p); e != nil {
		return e
	}
	parent, e := parentHeld(root, p)
	if e != nil {
		return e
	}
	defer syscall.Close(parent)
	fd, e := syscall.Openat(parent, filepath.Base(p), syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
	if e != nil {
		return e
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	if e = syscall.Fchmod(fd, mode); e != nil {
		return e
	}
	if n, e := f.Write(b); e != nil {
		return e
	} else if n != len(b) {
		return io.ErrShortWrite
	}
	if e = x.point("fsync-file:" + p); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	return x.point("after-fsync-file:" + p)
}
func (x *filesystem) rename(root int, from, to string) error {
	if e := x.point("rename:" + from + "->" + to); e != nil {
		return e
	}
	src, e := parentHeld(root, from)
	if e != nil {
		return e
	}
	defer syscall.Close(src)
	dst, e := parentHeld(root, to)
	if e != nil {
		return e
	}
	defer syscall.Close(dst)
	if e := syscall.Renameat(src, filepath.Base(from), dst, filepath.Base(to)); e != nil {
		return e
	}
	return x.point("after-rename:" + from + "->" + to)
}

// Mutations use held physical parent descriptors too; O_NOFOLLOW on a final
// component alone would still follow an intermediate symlink.
func parentHeld(root int, p string) (int, error) {
	if !validPath(p) {
		return -1, errors.New("mutation path")
	}
	d := filepath.Dir(p)
	var fd int
	var e error
	if d == "." {
		fd, e = dup(root)
	} else {
		fd, e = relative(root, d, true)
	}
	if e != nil {
		return -1, e
	}
	st, e := stat(fd)
	if e == nil {
		e = owned(st, 0700, true)
	}
	if e != nil {
		syscall.Close(fd)
		return -1, e
	}
	return fd, nil
}
func randomName() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return hex.EncodeToString(b[:]), nil
}
func symlinkat(target string, root int, name string) error {
	a, e := syscall.BytePtrFromString(target)
	if e != nil {
		return e
	}
	b, e := syscall.BytePtrFromString(name)
	if e != nil {
		return e
	}
	_, _, errno := syscall.Syscall(syscall.SYS_SYMLINKAT, uintptr(unsafe.Pointer(a)), uintptr(root), uintptr(unsafe.Pointer(b)))
	if errno != 0 {
		return errno
	}
	return nil
}
func readlinkat(root int, name string) (string, error) {
	p, e := syscall.BytePtrFromString(name)
	if e != nil {
		return "", e
	}
	var buf [513]byte
	n, _, errno := syscall.Syscall6(syscall.SYS_READLINKAT, uintptr(root), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	if errno != 0 {
		return "", errno
	}
	if n > 512 {
		return "", errors.New("selector limit")
	}
	return string(buf[:n]), nil
}
func (x *filesystem) tree(root int, expected map[string]uint32) (map[string][]byte, error) {
	return x.treeMode(root, expected, false)
}
func (x *filesystem) treeMode(root int, expected map[string]uint32, source bool) (map[string][]byte, error) {
	data := map[string][]byte{}
	observed := map[string]syscall.Stat_t{}
	seenDirs := map[string]bool{}
	total := int64(0)
	allowedDirs := map[string]bool{"": true}
	for p := range expected {
		if !validPath(p) {
			return nil, errors.New("tree path")
		}
		for d := filepath.Dir(p); d != "."; d = filepath.Dir(d) {
			allowedDirs[d] = true
		}
	}
	var walk func(int, string) error
	walk = func(fd int, prefix string) error {
		st, e := stat(fd)
		if e != nil {
			return e
		}
		if source {
			if st.Mode&syscall.S_IFMT != syscall.S_IFDIR || st.Mode&07022 != 0 || st.Uid != uint32(os.Getuid()) {
				return errors.New("source directory ownership or mode")
			}
		} else if e = owned(st, 0700, true); e != nil {
			return e
		}
		names, e := x.names(fd)
		if e != nil {
			return e
		}
		for _, n := range names {
			p := n
			if prefix != "" {
				p = prefix + "/" + n
			}
			if !validPath(p) {
				return errors.New("tree path")
			}
			h, e := syscall.Openat(fd, n, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if e != nil {
				return e
			}
			s, e := stat(h)
			if e != nil {
				syscall.Close(h)
				return e
			}
			if s.Mode&syscall.S_IFMT == syscall.S_IFDIR {
				if !allowedDirs[p] {
					syscall.Close(h)
					return errors.New("extra directory")
				}
				seenDirs[p] = true
				e = walk(h, p)
				syscall.Close(h)
				if e != nil {
					return e
				}
			} else {
				syscall.Close(h)
				mode, ok := expected[p]
				if !ok {
					return errors.New("extra file")
				}
				limit := int64(fileLimit)
				if mode == 0600 {
					limit = metadataLimit
				}
				b, fs, e := x.read(root, p, limit, mode)
				if e != nil {
					return e
				}
				if mode != 0600 {
					total += int64(len(b))
					if total > aggregateLimit {
						return errors.New("aggregate limit")
					}
				}
				if mode == 0644 && (!utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0) {
					return errors.New("payload is not generated text")
				}
				data[p] = b
				observed[p] = fs
			}
		}
		// Bind this directory after traversal against the original root-relative path.
		nowfd := root
		if prefix != "" {
			nowfd, e = relative(root, prefix, true)
			if e != nil {
				return e
			}
			defer syscall.Close(nowfd)
		}
		now, e := stat(nowfd)
		if e != nil || !same(st, now) {
			return errors.New("directory binding")
		}
		return nil
	}
	if e := walk(root, ""); e != nil {
		return nil, e
	}
	if len(data) != len(expected) || len(seenDirs)+1 != len(allowedDirs) {
		return nil, errors.New("tree closure")
	}
	for _, p := range sortedKeys(expected) {
		limit := int64(fileLimit)
		if expected[p] == 0600 {
			limit = metadataLimit
		}
		b, now, e := x.read(root, p, limit, expected[p])
		if e != nil {
			return nil, e
		}
		if !same(observed[p], now) || string(b) != string(data[p]) {
			return nil, errors.New("tree end binding")
		}
	}
	return data, nil
}
func (x *filesystem) syncTree(root int, prefix string) error {
	names, e := x.names(root)
	if e != nil {
		return e
	}
	for _, n := range names {
		h, e := syscall.Openat(root, n, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if e != nil {
			return e
		}
		st, e := stat(h)
		if e == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			e = x.syncTree(h, prefix+"/"+n)
		}
		syscall.Close(h)
		if e != nil {
			return e
		}
	}
	return x.sync(root, prefix)
}
func physicalBind(path string, held int) error {
	current, e := absolute(path, true)
	if e != nil {
		return e
	}
	defer syscall.Close(current)
	a, e := stat(held)
	if e != nil {
		return e
	}
	b, e := stat(current)
	if e != nil || !sameInode(a, b) || a.Mode != b.Mode || a.Uid != b.Uid {
		return fmt.Errorf("root binding")
	}
	return nil
}
