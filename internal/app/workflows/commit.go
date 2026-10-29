package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"

	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

const (
	maxChangelogBytes = 4 << 10
	// maxManifestBytes bounds the content a commit makes the Controller read
	// back to verify it, counting a blob once per file that names it.
	maxManifestBytes = 64 << 20
)

// CommitRequest is a new version of a Workflow. Parent is the version it was
// made on top of; 0 adopts: it creates the Workflow with this as version 1.
type CommitRequest struct {
	RequestID string          `json:"request_id"`
	Name      string          `json:"name"`
	Parent    int             `json:"parent"`
	Manifest  domain.Manifest `json:"manifest"`
	Changelog string          `json:"changelog,omitempty"`
}

// commitOutcome is what the transaction of a commit saves as its response. A
// conflict is an outcome, not an error: the WorkflowConflict event it writes
// would be rolled back with a failed transaction.
type commitOutcome struct {
	Version  Version `json:"version"`
	Conflict bool    `json:"conflict,omitempty"`
	Head     int     `json:"head,omitempty"`
}

// Commit stores req.Manifest as the next version of the Workflow, if req.Parent
// is still its head (CF_WORKFLOW_CONFLICT otherwise). The manifest must be
// valid, every blob it names uploaded, and its digests must be those of the
// content the Controller holds. Committing the content of the head is refused
// as nothing to commit (CF_INVALID_REQUEST); the content of an earlier version
// is a new version.
func (s *Service) Commit(ctx context.Context, by pki.Session, req CommitRequest) (Version, error) {
	if s.blobs == nil {
		return Version{}, domain.NewError(domain.CodeInternal, "the Controller has no blob store", nil)
	}
	if err := input.First(input.Name("name", req.Name), validParentAndChangelog(req)); err != nil {
		return Version{}, err
	}
	if err := req.Manifest.Validate(); err != nil {
		return Version{}, input.Invalid("%v", err)
	}
	if err := s.verify(ctx, req.Manifest); err != nil {
		return Version{}, err
	}
	canonical, err := req.Manifest.Canonical()
	if err != nil {
		return Version{}, input.Invalid("%v", err)
	}
	digest, err := req.Manifest.Digest()
	if err != nil {
		return Version{}, input.Invalid("%v", err)
	}
	origin := "client"
	if by.Principal.Kind == domain.KindWorker {
		origin = string(by.Principal.ID)
	}

	out, err := mutation.Do(ctx, s.db, s.clock, by, "workflow.commit", req.RequestID, req,
		func(tx *store.Tx, act mutation.Act) (commitOutcome, error) {
			c := commit{s: s, tx: tx, act: act, req: req, canonical: canonical, digest: digest, origin: origin, author: string(by.Principal.Kind)}
			return c.run(ctx)
		})
	if err != nil {
		return Version{}, err
	}
	if out.Conflict && req.Parent == 0 {
		return Version{}, domain.NewError(domain.CodeWorkflowConflict,
			fmt.Sprintf("workflow %q already exists at version %d: use `cf workflow commit`", req.Name, out.Head),
			map[string]any{"head": out.Head, "parent": req.Parent})
	}
	if out.Conflict {
		return Version{}, domain.NewError(domain.CodeWorkflowConflict,
			fmt.Sprintf("workflow %q is at version %d, not %d: rebase on the head", req.Name, out.Head, req.Parent),
			map[string]any{"head": out.Head, "parent": req.Parent})
	}
	return out.Version, nil
}

func validParentAndChangelog(req CommitRequest) error {
	if req.Parent < 0 {
		return input.Invalid("parent must be 0 (adopt) or a version")
	}
	if len(req.Changelog) > maxChangelogBytes || !utf8.ValidString(req.Changelog) {
		return input.Invalid("changelog must be UTF-8 of at most %d bytes", maxChangelogBytes)
	}
	return nil
}

type commit struct {
	s         *Service
	tx        *store.Tx
	act       mutation.Act
	req       CommitRequest
	canonical []byte
	digest    string
	origin    string
	author    string // the kind of the principal
}

