package checkpoints

import (
	"context"
	"errors"
	"fmt"
	"io"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// verifyCapture checks what a Worker claims of a capture before it is
// published: the ref is in the relay at the reported commit, and the native
// state is a blob the Controller holds.
func (s *Service) verifyCapture(ctx context.Context, q store.Querier, task store.Task, cp store.Checkpoint, c Completion) error {
	if !domain.ValidCommitID(c.Commit) {
		return input.Invalid("a completed capture names the commit its ref points to")
	}
	if s.relay == nil {
		return domain.NewError(domain.CodeInternal, "the Controller has no relay to verify the capture against", nil)
	}
	tip, found, err := s.relay.RefTip(ctx, domain.ID(task.ProjectID), cp.SourceRef)
	if err != nil {
		return fmt.Errorf("checkpoints: asking the relay for %s: %w", cp.SourceRef, err)
	}
	if !found || tip != c.Commit {
		return input.Invalid("%s is not at %s in the relay: push it before reporting the capture", cp.SourceRef, c.Commit)
	}
	n := c.NativeState
	if n == nil {
		return nil
	}
	if err := requireNativeState(ctx, q, task.ID); err != nil {
		return err
	}
	if n.Adapter == "" {
		return input.Invalid("native state needs the adapter that took it")
	}
	if _, err := blob.ParseDigest(n.Digest); err != nil {
		return input.Invalid("native state digest must be sha256:<64 lower-case hex characters>")
	}
	uploaded, err := store.NativeUpload(ctx, q, cp.ID)
	if err != nil {
		return err
	}
	if uploaded != n.Digest {
		return input.Invalid("native state %s was not uploaded for this checkpoint", n.Digest)
	}
	if _, err := store.BlobMeta(ctx, q, n.Digest); errors.Is(err, store.ErrNotFound) {
		return input.Invalid("native state %s was not uploaded", n.Digest)
	} else if err != nil {
		return err
	}
	return nil
}

// requireNativeState denies native state to a Project whose owner switched it
// off (native_state, migration 0504): the Controller neither stores nor serves
// it, whatever the Worker does.
func requireNativeState(ctx context.Context, q store.Querier, task domain.ID) error {
	t, err := store.TaskByID(ctx, q, task)
	if err != nil {
		return err
	}
	project, err := store.ProjectByID(ctx, q, t.ProjectID)
	if err != nil {
		return err
	}
	if !project.NativeState {
		return domain.NewError(domain.CodePolicyDenied, "the Project does not keep native state", nil)
	}
	return nil
}

// ownCheckpoint is the checkpoint a Worker names, which must be one of the Run
// it holds: a checkpoint of any other Run is not its to read or write.
func (s *Service) ownCheckpoint(ctx context.Context, q store.Querier, by pki.Session, runID domain.ID, epoch uint64, id domain.ID) (store.Run, store.Checkpoint, error) {
	run, _, err := s.held(ctx, q, by, runID, epoch)
	if err != nil {
		return store.Run{}, store.Checkpoint{}, err
	}
	cp, err := store.CheckpointByID(ctx, q, id)
	if err != nil {
		return store.Run{}, store.Checkpoint{}, notFound(err, "checkpoint")
	}
	if cp.RunID != runID {
		return store.Run{}, store.Checkpoint{}, domain.NewError(domain.CodePolicyDenied, "this checkpoint is not of the Run the Worker holds", nil)
	}
	return run, cp, nil
}

// PutNativeState stores the native state of a capture under the digest the
// Worker declares, for a checkpoint of its Run that is still a reservation. It
// installs nothing if the content is larger than the limit or does not hash to
// the digest. The blob is only reachable through OpenNativeState: the generic
// blob routes refuse a digest that a checkpoint holds.
func (s *Service) PutNativeState(ctx context.Context, by pki.Session, runID domain.ID, epoch uint64, id domain.ID, digest string, r io.Reader) (v1.NativeStateUploaded, error) {
	if s.blobs == nil {
		return v1.NativeStateUploaded{}, domain.NewError(domain.CodeInternal, "the Controller has no blob store", nil)
	}
	d, err := blob.ParseDigest(digest)
	if err != nil {
		return v1.NativeStateUploaded{}, input.Invalid("digest must be sha256:<64 lower-case hex characters>")
	}
	err = s.db.Read(ctx, func(q store.Querier) error {
		run, cp, err := s.ownCheckpoint(ctx, q, by, runID, epoch, id)
		if err != nil {
			return err
		}
		if err := requireNativeState(ctx, q, run.TaskID); err != nil {
			return err
		}
		published, err := store.CheckpointPublishedAt(ctx, q, cp.ID)
		if err != nil {
			return err
		}
		if published != nil {
			return domain.NewError(domain.CodeInvalidTransition, "the checkpoint was already published", nil)
		}
		return nil
	})
	if err != nil {
		return v1.NativeStateUploaded{}, err
	}
	_, size, err := s.blobs.Put(ctx, &limitReader{r: r, left: s.maxNative}, d)
	switch {
	case errors.Is(err, errTooLarge):
		return v1.NativeStateUploaded{}, input.Invalid("native state is at most %d bytes", s.maxNative)
	case errors.Is(err, blob.ErrDigestMismatch):
		return v1.NativeStateUploaded{}, input.Invalid("the content does not hash to %s", digest)
	case err != nil:
		return v1.NativeStateUploaded{}, err
	}
	err = s.db.WithTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.PutBlobMeta(ctx, store.Blob{Digest: digest, Size: size, CreatedAt: s.clock.Now()}); err != nil {
			return err
		}
		return raced(tx.RecordNativeUpload(ctx, runID, id, digest))
	})
	return v1.NativeStateUploaded{Digest: digest, Size: size}, err
}

