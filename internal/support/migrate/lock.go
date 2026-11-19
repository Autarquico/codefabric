package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockMigration takes the one lock of every migration of this user, and
// refuses at once if another process holds it. The file is in the directory
// that holds the default data directory of the user (~/.local/share): not in a
// directory a migration moves, and not where the data directory of a unit
// happens to be, so that two migrations that overlap, through the data or the
// configuration they share, can never take different locks. It is released when
// the process ends too.
func lockMigration(home string) (release func(), err error) {
	_, data := legacyDirs(home)
	parent := filepath.Dir(data)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	path := filepath.Join(parent, lockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the lock file of the migration, in the parent of the data directory
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil { //nolint:gosec // a descriptor fits an int
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another cflk is moving these services and directories right now (it holds %s): wait for it, then run this again; nothing was changed", path)
		}
		return nil, fmt.Errorf("migrate: locking %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}
