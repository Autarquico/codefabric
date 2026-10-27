package runs

import (
	"context"
	"errors"
	"slices"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// maxWorkerEpochLen bounds the epoch a grant names: a ULID, with room to spare.
const maxWorkerEpochLen = 64

// GrantSecrets decides whether the Worker may resolve refs for a Run it holds
// (§16.7, the one point that decides). It is decided anew on every call, from
// the state of that instant, with no idempotency key: a retry after a lost
// answer is authorised again, and a ref denied since is denied to it. A grant
// already given is never revoked by what happens later.
//
// The caller must be a Worker with a good identity, as the daemon the
// Controller holds as current; the lease must be its own, accepted and the
// live one of the Run; and the Run must not be terminal. Each ref must be one
// the frozen spec of the Run declares, in the allowlist of its Project now, and
// held by the Worker for that Project.
//
// A grant is all or nothing, and every decision is kept with one audit entry
// per ref, denials included: the trail commits first, then a denial is the
// error (CF_POLICY_DENIED, CF_STALE_LEASE or CF_UNAUTHENTICATED) with
// details.denied, the []v1.SecretDenial of the refs denied.
func (s *Service) GrantSecrets(ctx context.Context, by pki.Session, runID string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
	if err := validateGrant(runID, req); err != nil {
		return v1.GrantSecretsResponse{}, err
	}
	var decided grantDecision
	err := s.db.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		if decided, err = decideGrant(ctx, tx, by, s.clock.Now(), domain.ID(runID), req); err != nil {
			return err
		}
		_, err = tx.RecordSecretGrant(ctx, decided.grant)
		return err
	})
	if err != nil {
		return v1.GrantSecretsResponse{}, err
	}
	return decided.answer()
}

func validateGrant(runID string, req v1.GrantSecretsRequest) error {
	if kind, _, err := domain.ParseID(runID); err != nil || kind != domain.KindRun {
		return domain.NewError(domain.CodeNotFound, "run not found", nil)
	}
	if kind, _, err := domain.ParseID(req.LeaseID); err != nil || kind != domain.KindLease {
		return input.Invalid("lease_id must be the ID of a lease")
	}
	if req.LeaseEpoch == 0 {
		return input.Invalid("lease_epoch must be at least 1")
	}
	if req.WorkerEpoch == "" || len(req.WorkerEpoch) > maxWorkerEpochLen {
		return input.Invalid("worker_epoch must be 1 to %d characters", maxWorkerEpochLen)
	}
	if len(req.Refs) == 0 {
		return input.Invalid("refs cannot be empty")
	}
	if err := domain.CheckSecretRefs(req.Refs, domain.MaxRunSecrets); err != nil {
		return input.Invalid("%v", err)
	}
	return nil
}

// grantDecision is a grant as it is kept, and the error a denial answers with.
type grantDecision struct {
	grant  store.SecretGrant
	denial domain.Code
}

func (d grantDecision) answer() (v1.GrantSecretsResponse, error) {
	if d.denial == "" {
		granted := make([]string, len(d.grant.Refs))
		for i, v := range d.grant.Refs {
			granted[i] = v.Ref
		}
		return v1.GrantSecretsResponse{Granted: granted}, nil
	}
	var denied []v1.SecretDenial
	for _, v := range d.grant.Refs {
		if v.Denial != "" {
			denied = append(denied, v1.SecretDenial{Ref: v.Ref, Reason: v.Denial})
		}
	}
	return v1.GrantSecretsResponse{}, domain.NewError(d.denial, "the secrets of this Run are not granted",
		map[string]any{"denied": denied})
}

