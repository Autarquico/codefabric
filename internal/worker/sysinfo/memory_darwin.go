package sysinfo

import (
	"encoding/binary"
	"fmt"
	"syscall"
)

func memoryBytes() (uint64, error) {
	raw, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("sysinfo: hw.memsize: %w", err)
	}
	// Sysctl drops the trailing NUL of what it returns, which for a
	// little-endian uint64 is its top byte: put it back.
	var buf [8]byte
	copy(buf[:], raw)
	return binary.LittleEndian.Uint64(buf[:]), nil
}
