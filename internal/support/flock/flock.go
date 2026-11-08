// Package flock is an exclusive advisory lock on a file, shared by processes.
package flock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Lock takes the exclusive lock of the file at path, creating it if needed,
// and blocks until it has it. A crash of the holder releases the lock too.
func Lock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the caller names its own lock file
	if err != nil {
		return nil, fmt.Errorf("flock: opening %s: %w", path, err)
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX) //nolint:gosec // a descriptor fits an int
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock: locking %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}
