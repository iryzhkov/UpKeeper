package fsatomic

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// RenameNoReplace moves oldpath to newpath only if nothing exists at newpath,
// and reports an error satisfying errors.Is(err, fs.ErrExist) otherwise. It
// works for directories, which is what os.Rename cannot do safely: Go refuses
// any existing directory at the destination, while rename(2) replaces an
// empty one. A claim on a path is therefore the rename itself, never a
// directory created first.
//
// Where the kernel offers an atomic no-replace rename it is used. Elsewhere
// the destination is checked immediately before an ordinary rename, which
// leaves only the narrow window between the two calls.
func RenameNoReplace(oldpath, newpath string) error {
	err := renameNoReplace(oldpath, newpath)
	if !errors.Is(err, errNoReplaceUnsupported) {
		return err
	}
	if _, statErr := os.Lstat(newpath); statErr == nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EEXIST}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}
	return os.Rename(oldpath, newpath)
}

// errNoReplaceUnsupported means the platform or the filesystem has no atomic
// no-replace rename and the checked fallback has to be used.
var errNoReplaceUnsupported = errors.New("no atomic no-replace rename")
