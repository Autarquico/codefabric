package runs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// RelayTokenTTL is how long a token for the git relay is good for.
const RelayTokenTTL = 15 * time.Minute

// relayClaims is what a token says, signed. It is only a claim: every request
// resolves the whole chain again from the database (AuthorizeRelay), so a
// token never grants more than the server's state grants at that moment.
type relayClaims struct {
	Worker  domain.ID `json:"w"`
	Run     domain.ID `json:"r"`
	Epoch   uint64    `json:"e"`
	Project domain.ID `json:"p"`
	Expires int64     `json:"x"` // Unix nanoseconds
	// Scope is "" for the token of a lease and scopeOrphan for that of an
	// orphan, which may only create its ref at Commit.
	Scope  string `json:"s,omitempty"`
	Commit string `json:"c,omitempty"`
}

const scopeOrphan = "orphan"

// RelayGrant is what AuthorizeRelay concedes to one request.
type RelayGrant struct {
	Project domain.ID
	Worker  domain.ID
	Write   bool
	// Branch and ExpectedOID are set for a write: the one ref it may update and
	// the one commit it may set it to. An orphan's write has Ref instead, a ref
	// outside refs/heads/ that the hook only lets it create.
	Branch      string
	Ref         string
	ExpectedOID string
}

func (s *Service) sign(c relayClaims) string {
	payload, _ := json.Marshal(c) // a struct of strings and numbers
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.mac(body))
}

func (s *Service) mac(body string) []byte {
	h := hmac.New(sha256.New, s.tokenKey)
	h.Write([]byte(body))
	return h.Sum(nil)
}

// verify reads a token and checks its signature and age. Any defect is the same
// answer: the caller learns nothing about which.
func (s *Service) verify(token string, now time.Time) (relayClaims, bool) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 1024 {
		return relayClaims{}, false
	}
	given, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(given, s.mac(body)) {
		return relayClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return relayClaims{}, false
	}
	var c relayClaims
	if json.Unmarshal(payload, &c) != nil || !now.Before(time.Unix(0, c.Expires)) {
		return relayClaims{}, false
	}
	return c, true
}

// relayChain is the chain a request to the relay stands on, resolved from the
// database: the Worker the caller's identity stands for, the lease it holds,
// and the Run, Task and Project that lease is for.
type relayChain struct {
	worker  store.Worker
	lease   store.Lease
	run     store.Run
	task    store.Task
	project domain.ID
}

// resolveChain walks it. The lease must be the exact (run, epoch), the
// Worker's own and accepted, and the Task must not be archived. The Run may be
// terminal: finishing the publication is what the relay is for (§16.8).
func resolveChain(ctx context.Context, q store.Querier, by pki.Session, now time.Time, run domain.ID, epoch uint64) (relayChain, error) {
	if _, err := pki.RequireCaller(ctx, q, by, now); err != nil {
		return relayChain{}, err
	}
	w, err := store.WorkerByIdentity(ctx, q, by.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return relayChain{}, domain.NewError(domain.CodeUnauthenticated, "this identity is not a Worker", nil)
	}
	if err != nil {
		return relayChain{}, err
	}
	l, err := store.LeaseByRunEpoch(ctx, q, run, epoch)
	if errors.Is(err, store.ErrNotFound) || (err == nil && l.WorkerID != w.ID) {
		return relayChain{}, domain.NewError(domain.CodeNotFound, "lease not found", nil)
	}
	if err != nil {
		return relayChain{}, err
	}
	r, err := store.RunByID(ctx, q, run)
	if err != nil {
		return relayChain{}, err
	}
	if l.State != domain.LeaseAccepted || r.LeaseEpoch != epoch {
		return relayChain{}, domain.NewError(domain.CodeStaleLease, "this is not the live lease of the Run",
			map[string]any{"lease": string(l.ID), "epoch": l.Epoch, "state": string(l.State)})
	}
	t, err := store.TaskByID(ctx, q, r.TaskID)
	if err != nil {
		return relayChain{}, err
	}
	if t.ArchivedAt != nil {
		return relayChain{}, domain.NewError(domain.CodeNotFound, "run not found", nil)
	}
	return relayChain{worker: w, lease: l, run: r, task: t, project: t.ProjectID}, nil
}

// mayPush says whether the result of the Run may still be pushed: only while
// its publication waits for it, and only once the result is sealed. Anything
// else, a published Run included, has no write access: its branch does not move.
func mayPush(r store.Run) bool {
	return r.Publication == domain.PublicationPending && r.ResultCommit != ""
}

