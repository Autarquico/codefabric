package secretjit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
)

// Errors of the package. None of them carries a value.
var (
	// ErrNotDelivered is a run directory without a delivery.
	ErrNotDelivered = errors.New("secretjit: no secrets delivered")
	// ErrNoAccess is a ref the harness may not read: it was not delivered,
	// or it was revoked.
	ErrNoAccess = errors.New("secretjit: secret not available to the harness")
	// ErrUnsafe is an entry that is not a private directory or regular file
	// of this user: a symlink, another type, group or other permissions, or
	// another owner.
	ErrUnsafe = errors.New("secretjit: unsafe secret entry")
	// ErrCorrupt is a delivery whose manifest or files do not agree.
	ErrCorrupt = errors.New("secretjit: corrupt delivery")
	// ErrStale is a delivery by an attempt older than the one in place.
	ErrStale = errors.New("secretjit: stale delivery attempt")
	// ErrPending is a release that has not finished: something is left to
	// remove, and the release has to be retried.
	ErrPending = errors.New("secretjit: release pending")
)

const (
	liveName     = "secrets"
	stagingName  = "secrets.staging"
	deletingName = "secrets.deleting"
	manifestName = "manifest.json"
	manifestTemp = "manifest.json.tmp"

	manifestSchema   = 1
	maxManifestBytes = 64 << 10

	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600
)

// manifest describes a delivery. It holds no value.
type manifest struct {
	Schema  int     `json:"schema"`
	Attempt uint64  `json:"attempt"`
	Refs    []entry `json:"refs"`
}

type entry struct {
	Ref     string `json:"ref"`
	File    string `json:"file"`
	Harness bool   `json:"harness"`
}

// fileName is the file that holds the value of ref.
func fileName(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])[:32]
}

// Manager delivers and releases the secrets of Runs. Its methods serialise
// with each other, across goroutines and processes, through a lock on each
// run directory.
type Manager struct {
	fs fsOps
	// tree and keep say what a registry of rendered files that cannot be read
	// discards (see DiscardingConfig).
	tree string
	keep func(runDir string) []string
}

// New is a Manager over the real file system.
func New() *Manager { return &Manager{fs: osOps()} }

// DiscardingConfig sets what Release removes when the registry of the rendered
// files cannot be read, so that their paths are not known: the directory tree
// of the run directory (slash-separated, relative to it) that holds them. The
// owner of the tree names it, and with keep the files of it that are not
// secrets of this package and that its owner removes itself: keep(runDir) are
// paths relative to runDir, each directly inside tree. Without a tree, such a
// registry is left pending. It returns m.
func (m *Manager) DiscardingConfig(tree string, keep func(runDir string) []string) *Manager {
	m.tree, m.keep = tree, keep
	return m
}

// fsOps are the steps that change the disk; tests replace them to fail, or
// to crash, at each one.
type fsOps struct {
	mkdir     func(path string) error
	writeFile func(path string, data []byte) error
	sync      func(path string) error
	rename    func(from, to string) error
	removeAll func(path string) error
}

func osOps() fsOps {
	return fsOps{
		mkdir:     mkdirPrivate,
		writeFile: writeNew,
		sync:      syncPath,
		rename:    os.Rename,
		removeAll: removeTree,
	}
}

func mkdirPrivate(path string) error {
	if err := os.Mkdir(path, dirPerm); err != nil {
		return err
	}
	return os.Chmod(path, dirPerm) // whatever the umask
}

// writeNew creates path, which must not exist, and writes data to it. It does
// not sync.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, filePerm) //nolint:gosec // a path of this package under the run directory
	if err != nil {
		return err
	}
	err = f.Chmod(filePerm)
	if err == nil {
		_, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func syncPath(path string) error {
	f, err := os.Open(path) //nolint:gosec // a path of this package under the run directory
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// removeTree removes path and everything under it. The harness runs as this
// user and may have taken the write permission off a directory, so a failed
// removal makes every directory writable again and tries once more.
func removeTree(path string) error {
	if os.RemoveAll(path) == nil {
		return nil
	}
	if root, err := os.OpenRoot(path); err == nil {
		_ = os.Chmod(path, dirPerm)
		_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = root.Chmod(p, dirPerm)
			}
			return nil
		})
		_ = root.Close()
	}
	return os.RemoveAll(path)
}

// present reports whether path exists, of any type.
func present(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}
