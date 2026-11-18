package daemon

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
	// ioctlRestoreTermios sets them after the output drained, and discards the input not read.
	ioctlRestoreTermios = unix.TIOCSETAF
)
