package leases

import (
	"context"
	"unicode"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Accept is the Worker taking the Run it was offered (§10.4.2). The lease
// must still be alive at its epoch and the Run still queued under it: in the
// same transaction the Run goes to preparing, the lease to accepted, and the
// Task re-derives. The reservation stays: only #13's stop releases it.
//
// Accepting an accepted lease answers the same, writing nothing; the answer
// holds only what no later step changes. An expired or rejected lease, or one
// whose accept_by has come, is CF_STALE_LEASE, and the Worker discards the
// assignment. A Worker that is draining may accept a lease it was given before.
func (s *Service) Accept(ctx context.Context, by pki.Session, leaseRef string, req v1.AcceptLeaseRequest) (v1.LeaseResponse, error) {
	return s.answer(ctx, by, "worker.lease.accept", leaseRef, req.Epoch,
		func(tx *store.Tx, act mutation.Act, l store.Lease) (store.Lease, error) {
			switch l.State {
			case domain.LeaseAccepted:
				return l, nil
			case domain.LeasePending:
			default:
				return l, stale("lease "+string(l.ID)+" is "+string(l.State), l)
			}
			if !act.Now.Before(l.AcceptBy) {
				return l, stale("lease "+string(l.ID)+" was not accepted before its accept_by", l)
			}
			run, err := currentRun(ctx, tx, l)
			if err != nil {
				return l, err
			}
			next, err := domain.NextRunState(domain.RunStatus{State: run.State}, domain.RunEventLeaseAccepted)
			if err != nil {
				return l, err
			}
			task, err := store.TaskByID(ctx, tx, run.TaskID)
			if err != nil {
				return l, err
			}
			if err := tx.AdvanceRun(ctx, run, next.State); err != nil {
				return l, err
			}
			if err := tx.AcceptLease(ctx, l.ID, act.Now); err != nil {
				return l, err
			}
			if err := tx.AckAssign(ctx, l.ID, act.Now); err != nil {
				return l, err
			}
			ev := act.Event(domain.EventLeaseAccepted, "lease accepted")
			ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = task.ProjectID, task.ID, run.ID, l.WorkerID
			ev.Data = map[string]any{"lease_id": string(l.ID), "epoch": l.Epoch}
			if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
				return l, err
			}
			if err := s.rederive(ctx, tx, act, task.ID); err != nil {
				return l, err
			}
			return store.LeaseByID(ctx, tx, l.ID)
		})
}

// Reject is the Worker handing back a Run it cannot take (§10.4.2), with the
// code that says why. The first rejection closes the lease: it is rejected,
// its reservation released, the Run returns to the queue with no Worker and
// the Worker's assign order is confirmed, since it must have received it to
// answer. Repeating it answers the same closure and writes nothing, so the
// Worker is not left alone for longer (domain.LeaseRejectBackoff). After
// accept_by, or on an expired lease, it is CF_STALE_LEASE; on an accepted one,
// CF_INVALID_TRANSITION.
func (s *Service) Reject(ctx context.Context, by pki.Session, leaseRef string, req v1.RejectLeaseRequest) (v1.LeaseResponse, error) {
	if !domain.ValidLeaseRejectCode(req.Code) {
		return v1.LeaseResponse{}, input.Invalid("code must be %s, %s or %s",
			domain.CodeWorkerCapacity, domain.CodeHarnessUnavailable, domain.CodeVersionIncompatible)
	}
	if err := message(req.Message); err != nil {
		return v1.LeaseResponse{}, err
	}
	return s.answer(ctx, by, "worker.lease.reject", leaseRef, req.Epoch,
		func(tx *store.Tx, act mutation.Act, l store.Lease) (store.Lease, error) {
			switch l.State {
			case domain.LeaseRejected:
				return l, nil
			case domain.LeaseAccepted:
				return l, domain.NewError(domain.CodeInvalidTransition,
					"lease "+string(l.ID)+" is accepted: a Worker cannot reject it any more", map[string]any{"lease": string(l.ID)})
			case domain.LeasePending:
			default:
				return l, stale("lease "+string(l.ID)+" is "+string(l.State), l)
			}
			if !act.Now.Before(l.AcceptBy) {
				return l, stale("lease "+string(l.ID)+" was not answered before its accept_by", l)
			}
			run, err := currentRun(ctx, tx, l)
			if err != nil {
				return l, err
			}
			return closeIn(ctx, tx, act, l, run, closing{
				to: domain.LeaseRejected, code: string(req.Code), message: req.Message, release: releaseLeaseRejected,
				event: domain.EventLeaseRejected, eventMessage: "lease rejected", answered: true,
			})
		})
}

const maxMessageLen = 512

// message accepts the short printable text a Worker gives with a rejection.
func message(s string) error {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return input.Invalid("message has characters that do not print")
		}
	}
	if len(s) > maxMessageLen {
		return input.Invalid("message is longer than %d characters", maxMessageLen)
	}
	return nil
}

// currentRun is the Run of a lease that is still waiting for its answer. A
// pending lease always points at its Run, queued and under that epoch (the
// invariant of 0103); if it does not, the lease is not the Run's any more.
func currentRun(ctx context.Context, tx *store.Tx, l store.Lease) (store.Run, error) {
	run, err := store.RunByID(ctx, tx, l.RunID)
	if err != nil {
		return store.Run{}, err
	}
	if run.State != domain.RunQueued || run.LeaseID != l.ID || run.LeaseEpoch != l.Epoch {
		return store.Run{}, stale("lease "+string(l.ID)+" is no longer the lease of its Run", l)
	}
	return run, nil
}
