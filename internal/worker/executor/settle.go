package executor

import (
	"cmp"
	"context"
	"log/slog"
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
	ordered bool              // a stop was ordered
	// exitFirst: the harness had ended before the stop was ordered.
	exitFirst bool
	message   string
}

// verdictOf says what the process with the wrapper's pid is now. The wrapper
// leads its own group (pgid == pid), which is what lets a verdict about the pid
// say something about the group; a record where that is not so says nothing.
func verdictOf(w *execwrap.Wrapper) procident.Verdict {
	if w.PGID != w.PID {
		return procident.Unknown
	}
	return procident.Verify(w.PID, w.Start)
}

// unresolved is a wrapper whose identity cannot be established. Nothing is
// signalled. A Run that is being stopped is parked in stop_unverified, to be
// verified again; one nobody asked to stop is left as it is and looked at again
// on the next tick: not knowing is no reason to stop a Run.
func (e *Executor) unresolved(ctx context.Context, r state.Run, why string) (done bool) {
	if r.Phase == state.PhaseStopping || r.Phase == state.PhaseStopUnverified {
		return e.markUnverified(ctx, r, why)
	}
	level := slog.LevelDebug
	if e.firstUnresolved(r.Key) {
		level = slog.LevelWarn // once, on entering the state
	}
	e.log.Log(ctx, level, "the wrapper of a Run cannot be identified; looking again", "run", r.RunID, "why", why)
	return false
}

// settleExit is the Run having an exit.json. The wrapper wrote it after it
// checked that nothing was left of the group; the daemon, which must not take a
// stop for granted, checks that the wrapper is gone and the group is empty.
func (e *Executor) settleExit(ctx context.Context, r state.Run, m markers) (done bool) {
	if w := m.wrapper; w != nil {
		switch verdictOf(w) {
		case procident.Same:
			if e.clock.Now().Sub(m.exit.ExitedAt) < wrapperExitWait {
				return false // it is ending
			}
			e.log.WarnContext(ctx, "the wrapper wrote its exit and did not end: killing it", "run", r.RunID)
			_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
			return false
		case procident.Gone:
			// The wrapper is gone, so a group with this ID is what it left behind.
			if !e.groupEmpty(ctx, w) {
				e.log.WarnContext(ctx, "processes of a Run survive its wrapper: killing them", "run", r.RunID)
				_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
				return false
			}
		case procident.Rebooted, procident.Different:
			// Another boot, or another process with the pid: the group of the wrapper
			// is empty (a pid is not reused while a group with its ID has members),
			// and whatever has this ID now is not ours.
		default:
			return e.unresolved(ctx, r, "the identity of the wrapper cannot be verified")
		}
	}
	e.endSession(ctx, r.Key)
	fresh := e.refresh(r, m)
	if fresh.exit == nil {
		fresh.exit = m.exit
	}
	f := facts{at: fresh.exit.ExitedAt, exit: fresh.exit, started: fresh.started}
	if d := fresh.diagnosis(); d != "" && fresh.exit.Error == "" {
		f.message = d
	}
	return e.finalize(ctx, r, f)
}

// refresh reads the markers of a Run again, once its wrapper is known to be
// over: what the first reading saw was taken while the wrapper could still write,
// and a started or an exit written in between would be missed. The wrapper record
// that was verified stays. A marker that cannot be read does not hold the Run
// back, since the wrapper is over and will never write it again: an exit that
// cannot be read is no exit (the Run is lost, with the error in its message), and
// a started that cannot be read is a harness that was launched.
func (e *Executor) refresh(r state.Run, m markers) markers {
	fresh := e.markers(r.Key)
	fresh.wrapper, fresh.wrapperErr = m.wrapper, nil
	if fresh.startedErr != nil {
		fresh.started = &execwrap.Started{StartedAt: launchedAt(fresh, e.clock.Now())}
	}
	return fresh
}

// launchedAt is when a harness whose started marker cannot be read was
// launched: when its wrapper started, never after its exit, and now only when
// neither is known.
func launchedAt(m markers, now time.Time) time.Time {
	at := now
	if m.wrapper != nil && !m.wrapper.StartedAt.IsZero() {
		at = m.wrapper.StartedAt
	}
	if m.exit != nil && at.After(m.exit.ExitedAt) {
		at = m.exit.ExitedAt
	}
	return at
}

