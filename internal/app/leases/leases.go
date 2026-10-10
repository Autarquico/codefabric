// Package leases is the use cases of leases and the orders that carry them
// (§10.4): creating one with its reservation, the Worker's accept and reject,
// the expiry of the ones nobody answers, and the queue a Worker reads its
// assignments from.
package leases

import (
	"context"
	"errors"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Rederive brings the materialised state of a Task up to date inside the
// transaction of whatever changed it (tasks.Rederive). It is injected because
// the tasks package imports the packages that import this one.
type Rederive func(ctx context.Context, tx *store.Tx, act mutation.Act, task domain.ID) error

// Service runs the lease use cases.
type Service struct {
	db       *store.DB
	clock    clock.Clock
	ids      *domain.IDGenerator
	rederive Rederive
}

// New builds the service. The clock is the one the store was opened with.
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, rederive Rederive) *Service {
	return &Service{db: db, clock: clk, ids: ids, rederive: rederive}
}

var systemActor = domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}

// Reasons a reservation is released (§10.5).
const (
	releaseLeaseExpired  = "lease_expired"
	releaseLeaseRejected = "lease_rejected"
	releaseRunEnded      = "run_ended"
)

// View is the lease as the API shows it.
func View(l store.Lease) v1.Lease {
	return v1.Lease{
		ID: string(l.ID), RunID: string(l.RunID), WorkerID: string(l.WorkerID), Epoch: l.Epoch, State: l.State,
		AcceptBy: l.AcceptBy, AcceptedAt: l.AcceptedAt, ClosedAt: l.ClosedAt, CloseCode: l.CloseCode, CloseMessage: l.CloseMessage,
	}
}

func stale(what string, l store.Lease) error {
	return domain.NewError(domain.CodeStaleLease, what,
		map[string]any{"lease": string(l.ID), "state": string(l.State), "epoch": l.Epoch})
}

// own finds the lease a Worker names, which must be its own: another Worker's
// lease and a lease that does not exist are the same answer, so a Worker
// learns nothing about the leases of others.
func own(ctx context.Context, q store.Querier, w store.Worker, ref string) (store.Lease, error) {
	notFound := domain.NewError(domain.CodeNotFound, "lease not found", nil)
	if kind, _, err := domain.ParseID(ref); err != nil || kind != domain.KindLease {
		return store.Lease{}, notFound
	}
	l, err := store.LeaseByID(ctx, q, domain.ID(ref))
	if errors.Is(err, store.ErrNotFound) || (err == nil && l.WorkerID != w.ID) {
		return store.Lease{}, notFound
	}
	return l, err
}

// workerOf is the Worker that the caller's identity stands for.
func workerOf(ctx context.Context, q store.Querier, by pki.Session) (store.Worker, error) {
	w, err := store.WorkerByIdentity(ctx, q, by.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Worker{}, domain.NewError(domain.CodeNotFound, "this identity is not a Worker", nil)
	}
	return w, err
}

// answer is what accept and reject have in common, in the order that keeps the
// two from disagreeing: the Worker comes from the caller's identity, the lease
// must be the Worker's own, and an epoch other than the lease's is stale
// whatever state the lease is in, repeated requests included. Only then does
// apply look at the state. It returns the lease as it is afterwards.
func (s *Service) answer(ctx context.Context, by pki.Session, operation, leaseRef string, epoch uint64,
	apply func(*store.Tx, mutation.Act, store.Lease) (store.Lease, error),
) (v1.LeaseResponse, error) {
	if epoch == 0 {
		return v1.LeaseResponse{}, domain.NewError(domain.CodeInvalidRequest, "epoch is required", nil)
	}
	request := struct {
		Lease string `json:"lease"`
		Epoch uint64 `json:"epoch"`
	}{leaseRef, epoch}
	return mutation.Do(ctx, s.db, s.clock, by, operation, "", request,
		func(tx *store.Tx, act mutation.Act) (v1.LeaseResponse, error) {
			w, err := workerOf(ctx, tx, by)
			if err != nil {
				return v1.LeaseResponse{}, err
			}
			l, err := own(ctx, tx, w, leaseRef)
			if err != nil {
				return v1.LeaseResponse{}, err
			}
			if epoch != l.Epoch {
				return v1.LeaseResponse{}, stale(fmt.Sprintf("lease %s was issued under epoch %d, not %d", l.ID, l.Epoch, epoch), l)
			}
			l, err = apply(tx, act, l)
			return v1.LeaseResponse{Lease: View(l)}, err
		})
}
