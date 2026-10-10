//go:build linux || darwin

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
	return st.Blocks * uint64(st.Bsize), nil //nolint:gosec // Bsize is a positive block size
}
