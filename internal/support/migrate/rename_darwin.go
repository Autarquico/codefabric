package migrate

import "golang.org/x/sys/unix"

// renameNoReplace moves oldPath to newPath and fails, instead of replacing it,
// if newPath exists.
func renameNoReplace(oldPath, newPath string) error {
	return unix.RenamexNp(oldPath, newPath, unix.RENAME_EXCL)
}