// MintRelayToken gives a Worker a token for the git relay, under the lease it
// holds the Run with. The token reads the Project of the Run, and writes it
// only if the Run is waiting to publish a sealed result.
func (s *Service) MintRelayToken(ctx context.Context, by pki.Session, runID string, req v1.RelayTokenRequest) (v1.RelayTokenResponse, error) {
	if kind, _, err := domain.ParseID(runID); err != nil || kind != domain.KindRun {
		return v1.RelayTokenResponse{}, domain.NewError(domain.CodeNotFound, "run not found", nil)
	}
	if s.relay == nil {
		// The Worker asks again with a backoff: the relay may be enabled when git is installed.
		return v1.RelayTokenResponse{}, domain.NewError(domain.CodeInternal, "the Controller has no relay", nil)
	}
	var out v1.RelayTokenResponse
	err := s.db.Read(ctx, func(q store.Querier) error {
		now := s.clock.Now()
		c, err := resolveChain(ctx, q, by, now, domain.ID(runID), req.LeaseEpoch)
		if err != nil {
			return err
		}
		expires := now.Add(RelayTokenTTL)
		out = v1.RelayTokenResponse{
			Token:     s.sign(relayClaims{Worker: c.worker.ID, Run: c.run.ID, Epoch: req.LeaseEpoch, Project: c.project, Expires: expires.UnixNano()}),
			ExpiresAt: expires, Path: v1.RelayPathPrefix + string(c.project) + ".git", Write: mayPush(c.run),
		}
		return nil
	})
	return out, err
}

// errRelayDenied is the one answer to every request the relay refuses to authorise.
func errRelayDenied() error {
	return domain.NewError(domain.CodePolicyDenied, "this request may not use the git relay", nil)
}

// AuthorizeRelay decides one request to the git relay, from the server's own
// state, every time: a valid token is necessary and never enough. The caller must
// be the Worker the token was issued to, the chain Run → Task → Project must
// lead to the Project of the URL, and the lease must still be the live, accepted
// one. A write is conceded only for the one branch and the one commit of the
// sealed result of a Run that waits to publish it. Every failure is the same
// CF_POLICY_DENIED. A request already in flight keeps what it was given when it
// began; the next one is authorised again.
func (s *Service) AuthorizeRelay(ctx context.Context, by pki.Session, project string, write bool, token string) (RelayGrant, error) {
	now := s.clock.Now()
	claims, ok := s.verify(token, now)
	if !ok || claims.Project != domain.ID(project) {
		return RelayGrant{}, errRelayDenied()
	}
	if kind, _, err := domain.ParseID(project); err != nil || kind != domain.KindProject {
		return RelayGrant{}, errRelayDenied()
	}
	if claims.Scope == scopeOrphan {
		return s.authorizeOrphan(ctx, by, claims, project, write, now)
	}
	var grant RelayGrant
	err := s.db.Read(ctx, func(q store.Querier) error {
		c, err := resolveChain(ctx, q, by, now, claims.Run, claims.Epoch)
		if err != nil {
			var coded *domain.Error
			if errors.As(err, &coded) && coded.Code == domain.CodeUnauthenticated {
				return err // the identity itself is no longer good: the caller must know
			}
			if errors.As(err, &coded) {
				return errRelayDenied()
			}
			return err
		}
		if c.worker.ID != claims.Worker || c.project != domain.ID(project) {
			return errRelayDenied()
		}
		grant = RelayGrant{Project: c.project, Worker: c.worker.ID}
		if !write {
			return nil
		}
		if !mayPush(c.run) {
			return errRelayDenied()
		}
		grant.Write, grant.Branch, grant.ExpectedOID = true, branchOf(c.task, c.run), c.run.ResultCommit
		return nil
	})
	return grant, err
}