func (c commit) run(ctx context.Context) (commitOutcome, error) {
	w, head, err := c.target(ctx)
	if err != nil {
		return commitOutcome{}, err
	}
	if head.Version != c.req.Parent {
		return c.conflict(ctx, w, head.Version)
	}
	if head.Version > 0 && head.Digest == c.digest {
		return commitOutcome{}, domain.NewError(domain.CodeInvalidRequest,
			fmt.Sprintf("nothing to commit: the manifest is that of version %d of %q", head.Version, w.Name),
			map[string]any{"nothing_to_commit": true, "head": head.Version})
	}
	changed, err := changedAgainst(head.Manifest, c.req.Manifest)
	if err != nil {
		return commitOutcome{}, err
	}
	v := store.WorkflowVersion{
		WorkflowID: w.ID, Version: head.Version + 1, Parent: head.Version, Digest: c.digest, Manifest: c.canonical,
		AuthorKind: c.author, AuthorID: c.act.Actor.ID, Origin: c.origin,
		Changelog: c.req.Changelog, Changed: changed, CreatedAt: c.act.Now,
	}
	if err := c.tx.InsertWorkflowVersion(ctx, v); errors.Is(err, store.ErrWorkflowConflict) {
		return c.conflict(ctx, w, head.Version)
	} else if err != nil {
		return commitOutcome{}, err
	}
	eventType, action := domain.EventWorkflowCommitted, "workflow.committed"
	if v.Version == 1 {
		eventType, action = domain.EventWorkflowAdopted, "workflow.adopted"
	}
	ev := c.act.Event(eventType, fmt.Sprintf("workflow %s version %d", w.Name, v.Version))
	ev.WorkflowID = w.ID
	ev.Data = map[string]any{
		"name": w.Name, "version": v.Version, "parent": v.Parent, "digest": v.Digest, "origin": v.Origin,
		"changed": len(changed),
	}
	if _, _, err := c.tx.AppendEvent(ctx, ev); err != nil {
		return commitOutcome{}, err
	}
	out := view(w.Name, v)
	if err := c.tx.AppendAudit(ctx, store.AuditEntry{
		Time: c.act.Now, ActorKind: c.act.Actor.Kind, ActorID: c.act.Actor.ID, Action: action,
		ResourceKind: "workflow", ResourceID: string(w.ID), After: out, Result: "ok",
	}); err != nil {
		return commitOutcome{}, err
	}
	return commitOutcome{Version: out}, nil
}

// target finds the Workflow the commit is for and its head (zero when it has
// none). An adopt creates the Workflow; any other parent needs it to exist.
func (c commit) target(ctx context.Context) (store.Workflow, store.WorkflowVersion, error) {
	w, err := store.WorkflowByName(ctx, c.tx, c.req.Name)
	switch {
	case errors.Is(err, store.ErrNotFound) && c.req.Parent == 0:
		w = store.Workflow{ID: c.s.ids.New(domain.KindWorkflow), Name: c.req.Name, CreatedBy: c.act.Actor.ID, CreatedAt: c.act.Now}
		return w, store.WorkflowVersion{}, c.tx.InsertWorkflow(ctx, w)
	case errors.Is(err, store.ErrNotFound):
		return w, store.WorkflowVersion{}, notFound("workflow %q not found", c.req.Name)
	case err != nil:
		return w, store.WorkflowVersion{}, err
	}
	head, err := store.WorkflowHeadVersion(ctx, c.tx, w.ID)
	if errors.Is(err, store.ErrNotFound) {
		return w, store.WorkflowVersion{}, nil
	}
	return w, head, err
}

func (c commit) conflict(ctx context.Context, w store.Workflow, head int) (commitOutcome, error) {
	ev := c.act.Event(domain.EventWorkflowConflict, fmt.Sprintf("commit on %s version %d lost: the head is %d", w.Name, c.req.Parent, head))
	ev.Level, ev.WorkflowID = domain.LevelWarn, w.ID
	ev.Data = map[string]any{"name": w.Name, "parent": c.req.Parent, "head": head, "digest": c.digest, "origin": c.origin}
	if _, _, err := c.tx.AppendEvent(ctx, ev); err != nil {
		return commitOutcome{}, err
	}
	return commitOutcome{Conflict: true, Head: head}, nil
}