// decideGrant finds what the request stands on and decides each ref. Only a
// request that names nothing the Controller can keep a grant against (a caller
// that is no Worker, a lease that is not this Run's under that epoch) is an
// error without a trail.
func decideGrant(ctx context.Context, tx *store.Tx, by pki.Session, now time.Time, run domain.ID, req v1.GrantSecretsRequest) (grantDecision, error) {
	w, err := store.WorkerByIdentity(ctx, tx, by.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return grantDecision{}, domain.NewError(domain.CodeUnauthenticated, "this identity is not a Worker", nil)
	}
	if err != nil {
		return grantDecision{}, err
	}
	l, err := store.LeaseByRunEpoch(ctx, tx, run, req.LeaseEpoch)
	if errors.Is(err, store.ErrNotFound) || (err == nil && l.ID != domain.ID(req.LeaseID)) {
		return grantDecision{}, domain.NewError(domain.CodeNotFound, "lease not found", nil)
	}
	if err != nil {
		return grantDecision{}, err
	}
	r, err := store.RunByID(ctx, tx, run)
	if err != nil {
		return grantDecision{}, err
	}
	t, err := store.TaskByID(ctx, tx, r.TaskID)
	if err != nil {
		return grantDecision{}, err
	}
	d := grantDecision{grant: store.SecretGrant{
		RunID: r.ID, LeaseID: l.ID, LeaseEpoch: l.Epoch, WorkerID: w.ID, WorkerEpoch: req.WorkerEpoch, DecidedAt: now,
	}}
	reason, code, err := authorizeGrant(ctx, tx, by, now, w, l, r, t, req.WorkerEpoch)
	if err != nil {
		return grantDecision{}, err
	}
	deny := func(string) string { return reason }
	if reason == "" {
		if deny, err = refPolicy(ctx, tx, w, r, t.ProjectID); err != nil {
			return grantDecision{}, err
		}
		code = domain.CodePolicyDenied
	}
	for _, ref := range sortedRefs(req.Refs) {
		d.grant.Refs = append(d.grant.Refs, store.SecretVerdict{Ref: ref, Denial: deny(ref)})
	}
	if !d.grant.Granted() {
		d.denial = code
	}
	return d, nil
}

// authorizeGrant decides whether the caller may prepare the Run at all: the
// reason it may not, "" if it may, with the code that answers it.
func authorizeGrant(ctx context.Context, q store.Querier, by pki.Session, now time.Time,
	w store.Worker, l store.Lease, r store.Run, t store.Task, workerEpoch string,
) (string, domain.Code, error) {
	if _, err := pki.RequireCaller(ctx, q, by, now); err != nil {
		var coded *domain.Error
		if errors.As(err, &coded) && coded.Code == domain.CodeUnauthenticated {
			return v1.GrantUnauthenticated, domain.CodeUnauthenticated, nil
		}
		return "", "", err
	}
	if l.WorkerID != w.ID {
		return v1.GrantNotLeaseHolder, domain.CodeStaleLease, nil
	}
	if l.State != domain.LeaseAccepted || r.LeaseEpoch != l.Epoch {
		return v1.GrantStaleLease, domain.CodeStaleLease, nil
	}
	if r.State.IsTerminal() || t.ArchivedAt != nil {
		return v1.GrantRunNotPreparable, domain.CodeStaleLease, nil
	}
	current, err := currentDaemon(ctx, q, w, workerEpoch)
	if err != nil || !current {
		return v1.GrantDaemonRetired, domain.CodeStaleLease, err
	}
	return "", "", nil
}

// currentDaemon says whether epoch is that of the daemon the Controller holds
// for the Worker: the one of its latest heartbeat taken and, once a daemon with
// a generation has reported, the one that generation is bound to. The fence of
// the heartbeat (T7) keeps a retired daemon from moving either.
func currentDaemon(ctx context.Context, q store.Querier, w store.Worker, epoch string) (bool, error) {
	if epoch != w.Report.Epoch {
		return false, nil
	}
	fence, fenced, err := store.WorkerSecretsFence(ctx, q, w.ID)
	if err != nil {
		return false, err
	}
	return !fenced || fence.Epoch == epoch, nil
}

// refPolicy is the reason each ref is denied ("" if it passes) for a caller
// that may prepare the Run: the frozen spec declares it, the allowlist of the
// Project holds it now, and the Worker holds it for that Project.
func refPolicy(ctx context.Context, q store.Querier, w store.Worker, r store.Run, project domain.ID) (func(string) string, error) {
	placement, err := PlacementOf(r.Spec)
	if err != nil {
		return nil, err
	}
	allowed, err := store.ProjectAllowlist(ctx, q, project)
	if err != nil {
		return nil, err
	}
	held, err := store.SecretsOfWorker(ctx, q, w.ID)
	if err != nil {
		return nil, err
	}
	scopes := make(map[string][]string, len(held))
	for _, m := range held {
		scopes[m.Ref] = m.Projects
	}
	return func(ref string) string {
		scope, holds := scopes[ref]
		switch {
		case !slices.Contains(placement.Secrets, ref):
			return v1.SecretNotDeclared
		case !slices.Contains(allowed, ref):
			return v1.SecretNotAllowlisted
		case !holds:
			return v1.SecretNotAnnounced
		case len(scope) > 0 && !slices.Contains(scope, string(project)):
			return v1.SecretOutOfScope
		}
		return ""
	}, nil
}

func sortedRefs(refs []string) []string {
	out := slices.Clone(refs)
	slices.Sort(out)
	return out
}
