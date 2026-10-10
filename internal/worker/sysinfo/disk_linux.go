package sysinfo

import (
	"fmt"
	"syscall"
)

func diskBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("sysinfo: statfs %s: %w", dir, err)
	}
	// Blocks counts fragments of Frsize bytes; Bsize is only the preferred I/O size.
	return st.Blocks * uint64(st.Frsize), nil //nolint:gosec // Frsize is a positive fragment size
}
