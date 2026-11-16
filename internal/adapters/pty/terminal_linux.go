package pty

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// terminalProcs lists the processes whose controlling terminal is the pty, from
// /proc: it needs no ps, which a slim container often lacks. The terminal of a
// process is the tty_nr of its stat, the device number of the slave in the
// encoding of the kernel.
func (s *Session) terminalProcs() ([]proc, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(s.tty, &st); err != nil {
		return nil, err
	}
	want := ttyNr(st.Rdev)
	dirs, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return nil, err
	}
	var procs []proc
	for _, path := range dirs {
		b, err := os.ReadFile(path) //nolint:gosec // a stat file of /proc
		if err != nil {
			continue // gone since the listing
		}
		// pid (comm) state ppid pgrp session tty_nr ...; comm may hold spaces and parentheses.
		text := string(b)
		end := strings.LastIndexByte(text, ')')
		if end < 0 {
			continue
		}
		rest := strings.Fields(text[end+1:])
		pid, err1 := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/proc/"), "/stat"))
		if len(rest) < 5 || err1 != nil {
			continue
		}
		pgid, err2 := strconv.Atoi(rest[2])
		tty, err3 := strconv.ParseUint(rest[4], 10, 64)
		if err2 == nil && err3 == nil && tty == want && pid > 1 && pgid > 1 {
			procs = append(procs, proc{pid, pgid})
		}
	}
	return procs, nil
}

// ttyNr is the device number as /proc/<pid>/stat prints it (the kernel's
// new_encode_dev), from the rdev stat gives (the glibc encoding).
func ttyNr(rdev uint64) uint64 {
	major := (rdev>>8)&0xfff | (rdev>>32)&^0xfff
	minor := rdev&0xff | (rdev>>12)&^0xff
	return minor&0xff | major<<8 | (minor&^0xff)<<12
}
