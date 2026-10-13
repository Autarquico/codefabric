package procident

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// stat is the fields of /proc/<pid>/stat that matter, counted as proc(5) does.
type stat struct {
	state     byte
	startTime string
}

// readStat parses /proc/<pid>/stat. The command is in parentheses and may hold
// spaces and parentheses, so everything is counted from the last ')'.
func readStat(pid int) (stat, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) {
		return stat{}, ErrGone
	}
	if err != nil {
		return stat{}, err
	}
	text := string(raw)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return stat{}, fmt.Errorf("procident: unexpected /proc/%d/stat", pid)
	}
	fields := strings.Fields(text[end+1:])
	// fields[0] is field 3 (state); fields[19] is field 22 (starttime).
	if len(fields) < 20 {
		return stat{}, fmt.Errorf("procident: short /proc/%d/stat", pid)
	}
	return stat{state: fields[0][0], startTime: fields[19]}, nil
}

func bootID() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(raw)), err
}

func startOf(pid int) (string, error) {
	s, err := readStat(pid)
	if err != nil {
		return "", err
	}
	if s.state == 'Z' || s.state == 'X' {
		return "", ErrGone // it exited; only its entry in the process table is left
	}
	return s.startTime, nil
}

func bootOf() (string, error) { return bootID() }
