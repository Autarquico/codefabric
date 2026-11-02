package cli

import (
	"bufio"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal says whether r is a terminal: a file whose descriptor answers the
// window-size request. A pipe, a file and /dev/null do not.
func isTerminal(r io.Reader) bool {
	if t, ok := r.(interface{ IsTerminal() bool }); ok {
		return t.IsTerminal()
	}
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ) //nolint:gosec // a descriptor
	return err == nil
}

// bufferedInput is the buffered reader of in: the one it already shares, if it
// shares one, so that a command run after another loses none of the input.
func bufferedInput(in io.Reader) *bufio.Reader {
	if shared, ok := in.(interface{ Buffer() *bufio.Reader }); ok {
		return shared.Buffer()
	}
	return bufio.NewReader(in)
}
