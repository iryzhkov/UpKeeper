//go:build linux

package author

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const sharedOPath = 0x200000

// Acquire every component before data opening. A root-owned or caller-owned
// sticky ancestor (such as /tmp) is accepted; other writable parents refuse.
func sharedHeld(path string, directory bool) (int, error) {
	if !sharedPhysicalPath(path) {
		return -1, errors.New("shared physical path required")
	}
	parent, err := syscall.Open("/", sharedOPath|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		next, err := syscall.Openat(parent, part, sharedOPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(parent)
		if err != nil {
			return -1, err
		}
		var st syscall.Stat_t
		err = syscall.Fstat(next, &st)
		dir := i < len(parts)-1 || directory
		want := uint32(syscall.S_IFREG)
		if dir {
			want = syscall.S_IFDIR
		}
		if err != nil || st.Mode&syscall.S_IFMT != want ||
			dir && (st.Uid != 0 && st.Uid != uint32(os.Getuid()) || st.Mode&0022 != 0 && st.Mode&syscall.S_ISVTX == 0) {
			syscall.Close(next)
			return -1, errors.New("shared physical path type or unsafe parent")
		}
		parent = next
	}
	return parent, nil
}
func acquireSharedRepository(path string) (func() error, func(), error) {
	held, err := sharedHeld(path, true)
	if err != nil {
		return nil, nil, err
	}
	var before syscall.Stat_t
	if err := syscall.Fstat(held, &before); err != nil {
		syscall.Close(held)
		return nil, nil, err
	}
	verify := func() error {
		now, err := sharedHeld(path, true)
		if err != nil {
			return err
		}
		defer syscall.Close(now)
		var st syscall.Stat_t
		if syscall.Fstat(now, &st) != nil || st.Dev != before.Dev || st.Ino != before.Ino || st.Mode != before.Mode || st.Uid != before.Uid {
			return errors.New("shared repository path replaced")
		}
		return nil
	}
	return verify, func() { syscall.Close(held) }, nil
}
func sharedSame(a, b syscall.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid && a.Nlink == b.Nlink && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}

func acquireSharedManifest(path string) ([]byte, func() error, func(), error) {
	held, err := sharedHeld(path, false)
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(err error) ([]byte, func() error, func(), error) { syscall.Close(held); return nil, nil, nil, err }
	var before syscall.Stat_t
	if syscall.Fstat(held, &before) != nil || before.Nlink != 1 || before.Size <= 0 || before.Size > sharedManifestLimit ||
		before.Mode&07777 != before.Mode&0777 || before.Mode&0022 != 0 || before.Uid != uint32(os.Getuid()) && before.Uid != 0 {
		return fail(errors.New("shared manifest ownership, links, mode or size"))
	}
	proc, err := syscall.Open("/proc/self/fd", sharedOPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	closeBoth := func() { syscall.Close(proc); syscall.Close(held) }
	var fs syscall.Statfs_t
	var pst syscall.Stat_t
	if syscall.Fstatfs(proc, &fs) != nil || fs.Type != 0x9fa0 || syscall.Fstat(proc, &pst) != nil || pst.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		closeBoth()
		return nil, nil, nil, errors.New("shared procfs unavailable")
	}
	read := func() ([]byte, error) {
		fd, err := syscall.Openat(proc, strconv.Itoa(held), syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), "held manifest")
		defer file.Close()
		var opened syscall.Stat_t
		if syscall.Fstat(fd, &opened) != nil || !sharedSame(before, opened) {
			return nil, errors.New("shared manifest descriptor binding")
		}
		raw, err := io.ReadAll(io.LimitReader(file, sharedManifestLimit+1))
		var after syscall.Stat_t
		if err != nil || syscall.Fstat(fd, &after) != nil || !sharedSame(before, after) || int64(len(raw)) != before.Size || len(raw) > sharedManifestLimit {
			return nil, errors.New("shared manifest changed during read")
		}
		return raw, nil
	}
	raw, err := read()
	if err != nil {
		closeBoth()
		return nil, nil, nil, err
	}
	bind := func() error {
		current, err := sharedHeld(path, false)
		if err != nil {
			return err
		}
		defer syscall.Close(current)
		var now syscall.Stat_t
		if syscall.Fstat(current, &now) != nil || !sharedSame(before, now) {
			return errors.New("shared manifest path replaced")
		}
		return nil
	}
	verify := func() error {
		if err := bind(); err != nil {
			return err
		}
		now, err := read()
		if err != nil || !bytes.Equal(raw, now) {
			return errors.New("shared manifest bytes replaced")
		}
		return bind()
	}
	if err := verify(); err != nil {
		closeBoth()
		return nil, nil, nil, err
	}
	return raw, verify, closeBoth, nil
}
