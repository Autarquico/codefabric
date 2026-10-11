package leases

import (
	"context"
	"errors"
	"fmt"

	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// closing says how a pending lease ends without being accepted. answered is
// whether the Worker has the assign order: a Worker that rejects has received
// it and persisted it, so its order is confirmed; the others withdraw it,
// which a Worker that already has it learns from the lease being stale.
type closing struct {
	to           domain.LeaseState
	code         string
	message      string
	release      string
	event        domain.EventType
	eventMessage string
	answered     bool
}

// closeIn ends the pending lease l of the queued Run in tx: the lease is
// closed, its reservation released, its assign order settled, the Run taken
// back from the Worker (its epoch stays) and the event written. It returns the
// lease as it is afterwards.
func closeIn(ctx context.Context, tx *store.Tx, act mutation.Act, l store.Lease, run store.Run, c closing) (store.Lease, error) {
	if err := tx.CloseLease(ctx, l.ID, c.to, c.code, c.message, act.Now); err != nil {
		return l, err
	}
	if err := tx.ReleaseReservation(ctx, l.ID, c.release, act.Now); err != nil {
		return l, err
	}
	settle := tx.CancelAssign
	if c.answered {
		settle = tx.AckAssign
	}
	if err := settle(ctx, l.ID, act.Now); err != nil {
		return l, err
	}
	if err := tx.ClearRunLease(ctx, run); err != nil {
		return l, err
	}
	task, err := store.TaskByID(ctx, tx, run.TaskID)
	if err != nil {
		return l, err
	}
	ev := act.Event(c.event, c.eventMessage)
	ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = task.ProjectID, task.ID, run.ID, l.WorkerID
	ev.Data = map[string]any{"lease_id": string(l.ID), "epoch": l.Epoch, "close_code": c.code}
	if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
		return l, err
	}
	return store.LeaseByID(ctx, tx, l.ID)
}

// SweepExpired expires the leases nobody answered by their accept_by (§10.4.4)
// and returns how many it expired. A lease that fails to expire does not stop
// the others: the error that comes back joins the ones of each lease. Each one is its own transaction, which
// reads the lease and the clock again: a lease accepted or already expired
// meanwhile is left alone, and one whose accept_by has not come (the boundary
// is exclusive: it is alive until the instant itself) is too. The Run is
// queued again, so the scheduler may reassign it with epoch + 1.
func (s *Service) SweepExpired(ctx context.Context) (int, error) {
	var due []store.Lease
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		due, err = store.PendingLeasesDue(ctx, q, s.clock.Now())
		return err
	})
	if err != nil {
		return 0, err
	}
	var (
		expired int
		failed  []error
	)
	for _, l := range due {
		var done bool
		err := s.db.WithTx(ctx, func(tx *store.Tx) error {
			done = false
			now := s.clock.Now()
			current, err := store.LeaseByID(ctx, tx, l.ID)
			if err != nil {
				return err
			}
			if current.State != domain.LeasePending || now.Before(current.AcceptBy) {
				return nil
			}
			run, err := store.RunByID(ctx, tx, current.RunID)
			if err != nil {
				return err
			}
			act := mutation.Act{Actor: systemActor, Now: now}
			_, err = closeIn(ctx, tx, act, current, run, closing{
				to: domain.LeaseExpired, code: domain.LeaseCloseAcceptTimeout, release: releaseLeaseExpired,
				event: domain.EventLeaseExpired, eventMessage: "lease expired",
			})
			done = err == nil
			return err
		})
		switch {
		case err != nil:
			failed = append(failed, fmt.Errorf("leases: expiring %s of run %s: %w", l.ID, l.RunID, err))
		case done:
			expired++
		}
	}
	return expired, errors.Join(failed...)
}

// InvalidateIn ends the lease of a Run that is about to be cancelled while it
// still waits for its Worker's answer (§10.5): the lease expires with
// run_ended, its reservation is released and its order withdrawn, in the
// transaction of the cancellation. It updates run to say that it no longer has
// a Worker or a lease, which is what FinishRun then expects. A Run without a
// pending lease is left as it is: with an accepted one, the lease and the
// reservation stay until the Worker has stopped it.
func InvalidateIn(ctx context.Context, tx *store.Tx, act mutation.Act, run *store.Run) error {
	if run.State != domain.RunQueued || run.LeaseState != domain.LeasePending {
		return nil
	}
	l, err := store.LeaseByID(ctx, tx, run.LeaseID)
	if err != nil {
		return err
	}
	if _, err := closeIn(ctx, tx, act, l, *run, closing{
		to: domain.LeaseExpired, code: domain.LeaseCloseRunEnded, release: releaseRunEnded,
		event: domain.EventLeaseExpired, eventMessage: "lease expired: its Run ended",
	}); err != nil {
		return err
	}
	run.WorkerID, run.LeaseID, run.LeaseState = "", "", ""
	return nil
}
