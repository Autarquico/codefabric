package workflows

import (
	"context"
	"errors"
	"io"

	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// DefaultMaxBlobBytes is the largest blob PutBlob accepts unless told otherwise:
// configuration files, not datasets.
const DefaultMaxBlobBytes = 8 << 20

// BlobStore is the content-addressed store the manifests' files live in.
// *blob.Store implements it.
type BlobStore interface {
	Open(d blob.Digest) (io.ReadCloser, error)
	// Put installs the content of r only if it hashes to want, and nothing
	// if r fails.
	Put(ctx context.Context, r io.Reader, want blob.Digest) (blob.Digest, int64, error)
}

// MaxBlobBytes is the largest blob PutBlob accepts: the one limit of the upload.
func (s *Service) MaxBlobBytes() int64 { return s.maxBlobBytes }

// BlobInfo describes a blob the Controller holds.
type BlobInfo struct {
	Digest string
	Size   int64
	// Created is false when the blob was already uploaded.
	Created bool
}

// PutBlob stores the content of r under the digest the caller declares. It
// reads at most the per-blob limit and installs nothing if the content is
// larger or does not hash to digest. Workers cannot upload (until #22
// defines what a Worker may read and write).
func (s *Service) PutBlob(ctx context.Context, by pki.Session, digest string, r io.Reader) (BlobInfo, error) {
	d, err := s.blobRequest(by, digest)
	if err != nil {
		return BlobInfo{}, err
	}
	_, size, err := s.blobs.Put(ctx, &limitReader{r: r, left: s.maxBlobBytes}, d)
	switch {
	case errors.Is(err, errTooLarge):
		return BlobInfo{}, input.Invalid("a blob is at most %d bytes", s.maxBlobBytes)
	case errors.Is(err, blob.ErrDigestMismatch):
		return BlobInfo{}, input.Invalid("the content does not hash to %s", digest)
	case err != nil:
		return BlobInfo{}, err
	}
	info := BlobInfo{Digest: digest, Size: size}
	err = s.db.WithTx(ctx, func(tx *store.Tx) (err error) {
		if info.Created, err = tx.PutBlobMeta(ctx, store.Blob{Digest: digest, Size: size, CreatedAt: s.clock.Now()}); err != nil || info.Created {
			return err
		}
		meta, err := store.BlobMeta(ctx, tx, digest)
		info.Size = meta.Size
		return err
	})
	return info, err
}

// OpenBlob returns the content of an uploaded blob and its size.
func (s *Service) OpenBlob(ctx context.Context, by pki.Session, digest string) (io.ReadCloser, int64, error) {
	d, err := s.blobRequest(by, digest)
	if err != nil {
		return nil, 0, err
	}
	var meta store.Blob
	err = s.db.Read(ctx, func(q store.Querier) (err error) {
		meta, err = store.BlobMeta(ctx, q, digest)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, 0, notFound("blob %s not found", digest)
	}
	if err != nil {
		return nil, 0, err
	}
	rc, err := s.blobs.Open(d)
	return rc, meta.Size, err
}

// blobRequest checks what a blob operation on one digest needs.
func (s *Service) blobRequest(by pki.Session, digest string) (blob.Digest, error) {
	if err := s.mayUseBlobs(by); err != nil {
		return "", err
	}
	return parseDigest(digest)
}

// mayUseBlobs needs a store and a caller that is not a Worker.
func (s *Service) mayUseBlobs(by pki.Session) error {
	if s.blobs == nil {
		return domain.NewError(domain.CodeInternal, "the Controller has no blob store", nil)
	}
	if by.Principal.Kind == domain.KindWorker {
		return domain.NewError(domain.CodePolicyDenied, "Workers cannot use the blob store", nil)
	}
	return nil
}

func parseDigest(digest string) (blob.Digest, error) {
	d, err := blob.ParseDigest(digest)
	if err != nil {
		return "", input.Invalid("digest must be sha256:<64 lower-case hex characters>")
	}
	return d, nil
}

var errTooLarge = errors.New("workflows: blob over the limit")

// limitReader fails, instead of truncating, when r has more than left bytes:
// a truncated blob would hash to something else, but the error is the reason.
type limitReader struct {
	r    io.Reader
	left int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return n, errTooLarge
	}
	return n, err
}
