package blob

import (
	"fmt"
	"os"
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

// syncFile flushes the contents of path to stable storage.
func syncFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // path is built by this package
	if err != nil {
		return fmt.Errorf("blob: opening %s for sync: %w", path, err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("blob: syncing %s: %w", path, syncErr)
	}
	return closeErr
}
