package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// lateEntry decides a started, exited or prepare_failed about a Run the
// Controller ended (cancelled, lost, failed for a lost assignment or worktree)
// under its live lease (B5). The state and its cause never change: the entry
// is noted, so that the Worker is not fenced and the reservation can be
// released by the evidence it brings.
//
//   - started: the session was created after the end. The runtime goes from none
//     to active (never again from stopped), its result is to be sealed, and the
//     Worker is ordered to stop it; a lost Run running again is an orphan.
//   - exited: the harness ran. The exit code is kept, and its result is to be sealed.
//   - prepare_failed: the harness never ran. A pending publication with nothing
//     sealed goes back to none, which lets the reservation go once it stops.
func (s *Service) lateEntry(ctx context.Context, tx *store.Tx, act mutation.Act, ev *domain.Event,
	run store.Run, task store.Task, l store.Lease, e v1.RunReportEntry,
) (apply func() error, err error) {
	ev.Data["after_end"] = true
	switch e.Kind {
	case v1.ReportStarted:
		if run.Runtime != domain.RuntimeNone {
			return nil, domain.NewError(domain.CodeInvalidTransition, "the runtime of this Run is "+string(run.Runtime)+": it was started already", nil)
		}
		raw, err := json.Marshal(e.Handle)
		if err != nil {
			return nil, internalError{err}
		}
		ev.Type, ev.Message = domain.EventRunStarted, "run started after it ended"
		ev.Data["handle"] = e.Handle
		return func() error {
			if err := tx.NoteStart(ctx, run, e.At, string(raw)); err != nil {
				return err
			}
			run.Runtime = domain.RuntimeActive
			if err := MarkExecutedIn(ctx, tx, &run); err != nil {
				return err
			}
			if _, err := OrderStopIn(ctx, tx, act, run, task, l, StopReasonOf(run)); err != nil {
				return err
			}
			if run.State == domain.RunLost {
				_, err := OrphanIn(ctx, tx, act, run, task, l)
				return err
			}
			return nil
		}, nil

	case v1.ReportExited:
		if run.HarnessRan != nil && !*run.HarnessRan {
			return nil, domain.NewError(domain.CodeInvalidTransition, "the harness of this Run never ran", nil)
		}
		ev.Type, ev.Message = domain.EventRunExited, "run exited after it ended"
		ev.Data["exit_code"], ev.Data["signal"], ev.Data["cause"], ev.Data["harness_ran"] = e.ExitCode, e.Signal, e.Cause, true
		return func() error {
			if err := tx.NoteExit(ctx, run, e.ExitCode); err != nil {
				return err
			}
			ran := true
			run.HarnessRan = &ran
			if err := MarkExecutedIn(ctx, tx, &run); err != nil {
				return err
			}
			return s.releaseIfAllowed(ctx, tx, act, run, l)
		}, nil

	default: // prepare_failed
		if run.HarnessRan != nil && *run.HarnessRan {
			return nil, domain.NewError(domain.CodeInvalidTransition, "the harness of this Run ran", nil)
		}
		ev.Type, ev.Message = domain.EventRunExited, "run ended after it ended, without running its harness"
		ev.Data["harness_ran"] = false
		return func() error {
			if run.HarnessRan == nil {
				if err := tx.SetHarnessRan(ctx, run, false); err != nil {
					return err
				}
				ran := false
				run.HarnessRan = &ran
			}
			if run.Publication == domain.PublicationPending && run.ResultCommit == "" {
				if err := tx.ClearPublication(ctx, run); err != nil {
					return err
				}
				run.Publication = domain.PublicationNone
			}
			return s.releaseIfAllowed(ctx, tx, act, run, l)
		}, nil
	}
}

// orphanPreserved decides the report that the result of an orphan is in the
// relay (§10.4 point 5): the only one taken under a lease epoch that is not the
// live one. The orphan must be one the Controller detected on this Worker under
// that epoch, and the relay must hold the commit at the orphan's ref; it is
// recorded once.
func (s *Service) orphanPreserved(ctx context.Context, tx *store.Tx, act mutation.Act, w store.Worker, run store.Run, task store.Task,
	l store.Lease, e v1.RunReportEntry, digest string,
) (v1.RunReportOutcome, error) {
	outcome := func(result string) v1.RunReportOutcome {
		return v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: result}
	}
	o, err := store.OrphanOf(ctx, tx, w.ID, run.ID, l.Epoch)
	if errors.Is(err, store.ErrNotFound) {
		return outcome(""), domain.NewError(domain.CodeStaleLease, "this is not an orphan of this Worker", nil)
	}
	if err != nil {
		return outcome(""), err
	}
	if o.Preserved == store.OrphanInRelay {
		if o.ResultCommit != e.ResultCommit {
			return outcome(""), domain.NewError(domain.CodeInvalidTransition, "the orphan was preserved with another commit", nil)
		}
		return outcome(""), domain.NewError(domain.CodeInvalidTransition, "the orphan is preserved already", nil)
	}
	if s.relay == nil {
		return outcome(""), internalError{errors.New("runs: there is no git relay to verify an orphan against")}
	}
	ref := domain.OrphanRef(run.ID, l.Epoch)
	tip, found, err := s.relay.RefTip(ctx, task.ProjectID, ref)
	if err != nil {
		return outcome(""), internalError{fmt.Errorf("runs: verifying an orphan: %w", err)}
	}
	if !found || tip != e.ResultCommit {
		return outcome(""), domain.NewError(domain.CodeInvalidRequest, "the relay does not hold this commit for the orphan", nil)
	}
	if err := tx.PreserveOrphan(ctx, w.ID, run.ID, l.Epoch, e.ResultCommit, act.Now); err != nil {
		return outcome(""), internalError{err}
	}
	ev := act.Event(domain.EventRunOrphanPreserved, "orphan preserved in the relay")
	ev.Timestamp = e.At
	ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = task.ProjectID, task.ID, run.ID, w.ID
	ev.WorkerEpoch, ev.WorkerSeq = e.WorkerEpoch, &e.WorkerSeq
	ev.Actor = domain.ActorRef{Kind: domain.ActorWorker, ID: string(w.ID)}
	ev.Data = map[string]any{
		"lease_epoch": e.LeaseEpoch, "report": string(e.Kind), dataReportDigest: digest, "short_id": shortID(run.Short),
		"result_commit": e.ResultCommit, "ref": ref,
	}
	if _, dup, err := tx.AppendEvent(ctx, ev); err != nil || dup {
		if err == nil {
			err = errors.New("runs: a report that was not stored is stored")
		}
		return outcome(""), internalError{err}
	}
	if err := tx.InsertReceipt(ctx, w.ID, e.WorkerEpoch, e.WorkerSeq, store.Receipt{RunID: run.ID, Kind: string(e.Kind), Digest: digest}, act.Now); err != nil {
		return outcome(""), internalError{err}
	}
	return outcome(v1.ReportApplied), nil
}
