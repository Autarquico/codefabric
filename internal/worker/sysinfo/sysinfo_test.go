package sysinfo

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCollectMeasuresThisMachine(t *testing.T) {
	info, err := Collect(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if info.CPU != runtime.NumCPU() || info.OS != runtime.GOOS || info.Arch != runtime.GOARCH || info.Hostname == "" {
		t.Errorf("identity of the machine: %+v", info)
	}
	if info.MemoryMB < 64 || info.DiskMB < 64 {
		t.Errorf("implausible sizes: %+v", info)
	}
}

func TestCollectMeasuresTheDiskOfADirectoryThatDoesNotExistYet(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not", "created", "yet")
	existing, err := Collect(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := Collect(missing)
	if err != nil {
		t.Fatal(err)
	}
	if info.DiskMB != existing.DiskMB {
		t.Errorf("disk %d MB, want that of its ancestor, %d MB", info.DiskMB, existing.DiskMB)
	}
}

func TestMemTotalBytes(t *testing.T) {
	const meminfo = "MemFree:         1000 kB\nMemTotal:       16384000 kB\nMemAvailable:    2000 kB\n"
	got, err := memTotalBytes(strings.NewReader(meminfo))
	if err != nil || got != 16384000<<10 {
		t.Errorf("MemTotal = %d, %v", got, err)
	}
	for _, bad := range []string{"", "MemFree: 1 kB\n", "MemTotal: lots kB\n", "MemTotal: 5 MB\n"} {
		if _, err := memTotalBytes(strings.NewReader(bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