// OpenNativeState is the native state of a published checkpoint, and its size,
// for the Worker that holds a Run of the same Task under its live lease: the
// Run that took it, or the one that resumes from it. A checkpoint of another
// Task is denied.
func (s *Service) OpenNativeState(ctx context.Context, by pki.Session, runID domain.ID, epoch uint64, id domain.ID) (io.ReadCloser, int64, error) {
	if s.blobs == nil {
		return nil, 0, domain.NewError(domain.CodeInternal, "the Controller has no blob store", nil)
	}
	var meta store.Blob
	err := s.db.Read(ctx, func(q store.Querier) error {
		run, _, err := s.held(ctx, q, by, runID, epoch)
		if err != nil {
			return err
		}
		cp, err := store.CheckpointByID(ctx, q, id)
		if err != nil {
			return notFound(err, "checkpoint")
		}
		owner, err := store.RunByID(ctx, q, cp.RunID)
		if err != nil {
			return err
		}
		if owner.TaskID != run.TaskID {
			return domain.NewError(domain.CodePolicyDenied, "this checkpoint is not of the Task the Worker holds", nil)
		}
		if err := requireNativeState(ctx, q, run.TaskID); err != nil {
			return err
		}
		published, err := store.CheckpointPublishedAt(ctx, q, cp.ID)
		if err != nil {
			return err
		}
		if published == nil || cp.NativeState == nil {
			return domain.NewError(domain.CodeNotFound, "this checkpoint has no native state", nil)
		}
		meta, err = store.BlobMeta(ctx, q, cp.NativeState.Digest)
		return notFound(err, "native state")
	})
	if err != nil {
		return nil, 0, err
	}
	d, err := blob.ParseDigest(meta.Digest)
	if err != nil {
		return nil, 0, err
	}
	rc, err := s.blobs.Open(d)
	return rc, meta.Size, err
}

