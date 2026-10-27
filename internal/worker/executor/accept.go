package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// decideLoop answers the assignments one at a time, in order, so that two
// decisions never count the same free capacity twice.
func (e *Executor) decideLoop(ctx context.Context) {
	ticker := e.clock.NewTicker(tick)
	defer ticker.Stop()
	var backoff time.Duration
	for ctx.Err() == nil {
		if e.decideOnce(ctx) {
			backoff = nextBackoff(backoff)
			e.pause(ctx, backoff)
			continue
		}
		backoff = 0
		select {
		case <-ctx.Done():
		case <-e.wake:
		case <-ticker.C():
		}
	}
}

// decideOnce answers every assignment waiting for it, until one cannot be
// answered because the Controller cannot be reached: retry says so, and it
// waits its turn again.
func (e *Executor) decideOnce(ctx context.Context) (retry bool) {
	if _, ok := e.currentLimits(); !ok {
		return false // the Worker does not know its capacity yet: nothing is decided
	}
	if !e.Reconciled() {
		return false // until the reconciliation completes no lease is answered (§27.3)
	}
	waiting, err := state.AssignedRuns(ctx, e.o.DB.Reader())
	if err != nil {
		if ctx.Err() == nil {
			e.log.ErrorContext(ctx, "reading the assignments", "err", err)
		}
		return true
	}
	for _, r := range waiting {
		if ctx.Err() != nil {
			return false
		}
		if e.decide(ctx, r.Key) {
			return true
		}
	}
	return false
}

// verdict is the local answer to an assignment: take it, or hand it back with
// the code and the reason.
type verdict struct {
	reject  domain.Code
	message string
}

// decide answers one assignment, under the lock of its Run. It returns true if
// the Controller could not be reached, in which case the assignment stays as it
// is and the answer is tried again.
func (e *Executor) decide(ctx context.Context, k state.Key) (retry bool) {
	defer e.lock(k)()
	if !e.Reconciled() {
		return false // a reconciliation was asked for meanwhile: the lease waits for it
	}
	r, err := state.RunBy(ctx, e.o.DB.Reader(), k)
	if err != nil || r.Phase != state.PhaseAssigned {
		return false // a stop got here first, or it was answered
	}
	lim, _ := e.currentLimits()
	load, err := state.Occupied(ctx, e.o.DB.Reader())
	if err != nil {
		e.log.ErrorContext(ctx, "reading the load", "err", err)
		return true
	}
	if v := e.evaluate(ctx, r, lim, load); v.reject != "" {
		err := e.reject(ctx, r, v)
		var coded *domain.Error
		if errors.As(err, &coded) && coded.Code == domain.CodeInvalidTransition {
			// The lease is accepted already: an earlier accept got to the Controller and
			// the daemon died before it wrote it down, and what it sees now is different.
			// Accepting again is idempotent and says whether it is still ours. If it is, the
			// Controller holds a Run this Worker has just found it cannot take (no room, no
			// such harness): it is not started, so it cannot go over the capacity. It is
			// failed and stopped as one that never ran, which releases its reservation.
			return e.abandon(ctx, r, v, e.accept(ctx, r))
		}
		return e.answer(ctx, r, err, state.PhaseRejected)
	}
	return e.answer(ctx, r, e.accept(ctx, r), state.PhaseAccepted)
}

// abandon ends an assignment that the Controller holds as accepted and that the
// Worker will not run. acceptErr is the answer to the accept that confirmed it.
func (e *Executor) abandon(ctx context.Context, r state.Run, v verdict, acceptErr error) (retry bool) {
	if acceptErr != nil {
		return e.answer(ctx, r, acceptErr, state.PhaseAccepted) // stale, unknown or unreachable: as ever
	}
	now := e.clock.Now()
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if err := e.report(ctx, tx, r.Key, v1.ReportPrepareFailed, now, func(en *v1.RunReportEntry) { en.Message = v.message }); err != nil {
			return err
		}
		if err := e.report(ctx, tx, r.Key, v1.ReportStopped, now, func(en *v1.RunReportEntry) { en.NeverStarted = true }); err != nil {
			return err
		}
		return state.Close(ctx, tx, r.Key, []state.Phase{state.PhaseAssigned}, state.PhaseStopped, true, now)
	})
	if err != nil {
		e.log.ErrorContext(ctx, "abandoning an assignment", "run", r.RunID, "err", err)
		return true
	}
	e.o.Journal.Notify()
	e.forget(r.Key)
	return false
}

