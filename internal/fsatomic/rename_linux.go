//go:build linux && (amd64 || arm64)

package fsatomic

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// renameNoReplaceFlag is RENAME_NOREPLACE from linux/fs.h.
const renameNoReplaceFlag = 1

// atFDCWD is AT_FDCWD: relative paths resolve against the working directory.
const atFDCWD = -100

// renameNoReplace is renameat2(2) with RENAME_NOREPLACE, which the kernel
// performs atomically. A kernel or filesystem without it answers ENOSYS or
// EINVAL, and the caller falls back.
func renameNoReplace(oldpath, newpath string) error {
	from, err := syscall.BytePtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := syscall.BytePtrFromString(newpath)
	if err != nil {
		return err
	}
	fd := atFDCWD
	_, _, errno := syscall.Syscall6(sysRenameat2,
		uintptr(fd), uintptr(unsafe.Pointer(from)),
		uintptr(fd), uintptr(unsafe.Pointer(to)),
		renameNoReplaceFlag, 0)
	switch {
	case errno == 0:
		return nil
	case errors.Is(errno, syscall.ENOSYS) || errors.Is(errno, syscall.EINVAL):
		return errNoReplaceUnsupported
	default:
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errno}
	}
}
