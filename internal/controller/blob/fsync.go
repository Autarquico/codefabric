package blob

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// syncDir makes the directory entries of dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is built by this package
	if err != nil {
		return fmt.Errorf("blob: opening %s for sync: %w", dir, err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return fmt.Errorf("blob: syncing %s: %w", dir, syncErr)
	}
	return closeErr
}

// mkdirAllDurable creates dir and any missing parents, syncing each new
// directory and the parent that now points at it.
func mkdirAllDurable(dir string) error {
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	parent := filepath.Dir(dir)
	if err := mkdirAllDurable(parent); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	return syncDir(parent)
}
