package executor

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Sources of a stop.
const (
	SourceController = "controller"
	SourceWorker     = "worker"
)

// Stop ends a Run (§10.7). The order is written first, in the same transaction as
// what it does to the phase; then it is carried out:
//
//   - not accepted yet: the assignment is aborted, and will not be answered;
//   - accepted and not started: it is aborted, Start refuses it, and the
//     Controller is told RunStopped(never_started);
//   - started: the wrapper is asked to end the group, waits stop.grace and kills
//     it, and the supervisor reports the exit and RunStopped when it has verified
//     that nothing is left.
//
// It takes the lock of the Run, so a start in progress finishes first.
func (e *Executor) Stop(ctx context.Context, runID string, leaseEpoch uint64, reason, source string) error {
	k := state.Key{RunID: runID, LeaseEpoch: leaseEpoch}
	unlock := e.lock(k)
	defer unlock()
	r, err := state.RunBy(ctx, e.o.DB.Reader(), k)
	if errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("executor: there is no assignment of run %s under epoch %d", runID, leaseEpoch)
	}
	if err != nil {
		return err
	}
	if !r.Phase.Live() {
		return nil
	}
	now := e.clock.Now()
	order := state.StopOrder{Key: k, Reason: reason, Source: source, CreatedAt: now}
	switch r.Phase {
	case state.PhaseAssigned, state.PhaseAccepted:
		err = e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
			if _, err := state.AddStopOrder(ctx, tx, order); err != nil {
				return err
			}
			if err := state.Close(ctx, tx, k, []state.Phase{r.Phase}, state.PhaseAborted, true, now); err != nil {
				return err
			}
			if r.Phase == state.PhaseAccepted { // the Controller holds our acceptance: it is told
				if err := e.report(ctx, tx, k, v1.ReportStopped, now, func(en *v1.RunReportEntry) { en.NeverStarted = true }); err != nil {
					return err
				}
			}
			return state.CompleteStopOrder(ctx, tx, k, now)
		})
		if err == nil {
			e.o.Journal.Notify()
		}
		return err
	}
	err = e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if _, err := state.AddStopOrder(ctx, tx, order); err != nil {
			return err
		}
		if r.Phase == state.PhaseStarting || r.Phase == state.PhaseRunning {
			return state.Advance(ctx, tx, k, r.Phase, state.PhaseStopping, now)
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.Phase = state.PhaseStopping
	e.mu.Lock()
	e.signaled[k] = true
	e.mu.Unlock()
	m, err := e.markers(k)
	if err != nil {
		return err
	}
	e.signalStop(ctx, r, m)
	if e.spawn != nil {
		e.spawn(k)
	}
	return nil
}

// signalStop asks the Run to end. With a wrapper, it asks the wrapper, after
// checking that the process with its pid is the wrapper. Without one, the
// session is removed: nothing had started in it that the wrapper could stop.
func (e *Executor) signalStop(ctx context.Context, r state.Run, m markers) {
	if m.exit != nil {
		return
	}
	if m.wrapper == nil {
		_ = e.o.Runtime.Stop(ctx, e.o.Runtime.HandleFor(e.sessionName(r.Key)))
		return
	}
	switch procident.Verify(m.wrapper.PID, m.wrapper.Start) {
	case procident.Same:
		if err := syscall.Kill(m.wrapper.PID, syscall.SIGTERM); err != nil {
			e.log.WarnContext(ctx, "signalling the wrapper failed", "run", r.RunID, "err", err)
		}
	case procident.Gone:
		// Nothing to ask: the supervisor finds the wrapper gone and ends the group.
	default:
		e.markUnverified(ctx, r, "the wrapper of the Run is not the process it was")
	}
}
