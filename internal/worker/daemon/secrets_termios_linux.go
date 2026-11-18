package daemon

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
	// ioctlRestoreTermios sets them after the output drained, and discards the input not read.
	ioctlRestoreTermios = unix.TCSETSF
)
