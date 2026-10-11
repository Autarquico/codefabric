package executor

import (
	"context"
	"syscall"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// facts is what finalizing a Run knows about how it ended.
type facts struct {
	at      time.Time
	exit    *execwrap.Exit    // nil if the wrapper left none
	started *execwrap.Started // nil if the harness never spawned
	lost    bool              // the wrapper died and left no exit
	message string
}

// settleExit is the Run having an exit.json. The wrapper wrote it after it
// checked that nothing was left of the group; the daemon, which must not take a
// stop for granted, checks that the wrapper is gone and the group is empty.
func (e *Executor) settleExit(ctx context.Context, r state.Run, m markers) (done bool) {
	if w := m.wrapper; w != nil {
		switch procident.Verify(w.PID, w.Start) {
		case procident.Same:
			if e.clock.Now().Sub(m.exit.ExitedAt) < wrapperExitWait {
				return false // it is ending
			}
			e.log.WarnContext(ctx, "the wrapper wrote its exit and did not end: killing it", "run", r.RunID)
			_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
			return false
		case procident.Gone:
		default:
			return e.markUnverified(ctx, r, "the wrapper of the Run is not the process it was")
		}
		if !e.groupEmpty(ctx, w) {
			// The wrapper is gone, so a group with this ID is what it left behind.
			e.log.WarnContext(ctx, "processes of a Run survive its wrapper: killing them", "run", r.RunID)
			_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
			return false
		}
	}
	e.endSession(ctx, r.Key)
	return e.finalize(ctx, r, facts{at: m.exit.ExitedAt, exit: m.exit, started: m.started})
}

// settleLost is the wrapper having died with no exit. The group it led is ended
// by pid-verified signals, and the Run fails with the cause that was seen.
func (e *Executor) settleLost(ctx context.Context, r state.Run, m markers) (done bool) {
	if exit, err := readMarker[execwrap.Exit](e.runDir(r.Key), execwrap.ExitFile); err == nil && exit != nil {
		m.exit = exit // it wrote its exit and then died: that is not a loss
		return e.settleExit(ctx, r, m)
	}
	w := m.wrapper
	e.log.WarnContext(ctx, "the wrapper of a Run died without an exit", "run", r.RunID)
	_ = syscall.Kill(-w.PGID, syscall.SIGKILL) // the wrapper is gone, so the group is what it left
	if !e.groupEmpty(ctx, w) {
		return false
	}
	e.endSession(ctx, r.Key)
	return e.finalize(ctx, r, facts{at: e.clock.Now(), started: m.started, lost: true, message: "the wrapper died without leaving an exit"})
}

// settleNeverRan ends a Run whose harness never ran. The caller holds the lock.
func (e *Executor) settleNeverRan(ctx context.Context, k state.Key, message string) (done bool) {
	r, err := state.RunBy(ctx, e.o.DB.Reader(), k)
	if err != nil || !r.Phase.Live() {
		return err == nil
	}
	e.endSession(ctx, k)
	return e.finalize(ctx, r, facts{at: e.clock.Now(), message: message})
}

// groupEmpty says whether nothing is left of the group of a wrapper that is gone.
func (e *Executor) groupEmpty(ctx context.Context, w *execwrap.Wrapper) bool {
	empty, err := procident.GroupEmpty(w.PGID)
	if err != nil {
		e.log.WarnContext(ctx, "cannot tell if a process group is empty", "pgid", w.PGID, "err", err)
		return false
	}
	return empty
}

// endSession removes the session of the Runtime if it is still there.
func (e *Executor) endSession(ctx context.Context, k state.Key) {
	h := e.o.Runtime.HandleFor(e.sessionName(k))
	if st, err := e.o.Runtime.Status(ctx, h); err == nil && st.Alive {
		_ = e.o.Runtime.Stop(ctx, h)
	}
}

// escalateStop is a stop whose wrapper did not finish in time: its group is
// killed whole.
func (e *Executor) escalateStop(ctx context.Context, r state.Run, m markers) (done bool) {
	order, err := state.StopOrderOf(ctx, e.o.DB.Reader(), r.Key)
	if err != nil || e.clock.Now().Before(order.CreatedAt.Add(e.grace+stopMargin)) {
		return false
	}
	e.log.WarnContext(ctx, "a stop did not finish within stop.grace: killing the group", "run", r.RunID)
	if procident.Verify(m.wrapper.PID, m.wrapper.Start) == procident.Same {
		_ = syscall.Kill(-m.wrapper.PGID, syscall.SIGKILL)
	}
	_ = e.o.Runtime.Stop(ctx, e.o.Runtime.HandleFor(e.sessionName(r.Key)))
	return false
}

// markUnverified keeps a Run whose processes cannot be proven gone out of the
// stopped state: nothing is signalled and RunStopped is not reported, and the
// verification is tried again every minute and at every start of the daemon.
func (e *Executor) markUnverified(ctx context.Context, r state.Run, why string) (done bool) {
	e.mu.Lock()
	e.retryAt[r.Key] = e.clock.Now().Add(unverifiedRetry)
	e.mu.Unlock()
	if r.Phase == state.PhaseStopUnverified {
		return false
	}
	e.log.ErrorContext(ctx, "the processes of a Run cannot be verified: nothing is signalled and the stop is not confirmed", "run", r.RunID, "why", why)
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		return state.Advance(ctx, tx, r.Key, r.Phase, state.PhaseStopUnverified, e.clock.Now())
	})
	if err != nil {
		e.log.ErrorContext(ctx, "recording an unverified stop", "run", r.RunID, "err", err)
	}
	return false
}