// orphanChain is what an orphan's token stands on, resolved from the database:
// the Worker of the caller, its lease of the Run under that epoch (in any
// state), the orphan the Controller recorded of it and not yet preserved, and
// the Task, not archived.
func orphanChain(ctx context.Context, q store.Querier, by pki.Session, now time.Time, run domain.ID, epoch uint64) (store.Worker, store.Task, error) {
	if _, err := pki.RequireCaller(ctx, q, by, now); err != nil {
		return store.Worker{}, store.Task{}, err
	}
	w, err := store.WorkerByIdentity(ctx, q, by.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Worker{}, store.Task{}, domain.NewError(domain.CodeUnauthenticated, "this identity is not a Worker", nil)
	}
	if err != nil {
		return store.Worker{}, store.Task{}, err
	}
	notFound := domain.NewError(domain.CodeNotFound, "orphan not found", nil)
	l, err := store.LeaseByRunEpoch(ctx, q, run, epoch)
	if errors.Is(err, store.ErrNotFound) || (err == nil && l.WorkerID != w.ID) {
		return store.Worker{}, store.Task{}, notFound
	}
	if err != nil {
		return store.Worker{}, store.Task{}, err
	}
	o, err := store.OrphanOf(ctx, q, w.ID, run, epoch)
	if errors.Is(err, store.ErrNotFound) {
		return store.Worker{}, store.Task{}, notFound
	}
	if err != nil {
		return store.Worker{}, store.Task{}, err
	}
	if o.Preserved != store.OrphanInWorker {
		return store.Worker{}, store.Task{}, domain.NewError(domain.CodeInvalidTransition, "the orphan is preserved already", nil)
	}
	r, err := store.RunByID(ctx, q, run)
	if err != nil {
		return store.Worker{}, store.Task{}, err
	}
	t, err := store.TaskByID(ctx, q, r.TaskID)
	if err != nil {
		return store.Worker{}, store.Task{}, err
	}
	if t.ArchivedAt != nil {
		return store.Worker{}, store.Task{}, notFound
	}
	return w, t, nil
}

// MintOrphanToken gives a Worker a token that may only create the ref of an
// orphan it holds, at the commit it sealed (§10.4 point 5): the Controller must
// have detected the orphan on this Worker under that epoch, and it must not be
// preserved yet. It does not relax the tokens of a lease.
func (s *Service) MintOrphanToken(ctx context.Context, by pki.Session, runID string, req v1.OrphanTokenRequest) (v1.RelayTokenResponse, error) {
	if kind, _, err := domain.ParseID(runID); err != nil || kind != domain.KindRun {
		return v1.RelayTokenResponse{}, domain.NewError(domain.CodeNotFound, "run not found", nil)
	}
	if !domain.ValidCommitID(req.ResultCommit) || req.LeaseEpoch == 0 {
		return v1.RelayTokenResponse{}, domain.NewError(domain.CodeInvalidRequest, "an orphan token needs a lease epoch and a commit ID", nil)
	}
	if s.relay == nil {
		return v1.RelayTokenResponse{}, domain.NewError(domain.CodeInternal, "the Controller has no relay", nil)
	}
	var out v1.RelayTokenResponse
	err := s.db.Read(ctx, func(q store.Querier) error {
		now := s.clock.Now()
		w, t, err := orphanChain(ctx, q, by, now, domain.ID(runID), req.LeaseEpoch)
		if err != nil {
			return err
		}
		expires := now.Add(RelayTokenTTL)
		out = v1.RelayTokenResponse{
			Token: s.sign(relayClaims{
				Worker: w.ID, Run: domain.ID(runID), Epoch: req.LeaseEpoch, Project: t.ProjectID, Expires: expires.UnixNano(),
				Scope: scopeOrphan, Commit: req.ResultCommit,
			}),
			ExpiresAt: expires, Path: v1.RelayPathPrefix + string(t.ProjectID) + ".git", Write: true,
			Ref: domain.OrphanRef(domain.ID(runID), req.LeaseEpoch),
		}
		return nil
	})
	return out, err
}

// authorizeOrphan decides a request made with an orphan's token: only a push,
// by the Worker it was issued to, of the one ref of the orphan, created at the
// one commit, while the orphan is still not preserved.
func (s *Service) authorizeOrphan(ctx context.Context, by pki.Session, claims relayClaims, project string, write bool, now time.Time) (RelayGrant, error) {
	if !write {
		return RelayGrant{}, errRelayDenied()
	}
	var grant RelayGrant
	err := s.db.Read(ctx, func(q store.Querier) error {
		w, t, err := orphanChain(ctx, q, by, now, claims.Run, claims.Epoch)
		if err != nil {
			var coded *domain.Error
			if errors.As(err, &coded) && coded.Code == domain.CodeUnauthenticated {
				return err
			}
			if errors.As(err, &coded) {
				return errRelayDenied()
			}
			return err
		}
		if w.ID != claims.Worker || t.ProjectID != domain.ID(project) {
			return errRelayDenied()
		}
		grant = RelayGrant{
			Project: t.ProjectID, Worker: w.ID, Write: true, Ref: domain.OrphanRef(claims.Run, claims.Epoch),
			ExpectedOID: claims.Commit,
		}
		return nil
	})
	return grant, err
}
