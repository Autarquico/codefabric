package blob

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Entry is a blob as the disk holds it.
type Entry struct {
	Digest Digest
	Size   int64
	// ModTime is when the blob was installed or last put again.
	ModTime time.Time
}

// Walk calls fn for every installed blob, in no particular order, until fn
// returns an error or ctx ends. Files that do not carry a digest's name are
// not blobs and are skipped.
func (s *Store) Walk(ctx context.Context, fn func(Entry) error) error {
	root := filepath.Join(s.root, "blobs", "sha256")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil // removed under us, or the store is still empty
		case err != nil:
			return err
		case d.IsDir():
			return nil
		}
		digest, perr := ParseDigest(digestPrefix + d.Name())
		if perr != nil || s.path(digest) != path {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return fn(Entry{Digest: digest, Size: info.Size(), ModTime: info.ModTime()})
	})
	if err != nil {
		return fmt.Errorf("blob: walking the store: %w", err)
	}
	return nil
}

// Delete removes the blob unless it was installed or put again after
// notAfter, and says whether it did. The mtime is read under the lock that
// Put holds while it installs, so a Put that races with Delete is either seen
// (the blob stays) or comes after the unlink (and installs the blob anew).
func (s *Store) Delete(d Digest, notAfter time.Time) (bool, error) {
	if err := d.validate(); err != nil {
		return false, err
	}
	s.installing.Lock()
	defer s.installing.Unlock()
	info, err := os.Stat(s.path(d))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("blob: stat %s: %w", d, err)
	case info.ModTime().After(notAfter):
		return false, nil
	}
	if s.afterStat != nil {
		s.afterStat()
	}
	if err := os.Remove(s.path(d)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("blob: removing %s: %w", d, err)
	}
	return true, nil
}
