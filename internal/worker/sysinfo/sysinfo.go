// Package sysinfo measures the machine a Worker runs on with the standard
// library alone: runtime for the CPUs, /proc on Linux and sysctl on macOS for
// the memory, and statfs for the disk.
package sysinfo

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
)

const bytesPerMB = 1 << 20

// Info is what the machine has.
type Info struct {
	Hostname string
	OS       string
	Arch     string
	CPU      int
	MemoryMB int
	DiskMB   int // size of the disk that holds the directory given to Collect
}

// Collect measures the machine. dir is where the Worker keeps its files: its
// disk is the one reported, and it need not exist yet.
func Collect(dir string) (Info, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return Info{}, fmt.Errorf("sysinfo: hostname: %w", err)
	}
	memory, err := memoryBytes()
	if err != nil {
		return Info{}, err
	}
	disk, err := diskBytes(existingAncestor(dir))
	if err != nil {
		return Info{}, err
	}
	return Info{
		Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, CPU: runtime.NumCPU(),
		MemoryMB: megabytes(memory), DiskMB: megabytes(disk),
	}, nil
}

// existingAncestor is dir or, failing that, the closest directory above it that exists.
func existingAncestor(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// megabytes rounds down, and saturates rather than wrap around on a machine
// with more than 2^31 MB.
func megabytes(bytes uint64) int {
	return int(min(bytes/bytesPerMB, math.MaxInt32)) //nolint:gosec // bounded by MaxInt32 on the line above
}