func (e *Executor) accept(ctx context.Context, r state.Run) error {
	_, err := e.o.Controller.AcceptLease(ctx, r.LeaseID, r.LeaseEpoch)
	return err
}

func (e *Executor) reject(ctx context.Context, r state.Run, v verdict) error {
	_, err := e.o.Controller.RejectLease(ctx, r.LeaseID, v1.RejectLeaseRequest{Epoch: r.LeaseEpoch, Code: v.reject, Message: reportable(v.message)})
	return err
}

// answer records what the Controller said to our accept or reject. A lease that
// is no longer ours (stale, unknown, or past the point where it can change) is
// discarded without starting anything; one that could not be asked about waits.
func (e *Executor) answer(ctx context.Context, r state.Run, err error, done state.Phase) (retry bool) {
	to := done
	var coded *domain.Error
	switch {
	case err == nil:
	case errors.As(err, &coded) && slices.Contains([]domain.Code{domain.CodeStaleLease, domain.CodeNotFound, domain.CodeInvalidTransition}, coded.Code):
		e.log.InfoContext(ctx, "the lease is no longer ours: the assignment is discarded", "run", r.RunID, "epoch", r.LeaseEpoch, "code", coded.Code)
		to = state.PhaseDiscarded
	default:
		if ctx.Err() == nil {
			e.log.WarnContext(ctx, "answering an assignment failed; it will be asked again", "run", r.RunID, "err", err)
		}
		return true
	}
	now := e.clock.Now()
	txErr := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if to.Live() {
			return state.Advance(ctx, tx, r.Key, state.PhaseAssigned, to, now)
		}
		return state.Close(ctx, tx, r.Key, []state.Phase{state.PhaseAssigned}, to, true, now)
	})
	if txErr != nil {
		e.log.ErrorContext(ctx, "recording the answer to an assignment", "run", r.RunID, "err", txErr)
		return true
	}
	if to.Live() {
		if e.spawn != nil {
			e.spawn(r.Key)
		}
	} else {
		e.forget(r.Key)
	}
	return false
}

// evaluate decides whether this Worker can take the assignment (§10.4 point 2,
// §10.5). The Controller does not know what the Worker has meanwhile, so the
// Worker has the last word.
func (e *Executor) evaluate(ctx context.Context, r state.Run, lim Limits, load state.Load) verdict {
	spec := parseSpec(r.Spec)
	if !domain.ReadableRunSpecSchema(spec.Schema) {
		return verdict{domain.CodeVersionIncompatible, fmt.Sprintf("the spec has schema %d: this Worker reads schemas %d and %d", spec.Schema, domain.RunSpecSchema, domain.RunSpecSchemaSecrets)}
	}
	adapter, ok := e.harnesses.Get(spec.Requirements.Harness)
	switch {
	case spec.Requirements.Harness == "":
		return verdict{domain.CodeHarnessUnavailable, "the Run names no harness: this Worker runs only `command`, and only when it is asked for"}
	case !ok || e.withheld[spec.Requirements.Harness]:
		return verdict{domain.CodeHarnessUnavailable, fmt.Sprintf("this Worker has no harness %q", spec.Requirements.Harness)}
	case !adapter.Capabilities().Supports(domain.Mode(spec.Mode)):
		return verdict{domain.CodeHarnessUnavailable, fmt.Sprintf("the harness %q does not support the mode %q", spec.Requirements.Harness, spec.Mode)}
	}
	switch {
	case load.Runs+1 > lim.MaxConcurrentRuns:
		return verdict{domain.CodeWorkerCapacity, fmt.Sprintf("%d Runs are already taking the %d this Worker may run at once", load.Runs, lim.MaxConcurrentRuns)}
	case load.CPU+r.CPU > lim.CPU || load.MemoryMB+r.MemoryMB > lim.MemoryMB:
		return verdict{domain.CodeWorkerCapacity, "the Run does not fit in what this Worker has left"}
	}
	return e.secretsVerdict(ctx, r, spec)
}