// changedAgainst lists the components the commit touches against the head's
// stored (canonical) manifest; an empty one is no version.
func changedAgainst(head []byte, next domain.Manifest) ([]ChangedComponent, error) {
	var base *domain.Manifest
	if len(head) > 0 {
		base = new(domain.Manifest)
		if err := json.Unmarshal(head, base); err != nil {
			return nil, fmt.Errorf("workflows: reading the head manifest: %w", err)
		}
	}
	out := []ChangedComponent{}
	for _, ch := range next.ChangesFrom(base) {
		out = append(out, ChangedComponent(ch))
	}
	return out, nil
}

// verify is the integrity check of a commit: every file's blob was uploaded,
// the content the Controller holds hashes to the declared sha256, and each
// component's tree_digest is that of its files, and each file's secret_refs
// are those of the markers in its content. The caller's claims are never taken
// for the content's.
func (s *Service) verify(ctx context.Context, m domain.Manifest) error {
	sizes, err := s.blobSizes(ctx, m)
	if err != nil {
		return err
	}
	for _, comp := range m.Components {
		id := comp.Harness + "/" + comp.Name
		entries := make([]treedigest.Entry, 0, len(comp.Files))
		for _, f := range comp.Files {
			content, err := s.readVerified(f, sizes[f.SHA256])
			if err != nil {
				return input.Invalid("component %s: %v", id, err)
			}
			if !slices.Equal(domain.SecretRefsIn(content), sortedCopy(f.SecretRefs)) {
				return input.Invalid("component %s: file %q: secret_refs do not match the secret markers in its content", id, f.Path)
			}
			entries = append(entries, treedigest.File{Path: f.Path, Exec: f.Exec, Content: content})
		}
		got, err := treedigest.Files(entries)
		if err != nil {
			return input.Invalid("component %s: %v", id, err)
		}
		if got != comp.TreeDigest {
			return input.Invalid("component %s: tree_digest does not match its files (computed %s)", id, got)
		}
	}
	return nil
}

func sortedCopy(refs []string) []string {
	out := append([]string{}, refs...)
	slices.Sort(out)
	return out
}

// blobSizes checks that every blob is uploaded and the whole is within bounds,
// before any content is read.
func (s *Service) blobSizes(ctx context.Context, m domain.Manifest) (map[string]int64, error) {
	sizes := map[string]int64{}
	var total int64
	err := s.db.Read(ctx, func(q store.Querier) error {
		for _, comp := range m.Components {
			for _, f := range comp.Files {
				size, known := sizes[f.SHA256]
				if !known {
					meta, err := store.BlobMeta(ctx, q, f.SHA256)
					if errors.Is(err, store.ErrNotFound) {
						return input.Invalid("component %s/%s: file %q: blob %s was not uploaded", comp.Harness, comp.Name, f.Path, f.SHA256)
					}
					if err != nil {
						return err
					}
					size = meta.Size
					sizes[f.SHA256] = size
				}
				total += size
			}
		}
		return nil
	})
	if err == nil && total > maxManifestBytes {
		return nil, input.Invalid("the files of a version add up to at most %d bytes", maxManifestBytes)
	}
	return sizes, err
}

// readVerified reads the blob of f and checks it hashes to f.SHA256; size is
// what blobs_meta recorded, which bounds the read.
func (s *Service) readVerified(f domain.ManifestFile, size int64) ([]byte, error) {
	d, err := blob.ParseDigest(f.SHA256)
	if err != nil {
		return nil, fmt.Errorf("file %q: sha256 is malformed", f.Path)
	}
	rc, err := s.blobs.Open(d)
	if err != nil {
		return nil, fmt.Errorf("file %q: blob %s cannot be read", f.Path, f.SHA256)
	}
	defer func() { _ = rc.Close() }()
	content, err := io.ReadAll(io.LimitReader(rc, size+1))
	if err != nil {
		return nil, fmt.Errorf("file %q: reading blob %s: %w", f.Path, f.SHA256, err)
	}
	sum := sha256.Sum256(content)
	if "sha256:"+hex.EncodeToString(sum[:]) != f.SHA256 {
		return nil, fmt.Errorf("file %q: the content of blob %s does not hash to it", f.Path, f.SHA256)
	}
	return content, nil
}
