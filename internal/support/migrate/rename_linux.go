package migrate

import "golang.org/x/sys/unix"

// renameNoReplace moves oldPath to newPath and fails, instead of replacing it,
// if newPath exists.
func renameNoReplace(oldPath, newPath string) error {
	return unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE)
}
