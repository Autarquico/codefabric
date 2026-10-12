package procident

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

const zombie = 5 // SZOMB in <sys/proc.h>

func startOf(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A pid with no process answers with no data.
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return "", ErrGone
		}
		return "", err
	}
	if info.Proc.P_stat == zombie {
		return "", ErrGone
	}
	t := info.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", t.Sec, t.Usec), nil
}

func bootOf() (string, error) {
	t, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%06d", t.Sec, t.Usec), nil
}

func groupMembers(pgid int) ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, p := range procs {
		if int(p.Eproc.Pgid) == pgid && p.Proc.P_stat != zombie {
			out = append(out, int(p.Proc.P_pid))
		}
	}
	return out, nil
}
