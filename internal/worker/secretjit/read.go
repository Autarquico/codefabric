package secretjit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

const noFollow = unix.O_NOFOLLOW

// Resolver is the harness's access to the delivery of the Run whose
// directory is runDir: the value of ref, if the harness may read it. It is
// what the wrapper resolves the secret environment with. It only reads.
func Resolver(runDir string) func(ref string) (string, error) {
	return func(ref string) (string, error) {
		d, err := readDelivery(runDir)
		if err != nil {
			return "", fmt.Errorf("secret %q: %w", ref, err)
		}
		defer d.close()
		e, ok := d.find(ref)
		if !ok || !e.Harness {
			return "", fmt.Errorf("%w: %q", ErrNoAccess, ref)
		}
		value, err := d.value(e)
		if err != nil {
			return "", err
		}
		return string(value), nil
	}
}

// Load is every value the Run whose directory is runDir holds, revoked or
// not, for the redaction of its output and the guard of its publication. It
// only reads.
func Load(runDir string) ([]redact.Needle, error) {
	d, err := readDelivery(runDir)
	if err != nil {
		return nil, err
	}
	defer d.close()
	return d.needles()
}

// delivery is an open secrets/ directory and its manifest. The files are
// opened relative to the directory, so a rename of secrets/ or a symlink in
// its place after it is opened cannot redirect a read.
type delivery struct {
	dir    *os.File
	m      manifest
	unlock func()
}

// readDelivery opens the delivery of runDir under its shared lock, which the
// delivery keeps until it is closed.
func readDelivery(runDir string) (*delivery, error) {
	unlock, err := rlock(runDir)
	if errors.Is(err, unix.ENOENT) {
		return nil, ErrNotDelivered
	}
	if err != nil {
		return nil, err
	}
	d, err := openDelivery(runDir)
	if err != nil {
		unlock()
		return nil, err
	}
	d.unlock = unlock
	return d, nil
}

// openDelivery opens the delivery of runDir; the caller holds its lock.
func openDelivery(runDir string) (*delivery, error) {
	path := filepath.Join(runDir, liveName)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|noFollow|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, ErrNotDelivered
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return nil, fmt.Errorf("%w: %s is not a directory", ErrUnsafe, path)
	case err != nil:
		return nil, fmt.Errorf("secretjit: opening %s: %w", path, err)
	}
	dir := os.NewFile(uintptr(fd), path)
	d := &delivery{dir: dir}
	if err := d.load(); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

func (d *delivery) close() {
	_ = d.dir.Close()
	if d.unlock != nil {
		d.unlock()
	}
}

func (d *delivery) load() error {
	info, err := d.dir.Stat()
	if err != nil {
		return fmt.Errorf("secretjit: %w", err)
	}
	if err := private(d.dir.Name(), info, dirPerm); err != nil {
		return err
	}
	data, err := d.read(manifestName, maxManifestBytes)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// encoding/json may quote the input in its errors: say only where.
	if err := dec.Decode(&d.m); err != nil {
		return fmt.Errorf("%w: %s is not valid (offset %d)", ErrCorrupt, manifestName, dec.InputOffset())
	}
	return d.m.check()
}

func (m manifest) check() error {
	if m.Schema != manifestSchema || m.Attempt == 0 || len(m.Refs) == 0 {
		return fmt.Errorf("%w: %s has schema %d, attempt %d and %d refs", ErrCorrupt, manifestName, m.Schema, m.Attempt, len(m.Refs))
	}
	refs := make([]string, len(m.Refs))
	for i, e := range m.Refs {
		refs[i] = e.Ref
	}
	if err := domain.CheckSecretRefs(refs, domain.MaxRunSecrets); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrCorrupt, manifestName, err)
	}
	for _, e := range m.Refs {
		if e.File != fileName(e.Ref) {
			return fmt.Errorf("%w: %s names the wrong file for %q", ErrCorrupt, manifestName, e.Ref)
		}
	}
	return nil
}

func (d *delivery) find(ref string) (entry, bool) {
	for _, e := range d.m.Refs {
		if e.Ref == ref {
			return e, true
		}
	}
	return entry{}, false
}

func (d *delivery) value(e entry) ([]byte, error) {
	v, err := d.read(e.File, domain.MaxSecretValueLen)
	if err != nil {
		return nil, fmt.Errorf("secret %q: %w", e.Ref, err)
	}
	return v, nil
}

func (d *delivery) needles() ([]redact.Needle, error) {
	out := make([]redact.Needle, 0, len(d.m.Refs))
	for _, e := range d.m.Refs {
		v, err := d.value(e)
		if err != nil {
			return nil, err
		}
		out = append(out, redact.Needle{Ref: e.Ref, Value: v})
	}
	return out, nil
}

// read is the content of name in the delivery: a private regular file of
// this user, of at most limit bytes. It does not follow a symlink, nor block
// on a FIFO.
func (d *delivery) read(name string, limit int) ([]byte, error) {
	fd, err := unix.Openat(int(d.dir.Fd()), name, unix.O_RDONLY|noFollow|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, fmt.Errorf("%w: %s is missing", ErrCorrupt, name)
	case errors.Is(err, unix.ELOOP):
		return nil, fmt.Errorf("%w: %s is a symlink", ErrUnsafe, name)
	case err != nil:
		return nil, fmt.Errorf("secretjit: opening %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("secretjit: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrUnsafe, name)
	}
	if err := private(name, info, filePerm); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("secretjit: reading %s: %w", name, err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrCorrupt, name, limit)
	}
	return data, nil
}

// private is an error unless info, of name, has no permission beyond limit
// and belongs to this user.
func private(name string, info fs.FileInfo, limit fs.FileMode) error {
	if extra := info.Mode().Perm() &^ limit; extra != 0 {
		return fmt.Errorf("%w: %s has mode %04o, more than %04o", ErrUnsafe, name, info.Mode().Perm(), limit)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: cannot tell who owns %s", ErrUnsafe, name)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s belongs to uid %d, not to this user", ErrUnsafe, name, st.Uid)
	}
	return nil
}