// settleLost is the wrapper being gone with no exit. Only if it is gone in the
// same boot can something of its group be left, and then it is ours to end;
// after a reboot, or with the pid in other hands, nothing is signalled and the
// group counts as empty.
func (e *Executor) settleLost(ctx context.Context, r state.Run, m markers, v procident.Verdict) (done bool) {
	if m = e.refresh(r, m); m.exit != nil {
		return e.settleExit(ctx, r, m) // it wrote its exit and then died: that is not a loss
	}
	w := m.wrapper
	e.log.WarnContext(ctx, "the wrapper of a Run is gone without an exit", "run", r.RunID, "verdict", v.String())
	if v == procident.Gone && !e.groupEmpty(ctx, w) {
		_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
		return false
	}
	e.endSession(ctx, r.Key)
	if m = e.refresh(r, m); m.exit != nil {
		return e.settleExit(ctx, r, m)
	}
	message := "the wrapper died without leaving an exit"
	if d := m.diagnosis(); d != "" {
		message += ": " + d
	}
	return e.finalize(ctx, r, facts{at: e.clock.Now(), started: m.started, lost: true, message: message})
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
	if verdictOf(m.wrapper) == procident.Same {
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
	e.memo(r.Key).retryAt = e.clock.Now().Add(unverifiedRetry)
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
//
// `started` means that the session was created, so it is reported before the
// harness is known to have spawned. A harness that spawned (started.json) ends
// with exited and stopped. One that never spawned ends with prepare_failed and
// stopped, whether the failure was seen before or after `started` and whoever
// ordered the stop: the Controller notes it on a Run it ended, and it is the
// evidence that releases its reservation (B5). stopped says never_started only
// if `started` was never reported.
func (e *Executor) finalize(ctx context.Context, r state.Run, f facts) (done bool) {
	order, orderErr := state.StopOrderOf(ctx, e.o.DB.Reader(), r.Key)
	f.ordered = orderErr == nil && order.Reason != ""
	f.exitFirst = orderErr == nil && order.ExitFirst
	spawned := f.started != nil
	reportedStart := r.StartedAt != nil || spawned
	// A Run whose harness ran is to be sealed (§22.6). The intent, which is all that sealing depends
	// on, is stored with the exit; HEAD is read before the transaction because git is not in it.
	sealing := spawned && e.o.Git != nil
	var parent string
	if sealing {
		// Finalizing never depends on git: whatever HEAD fails with, the intent is stored
		// with no parent and the Run is closed. The sealing reads HEAD again, or fails.
		var err error
		if parent, err = e.o.Git.Head(ctx, e.worktree(r.Key)); err != nil {
			e.log.WarnContext(ctx, "HEAD of the worktree of a Run cannot be read: the sealing will try again", "run", r.RunID, "err", err)
			parent = ""
		}
	}
	now := e.clock.Now()
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if spawned && r.StartedAt == nil {
			if err := state.SetStarted(ctx, tx, r.Key, mustJSON(*e.runtimeHandle(r.Key)), f.started.StartedAt); err != nil {
				return err
			}
			if err := e.report(ctx, tx, r.Key, v1.ReportStarted, f.started.StartedAt, func(en *v1.RunReportEntry) {
				en.Handle = e.runtimeHandle(r.Key)
			}); err != nil {
				return err
			}
		}
		switch {
		case spawned:
			if err := e.report(ctx, tx, r.Key, v1.ReportExited, f.at, func(en *v1.RunReportEntry) { fillExit(en, f) }); err != nil {
				return err
			}
		default:
			message := f.message
			if f.exit != nil && f.exit.Error != "" {
				message = f.exit.Error
			}
			if err := e.report(ctx, tx, r.Key, v1.ReportPrepareFailed, f.at, func(en *v1.RunReportEntry) { en.Message = message }); err != nil {
				return err
			}
		}
		wasRunning := orderErr == nil && order.WasRunning
		if err := e.report(ctx, tx, r.Key, v1.ReportStopped, f.at, func(en *v1.RunReportEntry) {
			en.NeverStarted, en.WasRunning = !reportedStart, wasRunning && reportedStart
		}); err != nil {
			return err
		}
		if err := state.Close(ctx, tx, r.Key, state.OccupyingPhases, state.PhaseStopped, !reportedStart, now); err != nil {
			return err
		}
		if sealing {
			if _, err := state.InsertPublication(ctx, tx, state.Publication{Key: r.Key, ParentOID: parent, SealedAt: f.at, CreatedAt: now}); err != nil {
				return err
			}
		}
		return state.CompleteStopOrder(ctx, tx, r.Key, now)
	})
	if err != nil {
		e.log.ErrorContext(ctx, "finalizing a Run", "run", r.RunID, "err", err)
		return false
	}
	e.o.Journal.Notify()
	if sealing {
		e.kickPublisher()
	}
	e.forget(r.Key)
	e.queueRelease(r, nil)
	return true
}

// fillExit describes how the harness ended. A wrapper that was lost while a stop
// was ordered is a stop that ended the Run, not a loss.
func fillExit(en *v1.RunReportEntry, f facts) {
	switch {
	case f.lost && f.ordered:
		en.Cause, en.Message = v1.ExitCauseStop, f.message
	case f.lost:
		en.Cause, en.Message = v1.ExitCauseWrapperLost, f.message
	case f.exit != nil:
		en.ExitCode, en.Signal, en.Cause, en.Message = f.exit.ExitCode, f.exit.Signal, f.exit.Cause, cmp.Or(f.exit.Error, f.message)
		if f.ordered && !f.exitFirst && f.exit.Signal != "" && f.exit.Cause == v1.ExitCauseExit {
			// A harness that a signal killed after a stop was ordered ended by the
			// stop, whatever the wrapper made of it; one that had ended before keeps its cause.
			en.Cause = v1.ExitCauseStop
		}
	default:
		en.Cause, en.Message = v1.ExitCauseExit, f.message
	}
}

// forget drops what the executor kept in memory about a Run that is over.
func (e *Executor) forget(k state.Key) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.memos, k)
}
