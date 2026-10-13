package procgroup

import "golang.org/x/sys/unix"

const zombie = 5 // SZOMB in <sys/proc.h>

func members(pgid int) ([]int, error) {
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

func running(pid int) (bool, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false, err
	}
	return info.Proc.P_pid == int32(pid) && info.Proc.P_stat != zombie, nil //nolint:gosec // a pid fits in an int32
}
