package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// A Worker never reads the Workflow store at large: it reads what one Run of
// its own froze. The Run names a version, the lease says who may read it, and
// the manifest of that version says which blobs.

// RunManifest is the version of the Workflow the Run froze, with its manifest,
// for the Worker that holds the Run under lease epoch.
func (s *Service) RunManifest(ctx context.Context, by pki.Session, runID string, epoch uint64) (Version, error) {
	var out Version
	err := s.db.Read(ctx, func(q store.Querier) error {
		v, name, err := s.heldVersion(ctx, q, by, runID, epoch)
		if err != nil {
			return err
		}
		out = view(name, v)
		var m domain.Manifest
		if err := json.Unmarshal(v.Manifest, &m); err != nil {
			return fmt.Errorf("workflows: stored manifest of %s version %d: %w", name, v.Version, err)
		}
		out.Manifest = &m
		return nil
	})
	return out, err
}

// OpenRunBlob is the content of a blob the manifest of the Run's frozen
// version names, and its size, for the Worker that holds the Run under lease
// epoch. A blob the manifest does not name is not found, however the store
// holds it.
func (s *Service) OpenRunBlob(ctx context.Context, by pki.Session, runID string, epoch uint64, digest string) (io.ReadCloser, int64, error) {
	if s.blobs == nil {
		return nil, 0, domain.NewError(domain.CodeInternal, "the Controller has no blob store", nil)
	}
	d, err := parseDigest(digest)
	if err != nil {
		return nil, 0, err
	}
	var meta store.Blob
	err = s.db.Read(ctx, func(q store.Querier) error {
		v, _, err := s.heldVersion(ctx, q, by, runID, epoch)
		if err != nil {
			return err
		}
		var m domain.Manifest
		if err := json.Unmarshal(v.Manifest, &m); err != nil {
			return fmt.Errorf("workflows: stored manifest of version %d: %w", v.Version, err)
		}
		if !names(m, digest) {
			return notFound("blob %s is not in the Workflow of this Run", digest)
		}
		meta, err = store.BlobMeta(ctx, q, digest)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("blob %s not found", digest)
		}
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	rc, err := s.blobs.Open(d)
	return rc, meta.Size, err
}

func names(m domain.Manifest, digest string) bool {
	for _, c := range m.Components {
		for _, f := range c.Files {
			if f.SHA256 == digest {
				return true
			}
		}
	}
	return false
}

// heldVersion authorises the caller for the Run and finds the version the Run
// froze. The caller must be the Worker of the Run's current lease, pending or
// accepted, and epoch must be that lease's. A Worker that never held the Run,
// or holds another lease on it, is denied; one whose lease was superseded or
// closed is stale. A Run that ended, or whose Task is archived, serves nothing.
func (s *Service) heldVersion(ctx context.Context, q store.Querier, by pki.Session, runID string, epoch uint64) (store.WorkflowVersion, string, error) {
	denied := domain.NewError(domain.CodePolicyDenied, "this Worker does not hold that Run", nil)
	if kind, _, err := domain.ParseID(runID); err != nil || kind != domain.KindRun {
		return store.WorkflowVersion{}, "", domain.NewError(domain.CodeNotFound, "run not found", nil)
	}
	now := s.clock.Now()
	if _, err := pki.RequireCaller(ctx, q, by, now); err != nil {
		return store.WorkflowVersion{}, "", err
	}
	w, err := store.WorkerByIdentity(ctx, q, by.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.WorkflowVersion{}, "", domain.NewError(domain.CodeUnauthenticated, "this identity is not a Worker", nil)
	}
	if err != nil {
		return store.WorkflowVersion{}, "", err
	}
	l, err := store.LeaseByRunEpoch(ctx, q, domain.ID(runID), epoch)
	if errors.Is(err, store.ErrNotFound) || (err == nil && l.WorkerID != w.ID) {
		return store.WorkflowVersion{}, "", denied
	}
	if err != nil {
		return store.WorkflowVersion{}, "", err
	}
	r, err := store.RunByID(ctx, q, domain.ID(runID))
	if err != nil {
		return store.WorkflowVersion{}, "", err
	}
	live := l.State == domain.LeaseAccepted || (l.State == domain.LeasePending && now.Before(l.AcceptBy))
	if !live || r.LeaseID != l.ID || r.LeaseEpoch != epoch || r.State.IsTerminal() {
		return store.WorkflowVersion{}, "", domain.NewError(domain.CodeStaleLease, "this is not the live lease of the Run",
			map[string]any{"lease": string(l.ID), "epoch": l.Epoch, "state": string(l.State)})
	}
	t, err := store.TaskByID(ctx, q, r.TaskID)
	if err != nil {
		return store.WorkflowVersion{}, "", err
	}
	if t.ArchivedAt != nil {
		return store.WorkflowVersion{}, "", domain.NewError(domain.CodeNotFound, "run not found", nil)
	}
	if r.Workflow.Version == 0 {
		return store.WorkflowVersion{}, "", notFound("this Run froze no Workflow")
	}
	wf, err := store.WorkflowByID(ctx, q, r.Workflow.ID)
	if err != nil {
		return store.WorkflowVersion{}, "", err
	}
	v, err := store.WorkflowVersionByNumber(ctx, q, r.Workflow.ID, r.Workflow.Version)
	return v, wf.Name, err
}
