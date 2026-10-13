package cli

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal says whether r is a terminal: a file whose descriptor answers the
// window-size request. A pipe, a file and /dev/null do not.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ) //nolint:gosec // a descriptor
	return err == nil
}
