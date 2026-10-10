// Package blob is a content-addressed file store keyed by SHA-256.
//
// A blob is immutable: once a digest is installed it is never replaced, so a
// reader holding it open always sees the bytes that hash to its name.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const (
	digestPrefix = "sha256:"
	hexLen       = sha256.Size * 2
	tmpDirName   = "tmp"
	staleTmpAge  = time.Hour
)

// Errors reported by the store.
var (
	ErrMalformedDigest = errors.New("malformed digest")
	ErrDigestMismatch  = errors.New("content does not match the expected digest")
	ErrCorruptBlob     = errors.New("stored blob does not match its digest")
)

// Digest is "sha256:<64 lowercase hex characters>".
type Digest string

// ParseDigest validates the textual form of a Digest.
func ParseDigest(s string) (Digest, error) {
	d := Digest(s)
	if err := d.validate(); err != nil {
		return "", err
	}
	return d, nil
}

// validate is the single definition of a well-formed Digest. Every Store
// method that builds a path calls it first, so a zero or hostile Digest can
// neither panic nor escape the root.
func (d Digest) validate() error {
	hexPart, ok := strings.CutPrefix(string(d), digestPrefix)
	if !ok || len(hexPart) != hexLen {
		return fmt.Errorf("%w: %q", ErrMalformedDigest, string(d))
	}
	if raw, err := hex.DecodeString(hexPart); err != nil || hex.EncodeToString(raw) != hexPart {
		return fmt.Errorf("%w: %q", ErrMalformedDigest, string(d))
	}
	return nil
}

func (d Digest) String() string { return string(d) }

func (d Digest) hex() string { return strings.TrimPrefix(string(d), digestPrefix) }

// Store keeps blobs below a root directory.
type Store struct {
	root string
}

// New prepares root and removes temporary files abandoned by earlier crashes.
func New(root string, clk clock.Clock) (*Store, error) {
	s := &Store{root: root}
	if err := os.MkdirAll(s.tmpDir(), 0o750); err != nil {
		return nil, fmt.Errorf("blob: preparing %s: %w", root, err)
	}
	if err := s.removeStaleTemps(clk.Now()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) tmpDir() string { return filepath.Join(s.root, tmpDirName) }

func (s *Store) path(d Digest) string {
	h := d.hex()
	return filepath.Join(s.root, "blobs", "sha256", h[0:2], h[2:4], h)
}

func (s *Store) removeStaleTemps(now time.Time) error {
	entries, err := os.ReadDir(s.tmpDir())
	if err != nil {
		return fmt.Errorf("blob: listing temporaries: %w", err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue // vanished between ReadDir and Info
		}
		if now.Sub(info.ModTime()) > staleTmpAge {
			if err := os.Remove(filepath.Join(s.tmpDir(), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("blob: removing stale temporary: %w", err)
			}
		}
	}
	return nil
}

// Has reports whether the blob is installed.
func (s *Store) Has(d Digest) (bool, error) {
	if err := d.validate(); err != nil {
		return false, err
	}
	_, err := os.Stat(s.path(d))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("blob: stat %s: %w", d, err)
	}
}

// Open returns a reader for the blob, or a CF_NOT_FOUND error.
func (s *Store) Open(d Digest) (io.ReadCloser, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(d))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, domain.NewError(domain.CodeNotFound, "blob not found", map[string]any{"digest": string(d)})
	}
	if err != nil {
		return nil, fmt.Errorf("blob: open %s: %w", d, err)
	}
	return f, nil
}

// Put stores the content of r and returns its digest and size. If want is not
// empty and the content hashes to something else, nothing is stored and
// ErrDigestMismatch is returned. Putting content that is already stored is a
// successful no-op.
func (s *Store) Put(ctx context.Context, r io.Reader, want Digest) (_ Digest, _ int64, err error) {
	if want != "" {
		if err := want.validate(); err != nil {
			return "", 0, err
		}
	}
	tmp, err := os.CreateTemp(s.tmpDir(), "put-*")
	if err != nil {
		return "", 0, fmt.Errorf("blob: creating temporary: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name()) // after a successful Link this only drops the extra name
	}()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hash), &ctxReader{ctx: ctx, r: r})
	if err != nil {
		return "", 0, fmt.Errorf("blob: writing temporary: %w", err)
	}
	got := Digest(digestPrefix + hex.EncodeToString(hash.Sum(nil)))
	if want != "" && want != got {
		return "", 0, fmt.Errorf("%w: want %s, got %s", ErrDigestMismatch, want, got)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("blob: syncing temporary: %w", err)
	}
	if err := s.install(tmp.Name(), got, size); err != nil {
		return "", 0, err
	}
	return got, size, nil
}

// install links tmp at the final path. os.Link fails with EEXIST instead of
// replacing, which closes the race between two Puts of the same digest.
func (s *Store) install(tmp string, d Digest, size int64) error {
	dst := s.path(d)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("blob: preparing directory: %w", err)
	}
	err := os.Link(tmp, dst)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrExist):
		if err := s.verifyInstalled(dst, d, size); err != nil {
			return err
		}
	default:
		return fmt.Errorf("blob: installing %s: %w", d, err)
	}
	return s.makeDurable(dst)
}

// makeDurable syncs the blob and every directory entry from it up to the
// store root. It runs on every successful Put, also when the blob already
// existed: an earlier installer may have failed or not yet finished syncing.
func (s *Store) makeDurable(dst string) error {
	if err := syncFile(dst); err != nil {
		return err
	}
	for dir := filepath.Dir(dst); ; dir = filepath.Dir(dir) {
		if err := syncDir(dir); err != nil {
			return err
		}
		if dir == s.root {
			return nil
		}
	}
}

// verifyInstalled re-hashes an existing blob; it is reported, never repaired.
func (s *Store) verifyInstalled(path string, d Digest, size int64) error {
	f, err := os.Open(path) //nolint:gosec // path comes from a validated digest
	if err != nil {
		return fmt.Errorf("blob: opening installed %s: %w", d, err)
	}
	defer func() { _ = f.Close() }()

	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err != nil {
		return fmt.Errorf("blob: reading installed %s: %w", d, err)
	}
	if n != size || Digest(digestPrefix+hex.EncodeToString(hash.Sum(nil))) != d {
		return fmt.Errorf("%w: %s", ErrCorruptBlob, d)
	}
	return nil
}

// ctxReader makes a long copy stop when its context ends.
type ctxReader struct {
	ctx context.Context //nolint:containedctx // wraps a single io.Copy call
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
