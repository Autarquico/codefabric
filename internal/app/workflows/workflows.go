// Package workflows is the use cases of Workflows (§8.9): commit a version of
// native harness configuration, list, show and resolve them, and the blobs
// their manifests name.
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Service runs the Workflow use cases.
type Service struct {
	db           *store.DB
	clock        clock.Clock
	ids          *domain.IDGenerator
	blobs        BlobStore
	maxBlobBytes int64
}

// Option adjusts New.
type Option func(*Service)

// WithMaxBlobBytes sets the largest blob PutBlob accepts.
func WithMaxBlobBytes(n int64) Option { return func(s *Service) { s.maxBlobBytes = n } }

// New builds the service. blobs may be nil, for a Controller without a blob
// store: committing and the blob operations then fail with CF_INTERNAL.
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, blobs BlobStore, opts ...Option) *Service {
	s := &Service{db: db, clock: clk, ids: ids, blobs: blobs, maxBlobBytes: DefaultMaxBlobBytes}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Author is the principal that made a version.
type Author struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ChangedComponent is a component a version touched against its parent.
type ChangedComponent = store.ChangedComponent

// Version is one version of a Workflow. Manifest is only set by Show.
type Version struct {
	Workflow   string             `json:"workflow"`
	WorkflowID string             `json:"workflow_id"`
	Version    int                `json:"version"`
	Parent     int                `json:"parent,omitempty"`
	Digest     string             `json:"digest"`
	Author     Author             `json:"author"`
	Origin     string             `json:"origin"`
	Changelog  string             `json:"changelog,omitempty"`
	Changed    []ChangedComponent `json:"changed"`
	CreatedAt  time.Time          `json:"created_at"`
	Manifest   *domain.Manifest   `json:"manifest,omitempty"`
}

// Summary is a Workflow with its head; Head is 0 for one without versions.
type Summary struct {
	Name       string    `json:"name"`
	ID         string    `json:"id"`
	Head       int       `json:"head"`
	HeadDigest string    `json:"head_digest,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// List returns every Workflow, by name.
func (s *Service) List(ctx context.Context) ([]Summary, error) {
	var out []Summary
	err := s.db.Read(ctx, func(q store.Querier) error {
		heads, err := store.ListWorkflows(ctx, q)
		for _, h := range heads {
			out = append(out, Summary{
				Name: h.Name, ID: string(h.ID), Head: h.Head, HeadDigest: h.HeadDigest, CreatedAt: h.CreatedAt,
			})
		}
		return err
	})
	return out, err
}

// Show returns the version a selector (name, name@latest, name@N,
// name@sha256:...) points to, with its manifest.
func (s *Service) Show(ctx context.Context, selector string) (Version, error) {
	var out Version
	err := s.db.Read(ctx, func(q store.Querier) error {
		ref, err := Resolve(ctx, q, selector)
		if err != nil {
			return err
		}
		w, err := store.WorkflowByID(ctx, q, ref.ID)
		if err != nil {
			return err
		}
		v, err := store.WorkflowVersionByNumber(ctx, q, ref.ID, ref.Version)
		if err != nil {
			return err
		}
		out = view(w.Name, v)
		var m domain.Manifest
		if err := json.Unmarshal(v.Manifest, &m); err != nil {
			return fmt.Errorf("workflows: stored manifest of %s version %d: %w", w.Name, v.Version, err)
		}
		out.Manifest = &m
		return nil
	})
	return out, err
}

// History lists the versions of a Workflow, newest first, without manifests.
func (s *Service) History(ctx context.Context, name string) ([]Version, error) {
	var out []Version
	err := s.db.Read(ctx, func(q store.Querier) error {
		w, err := workflowNamed(ctx, q, name)
		if err != nil {
			return err
		}
		versions, err := store.WorkflowHistory(ctx, q, w.ID)
		for _, v := range versions {
			out = append(out, view(w.Name, v))
		}
		return err
	})
	return out, err
}

// Resolve answers which version a selector points to: a name (the latest),
// name@latest, name@N or name@sha256:<hex> (the latest version with that
// digest). It reads through q, so it can run inside the transaction that
// pins a Run to the version.
func Resolve(ctx context.Context, q store.Querier, selector string) (domain.WorkflowRef, error) {
	sel, err := domain.ParseWorkflowRef(selector)
	if err != nil {
		return domain.WorkflowRef{}, input.Invalid("%q is not a workflow reference: name, name@latest, name@N or name@sha256:<hex>", selector)
	}
	w, err := workflowNamed(ctx, q, sel.Name)
	if err != nil {
		return domain.WorkflowRef{}, err
	}
	var v store.WorkflowVersion
	switch {
	case sel.Version > 0:
		v, err = store.WorkflowVersionByNumber(ctx, q, w.ID, sel.Version)
	case sel.Digest != "":
		v, err = store.WorkflowVersionByDigest(ctx, q, w.ID, sel.Digest)
	default:
		v, err = store.WorkflowHeadVersion(ctx, q, w.ID)
	}
	if errors.Is(err, store.ErrNotFound) {
		return domain.WorkflowRef{}, notFound("workflow %q has no version %q", sel.Name, selector)
	}
	if err != nil {
		return domain.WorkflowRef{}, err
	}
	return domain.WorkflowRef{ID: w.ID, Version: v.Version, Digest: v.Digest}, nil
}

// Resolve is the package function on the service's own database.
func (s *Service) Resolve(ctx context.Context, selector string) (ref domain.WorkflowRef, err error) {
	err = s.db.Read(ctx, func(q store.Querier) error {
		ref, err = Resolve(ctx, q, selector)
		return err
	})
	return ref, err
}

func workflowNamed(ctx context.Context, q store.Querier, name string) (store.Workflow, error) {
	w, err := store.WorkflowByName(ctx, q, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Workflow{}, notFound("workflow %q not found", name)
	}
	return w, err
}

func notFound(format string, args ...any) error {
	return domain.NewError(domain.CodeNotFound, fmt.Sprintf(format, args...), nil)
}

func view(name string, v store.WorkflowVersion) Version {
	changed := v.Changed
	if changed == nil {
		changed = []ChangedComponent{}
	}
	return Version{
		Workflow: name, WorkflowID: string(v.WorkflowID), Version: v.Version, Parent: v.Parent, Digest: v.Digest,
		Author: Author{Kind: v.AuthorKind, ID: v.AuthorID}, Origin: v.Origin, Changelog: v.Changelog,
		Changed: changed, CreatedAt: v.CreatedAt,
	}
}
