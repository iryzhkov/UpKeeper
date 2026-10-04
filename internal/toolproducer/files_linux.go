//go:build linux

package toolproducer

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const oPath = 0x200000

type physical struct {
	proc  int
	held  []observation
	hooks readHooks
}
type observation struct {
	path  string
	fd    int
	st    syscall.Stat_t
	raw   []byte
	limit int64
	dir   bool
}
type readHooks struct {
	afterCapture func(string)
	dataOpen     func(string)
	afterRead    func(string)
	beforeFinal  func()
}

func newPhysical() (*physical, error) {
	fd, e := syscall.Open("/proc/self/fd", oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, refuse("procfs")
	}
	var fs syscall.Statfs_t
	if syscall.Fstatfs(fd, &fs) != nil || fs.Type != 0x9fa0 {
		syscall.Close(fd)
		return nil, refuse("procfs")
	}
	return &physical{proc: fd}, nil
}
func (p *physical) close() {
	for _, o := range p.held {
		syscall.Close(o.fd)
	}
	syscall.Close(p.proc)
}
func same(a, b syscall.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Nlink == b.Nlink && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}
func inode(a, b syscall.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode }
func absolute(p string, dir bool) (int, error) {
	if !filepath.IsAbs(p) || !safe(p) || filepath.Clean(p) != p {
		return -1, refuse("physical-path")
	}
	fd, e := syscall.Open("/", oPath|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return -1, e
	}
	if p == "/" {
		if dir {
			return fd, nil
		}
		syscall.Close(fd)
		return -1, refuse("physical-type")
	}
	parts := strings.Split(p[1:], "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 255 {
			syscall.Close(fd)
			return -1, refuse("physical-path")
		}
		next, e := syscall.Openat(fd, part, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if e != nil {
			return -1, e
		}
		var st syscall.Stat_t
		want := uint32(syscall.S_IFDIR)
		if i == len(parts)-1 && !dir {
			want = syscall.S_IFREG
		}
		if syscall.Fstat(next, &st) != nil || st.Mode&syscall.S_IFMT != want {
			syscall.Close(next)
			return -1, refuse("physical-type")
		}
		fd = next
	}
	return fd, nil
}
func (p *physical) root(path string) error {
	fd, e := absolute(path, true)
	if e != nil {
		return e
	}
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil {
		syscall.Close(fd)
		return refuse("root-stat")
	}
	p.held = append(p.held, observation{path: path, fd: fd, st: st, dir: true})
	return nil
}
func (p *physical) readFD(fd int, label string, limit int64) ([]byte, syscall.Stat_t, error) {
	var before, opened, after syscall.Stat_t
	if syscall.Fstat(fd, &before) != nil || before.Mode&syscall.S_IFMT != syscall.S_IFREG || before.Nlink != 1 || before.Mode&07777 != 0644 && before.Mode&07777 != 0755 || before.Size < 1 || before.Size > limit {
		return nil, before, refuse("physical-type-mode-size")
	}
	if p.hooks.afterCapture != nil {
		p.hooks.afterCapture(label)
	}
	if p.hooks.dataOpen != nil {
		p.hooks.dataOpen(label)
	}
	n, e := syscall.Openat(p.proc, strconv.Itoa(fd), syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, before, e
	}
	f := os.NewFile(uintptr(n), fmt.Sprintf("producer-held-%d", fd))
	defer f.Close()
	if syscall.Fstat(n, &opened) != nil || !same(before, opened) {
		return nil, before, refuse("physical-open-binding")
	}
	raw, e := io.ReadAll(io.LimitReader(f, limit+1))
	if p.hooks.afterRead != nil {
		p.hooks.afterRead(label)
	}
	if e != nil || int64(len(raw)) != before.Size || int64(len(raw)) > limit || syscall.Fstat(n, &after) != nil || !same(before, after) {
		return nil, before, refuse("physical-read-binding")
	}
	return raw, after, nil
}
func (p *physical) file(path string, limit int64) ([]byte, int, error) {
	fd, e := absolute(path, false)
	if e != nil {
		return nil, 0, e
	}
	raw, st, e := p.readFD(fd, path, limit)
	if e != nil {
		syscall.Close(fd)
		return nil, 0, e
	}
	p.held = append(p.held, observation{path: path, fd: fd, st: st, raw: raw, limit: limit})
	return raw, int(st.Mode & 07777), nil
}
func (p *physical) check() error {
	if p.hooks.beforeFinal != nil {
		p.hooks.beforeFinal()
	}
	for _, o := range p.held {
		fd, e := absolute(o.path, o.dir)
		if e != nil {
			return refuse("physical-final-path")
		}
		var st syscall.Stat_t
		e = syscall.Fstat(fd, &st)
		syscall.Close(fd)
		if e != nil || o.dir && !inode(o.st, st) || !o.dir && !same(o.st, st) {
			return refuse("physical-final-binding")
		}
		if !o.dir {
			raw, now, e := p.readFD(o.fd, o.path, o.limit)
			if e != nil || !same(o.st, now) || !bytes.Equal(raw, o.raw) {
				return refuse("physical-final-content")
			}
		}
		fd, e = absolute(o.path, o.dir)
		if e != nil {
			return refuse("physical-final-path")
		}
		e = syscall.Fstat(fd, &st)
		syscall.Close(fd)
		if e != nil || o.dir && !inode(o.st, st) || !o.dir && !same(o.st, st) {
			return refuse("physical-final-binding")
		}
	}
	return nil
}