// finalize records how a Run ended and what the Controller has to be told, in
// one transaction: its reports, the final phase and the stop order that is done.
// A harness that started ends with exited and stopped; one that never ran, with
// prepare_failed and stopped(never_started), unless a stop was ordered, in which
// case the Controller already knows that the Run is over and only the stop is
// left to confirm.
func (e *Executor) finalize(ctx context.Context, r state.Run, f facts) (done bool) {
	_, orderErr := state.StopOrderOf(ctx, e.o.DB.Reader(), r.Key)
	ordered := orderErr == nil
	ran := r.StartedAt != nil || f.started != nil
	now := e.clock.Now()
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if f.started != nil && r.StartedAt == nil {
			if err := state.SetStarted(ctx, tx, r.Key, mustJSON(*e.runtimeHandle(r.Key)), f.started.StartedAt); err != nil {
				return err
			}
			if err := e.report(ctx, tx, r.Key, v1.ReportStarted, f.started.StartedAt, func(en *v1.RunReportEntry) {
				en.Handle = e.runtimeHandle(r.Key)
			}); err != nil {
				return err
			}
		}
		if ran {
			if err := e.report(ctx, tx, r.Key, v1.ReportExited, f.at, func(en *v1.RunReportEntry) { fillExit(en, f) }); err != nil {
				return err
			}
		} else if !ordered {
			message := f.message
			if f.exit != nil && f.exit.Error != "" {
				message = f.exit.Error
			}
			if err := e.report(ctx, tx, r.Key, v1.ReportPrepareFailed, f.at, func(en *v1.RunReportEntry) { en.Message = message }); err != nil {
				return err
			}
		}
		if err := e.report(ctx, tx, r.Key, v1.ReportStopped, f.at, func(en *v1.RunReportEntry) { en.NeverStarted = !ran }); err != nil {
			return err
		}
		from := []state.Phase{state.PhaseAccepted, state.PhaseStarting, state.PhaseRunning, state.PhaseStopping, state.PhaseStopUnverified}
		if err := state.Close(ctx, tx, r.Key, from, state.PhaseStopped, !ran, now); err != nil {
			return err
		}
		return state.CompleteStopOrder(ctx, tx, r.Key, now)
	})
	if err != nil {
		e.log.ErrorContext(ctx, "finalizing a Run", "run", r.RunID, "err", err)
		return false
	}
	e.o.Journal.Notify()
	e.forget(r.Key)
	return true
}

func fillExit(en *v1.RunReportEntry, f facts) {
	switch {
	case f.lost:
		en.Cause = v1.ExitCauseWrapperLost
	case f.exit != nil:
		en.ExitCode, en.Signal, en.Cause, en.Message = f.exit.ExitCode, f.exit.Signal, f.exit.Cause, f.exit.Error
	default:
		en.Cause, en.Message = v1.ExitCauseExit, f.message
	}
}

// forget drops what the executor kept in memory about a Run that is over.
func (e *Executor) forget(k state.Key) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.failures, k)
	delete(e.lastErr, k)
	delete(e.signaled, k)
	delete(e.retryAt, k)
}
