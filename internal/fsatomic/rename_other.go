//go:build !(linux && (amd64 || arm64))

package fsatomic

// renameNoReplace has no atomic form here; RenameNoReplace checks the
// destination and renames.
func renameNoReplace(string, string) error { return errNoReplaceUnsupported }
