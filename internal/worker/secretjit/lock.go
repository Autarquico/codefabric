package secretjit

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// lock takes the exclusive lock of runDir, which every change to its secrets
// holds, and returns its release. The lock is flock(2) on the directory
// itself: it serialises goroutines as well as processes, leaves no file
// behind, and a crash releases it.
func lock(runDir string) (unlock func(), err error) { return flock(runDir, unix.LOCK_EX) }

// rlock takes the shared lock of runDir, which readers hold so that a change
// never happens under them.
func rlock(runDir string) (unlock func(), err error) { return flock(runDir, unix.LOCK_SH) }

func flock(runDir string, how int) (unlock func(), err error) {
	fd, err := unix.Open(runDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("secretjit: opening %s: %w", runDir, err)
	}
	for {
		err = unix.Flock(fd, how)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("secretjit: locking %s: %w", runDir, err)
	}
	return func() { _ = unix.Close(fd) }, nil
}
