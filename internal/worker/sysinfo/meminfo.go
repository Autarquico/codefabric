package sysinfo

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// memTotalBytes reads MemTotal from the text of /proc/meminfo.
func memTotalBytes(meminfo io.Reader) (uint64, error) {
	scanner := bufio.NewScanner(meminfo)
	for scanner.Scan() {
		rest, found := strings.CutPrefix(scanner.Text(), "MemTotal:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("sysinfo: unexpected MemTotal line %q", scanner.Text())
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("sysinfo: MemTotal: %w", err)
		}
		return kb << 10, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("sysinfo: reading meminfo: %w", err)
	}
	return 0, fmt.Errorf("sysinfo: no MemTotal in meminfo")
}
