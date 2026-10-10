package sysinfo

import (
	"fmt"
	"os"
)

func memoryBytes() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("sysinfo: %w", err)
	}
	defer func() { _ = f.Close() }()
	return memTotalBytes(f)
}