// PutNativeSeed stores the session a client uploads to seed a Run of the Project
// (lineage.native_seed) under the digest it declares. Like the native state of a
// checkpoint it needs the Project to have opted in, installs nothing if the content
// is larger than the limit or does not hash to the digest, and is only reachable
// through OpenNativeSeed, never through /v1/blobs.
func (s *Service) PutNativeSeed(ctx context.Context, by pki.Session, project, digest string, r io.Reader) (v1.NativeSeedUploaded, error) {
	if s.blobs == nil {
		return v1.NativeSeedUploaded{}, domain.NewError(domain.CodeInternal, "the Controller has no blob store", nil)
	}
	d, err := blob.ParseDigest(digest)
	if err != nil {
		return v1.NativeSeedUploaded{}, input.Invalid("digest must be sha256:<64 lower-case hex characters>")
	}
	var projectID domain.ID
	err = s.db.Read(ctx, func(q store.Querier) error {
		if _, err := pki.RequireCaller(ctx, q, by, s.clock.Now()); err != nil {
			return err
		}
		if projectID, err = refs.Project(ctx, q, project); err != nil {
			return err
		}
		p, err := store.ProjectByID(ctx, q, projectID)
		if err != nil {
			return err
		}
		if !p.NativeState {
			return domain.NewError(domain.CodePolicyDenied, "the Project does not keep native state", nil)
		}
		return nil
	})
	if err != nil {
		return v1.NativeSeedUploaded{}, err
	}
	_, size, err := s.blobs.Put(ctx, &limitReader{r: r, left: s.maxNative}, d)
	switch {
	case errors.Is(err, errTooLarge):
		return v1.NativeSeedUploaded{}, input.Invalid("a session seed is at most %d bytes", s.maxNative)
	case errors.Is(err, blob.ErrDigestMismatch):
		return v1.NativeSeedUploaded{}, input.Invalid("the content does not hash to %s", digest)
	case err != nil:
		return v1.NativeSeedUploaded{}, err
	}
	err = s.db.WithTx(ctx, func(tx *store.Tx) error {
		fresh, err := tx.PutBlobMeta(ctx, store.Blob{Digest: digest, Size: size, CreatedAt: s.clock.Now()})
		if err != nil {
			return err
		}
		if !fresh {
			// A seed uploaded again is wanted again: its grace starts anew.
			if err := tx.RenewBlobMeta(ctx, digest, s.clock.Now()); err != nil {
				return err
			}
		}
		return tx.RecordNativeSeed(ctx, projectID, digest, string(by.Principal.ID), s.clock.Now())
	})
	return v1.NativeSeedUploaded{Digest: digest, Size: size}, err
}

// OpenNativeSeed is the session a Run was seeded with, and its size, for the Worker
// that holds the Run under its live lease.
func (s *Service) OpenNativeSeed(ctx context.Context, by pki.Session, runID domain.ID, epoch uint64) (io.ReadCloser, int64, error) {
	if s.blobs == nil {
		return nil, 0, domain.NewError(domain.CodeInternal, "the Controller has no blob store", nil)
	}
	var meta store.Blob
	err := s.db.Read(ctx, func(q store.Querier) error {
		run, _, err := s.held(ctx, q, by, runID, epoch)
		if err != nil {
			return err
		}
		if err := requireNativeState(ctx, q, run.TaskID); err != nil {
			return err
		}
		seed, err := store.RunSeed(ctx, q, run.ID)
		if err != nil {
			return err
		}
		if seed == nil {
			return domain.NewError(domain.CodeNotFound, "this Run was not seeded with a session", nil)
		}
		meta, err = store.BlobMeta(ctx, q, seed.Digest)
		return notFound(err, "session seed")
	})
	if err != nil {
		return nil, 0, err
	}
	d, err := blob.ParseDigest(meta.Digest)
	if err != nil {
		return nil, 0, err
	}
	rc, err := s.blobs.Open(d)
	return rc, meta.Size, err
}

var errTooLarge = errors.New("checkpoints: native state over the limit")

// limitReader fails, instead of truncating, when r has more than left bytes.
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
