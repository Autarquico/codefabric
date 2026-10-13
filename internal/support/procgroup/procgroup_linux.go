package procgroup

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// stat reads the state and the process group of /proc/<pid>/stat. The command
// is in parentheses and may hold spaces and parentheses, so the fields are
// counted from the last ')'.
func stat(pid int) (state byte, pgrp int, err error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, err
	}
	text := string(raw)
	end := strings.LastIndexByte(text, ')')
	fields := strings.Fields(text[end+1:])
	if end < 0 || len(fields) < 3 {
		return 0, 0, fmt.Errorf("procgroup: unexpected /proc/%d/stat", pid)
	}
	// fields[0] is field 3 (state); fields[2] is field 5 (pgrp).
	pgrp, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, fmt.Errorf("procgroup: bad pgrp in /proc/%d/stat", pid)
	}
	return fields[0][0], pgrp, nil
}

func live(state byte) bool { return state != 'Z' && state != 'X' }

func members(pgid int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		state, pgrp, err := stat(pid)
		if err != nil {
			continue // it exited meanwhile
		}
		if pgrp == pgid && live(state) {
			out = append(out, pid)
		}
	}
	return out, nil
}

func stateOf(pid int) leaderState {
	state, _, err := stat(pid)
	switch {
	case err != nil:
		return unknown
	case live(state):
		return running
	}
	return ended
}
