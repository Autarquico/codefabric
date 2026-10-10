package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write writes data to a temporary file in the same directory, syncs
// it and renames it over path, so readers see the old or the new content and a
// crash never leaves a half-written file.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicfile: creating temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfile: writing %s: %w", path, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfile: closing %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("atomicfile: publishing %s: %w", path, err)
	}
	return SyncDir(dir)
}

// SyncDir makes the entries of dir durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // callers pass directories they built
	if err != nil {
		return fmt.Errorf("atomicfile: opening %s for sync: %w", dir, err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return fmt.Errorf("atomicfile: syncing %s: %w", dir, syncErr)
	}
	return closeErr
}
