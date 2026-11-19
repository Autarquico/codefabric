package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockMigration takes the lock of every migration of services and directories
// that share the old data directory dataDir, and refuses at once if another
// process holds it. The file lives in the directory that holds that one, which
// no migration moves (the lock files in the directories themselves move with
// them), and is of the user: a Worker and a Controller of one root, and any
// spelling of its path, find the same one. It is released when the process
// ends too.
func lockMigration(dataDir string) (release func(), err error) {
	resolvedDir := dataDir
	if resolved, err := filepath.EvalSymlinks(dataDir); err == nil {
		resolvedDir = resolved
	}
	parent := filepath.Dir(filepath.Clean(resolvedDir))
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
