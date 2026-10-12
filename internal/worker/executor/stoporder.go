package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// persistStop stores a stop of the Controller in the transaction of the cursor.
// It says whether the Worker holds a record of the assignment: one it does not
// is rebuilt after the commit (stopOrdered).
func (e *Executor) persistStop(ctx context.Context, tx *state.Tx, c v1.WorkerCommand) error {
	s := c.Stop
	k := state.Key{RunID: s.RunID, LeaseEpoch: s.Epoch}
	if _, err := state.RunBy(ctx, tx, k); err == nil {
		if _, err := state.AddStopOrder(ctx, tx, state.StopOrder{Key: k, Reason: string(s.Reason), Source: SourceController, CreatedAt: e.clock.Now()}); err != nil {
			return err
		}
	} else if !errors.Is(err, state.ErrNotFound) {
		return err
	}
	return state.SetCursor(ctx, tx, c.Seq)
}

// stopOrdered carries out a stop of the Controller (§10.7). With a record of
// the assignment it is the normal Stop. Without one (the disk was lost, or the
// record never existed here) it is never taken for done by a signal: if the
// session or the markers of the Run are there, a minimal record is rebuilt from
// the order and the normal Stop runs on it, which verifies the identity of the
// wrapper and reports the exit and RunStopped; if nothing of it is there, the
// order is done and RunStopped(never_started) reported.
func (e *Executor) stopOrdered(ctx context.Context, s v1.StopCommand) error {
	k := state.Key{RunID: s.RunID, LeaseEpoch: s.Epoch}
	if _, err := state.RunBy(ctx, e.o.DB.Reader(), k); err == nil {
		return e.Stop(ctx, s.RunID, s.Epoch, string(s.Reason), SourceController)
	} else if !errors.Is(err, state.ErrNotFound) {
		return err
	}
	unlock := e.lock(k)
	adopted, err := e.rebuildForStop(ctx, s)
	unlock()
	if err != nil || !adopted {
		return err
	}
	return e.Stop(ctx, s.RunID, s.Epoch, string(s.Reason), SourceController)
}

// rebuildForStop writes the record of an assignment a stop names and this
// Worker has no record of. The caller holds the lock of the Run. adopted says
// that something of the Run is here, and it is to be stopped the normal way;
// otherwise the stop is recorded as done, with its report.
func (e *Executor) rebuildForStop(ctx context.Context, s v1.StopCommand) (adopted bool, err error) {
	k := state.Key{RunID: s.RunID, LeaseEpoch: s.Epoch}
	alive, err := e.sessionAlive(ctx, k)
	if err != nil {
		return false, err
	}
	dir := e.runDir(k)
	_, dirErr := os.Stat(dir)
	_, wrapperErr := os.Stat(filepath.Join(dir, execwrap.WrapperFile))
	now := e.clock.Now()
	r := state.Run{
		Key: k, LeaseID: s.LeaseID, RunShort: s.RunID, TaskID: s.TaskID, ProjectID: s.ProjectID, AcceptBy: now,
		Spec: []byte(`{}`), Branch: s.Branch, CreatedAt: now,
		// The Controller may know of its start already: `started` is never reported for a rebuilt record.
		StartedAt: &now, Handle: mustJSON(*e.runtimeHandle(k)),
	}
	order := state.StopOrder{Key: k, Reason: string(s.Reason), Source: SourceController, CreatedAt: now}
	if !alive && dirErr != nil {
		r.Phase, r.NeverStarted, r.StartedAt, r.Handle = state.PhaseAborted, true, nil, ""
		err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
			if inserted, err := state.InsertAdopted(ctx, tx, r); err != nil || !inserted {
				return err
			}
			if _, err := state.AddStopOrder(ctx, tx, order); err != nil {
				return err
			}
			if err := e.report(ctx, tx, k, v1.ReportStopped, now, func(en *v1.RunReportEntry) {
				en.NeverStarted, en.Message = true, "nothing of the Run is on this Worker"
			}); err != nil {
				return err
			}
			return state.CompleteStopOrder(ctx, tx, k, now)
		})
		if err == nil {
			e.o.Journal.Notify()
		}
		return false, err
	}
	r.Phase = state.PhaseStarting
	if wrapperErr == nil {
		r.Phase = state.PhaseRunning
	}
	e.log.InfoContext(ctx, "a stop names a Run this Worker has no record of: the record is rebuilt to stop it", "run", s.RunID,
		"epoch", s.Epoch, "session", cfruntime.NameFor(s.RunID, s.Epoch), "alive", alive)
	err = e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		if _, err := state.InsertAdopted(ctx, tx, r); err != nil {
			return err
		}
		_, err := state.AddStopOrder(ctx, tx, order)
		return err
	})
	return err == nil, err
}
