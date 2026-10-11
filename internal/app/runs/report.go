package runs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// internalError marks a failure after an entry started writing. It hides the
// error from errors.As on purpose: a domain error there would be taken for a
// rejection of the entry while half of it is already in the transaction.
type internalError struct{ error }

// Report applies what the Worker tells about a Run it executes (§10.4 point 5,
// §16.8): the harness started, exited, failed to prepare, or has no process
// left. The whole request is one transaction.
//
// The entries are applied in order, each against the state the previous left,
// so started, exited and stopped can travel in one batch. The first one the
// Controller rejects stops the batch: it and what follows write nothing, the
// answer says which, and the Worker sends the rest again. An entry is
// identified by (worker, epoch, seq): one already stored is a duplicate if it
// says the same thing, and a rejection if it does not. A failure that is not a
// rejection of an entry rolls the whole request back.
func (s *Service) Report(ctx context.Context, by pki.Session, runID string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
	if err := validateReport(req); err != nil {
		return v1.RunReportResponse{}, err
	}
	if kind, _, err := domain.ParseID(runID); err != nil || kind != domain.KindRun {
		return v1.RunReportResponse{}, domain.NewError(domain.CodeNotFound, "run not found", nil)
	}
	return mutation.Do(ctx, s.db, s.clock, by, "worker.run.report", "", struct {
		Run string `json:"run"`
		v1.RunReportRequest
	}{runID, req}, func(tx *store.Tx, act mutation.Act) (v1.RunReportResponse, error) {
		w, err := store.WorkerByIdentity(ctx, tx, by.Principal.ID)
		if errors.Is(err, store.ErrNotFound) {
			return v1.RunReportResponse{}, domain.NewError(domain.CodeNotFound, "this identity is not a Worker", nil)
		}
		if err != nil {
			return v1.RunReportResponse{}, err
		}
		run := domain.ID(runID)
		// A Run the Worker was never offered is as unknown as one that does not exist.
		had, err := store.WorkerHadLeaseOn(ctx, tx, w.ID, run)
		if err != nil {
			return v1.RunReportResponse{}, err
		}
		if !had {
			return v1.RunReportResponse{}, domain.NewError(domain.CodeNotFound, "run not found", nil)
		}
		out := v1.RunReportResponse{Outcomes: make([]v1.RunReportOutcome, 0, len(req.Entries))}
		stopped := false
		for _, e := range req.Entries {
			if stopped {
				out.Outcomes = append(out.Outcomes, v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: v1.ReportNotApplied})
				continue
			}
			outcome, err := s.reportEntry(ctx, tx, act, w, run, e)
			var rejection *domain.Error
			switch {
			case errors.As(err, &rejection):
				outcome = v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: v1.ReportRejected, Code: rejection.Code, Message: rejection.Message}
				stopped = true
			case err != nil:
				return v1.RunReportResponse{}, err
			}
			out.Outcomes = append(out.Outcomes, outcome)
		}
		current, err := store.RunByID(ctx, tx, run)
		if err != nil {
			return v1.RunReportResponse{}, err
		}
		out.Run = view(current, false)
		return out, nil
	})
}

// reportEntry decides one entry. Everything that can reject it comes before
// the first write.
func (s *Service) reportEntry(ctx context.Context, tx *store.Tx, act mutation.Act, w store.Worker, runID domain.ID, e v1.RunReportEntry) (v1.RunReportOutcome, error) {
	outcome := func(result string) v1.RunReportOutcome {
		return v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: result}
	}
	digest := reportDigest(runID, e)

	stored, found, err := store.WorkerEvent(ctx, tx, w.ID, e.WorkerEpoch, e.WorkerSeq)
	if err != nil {
		return outcome(""), err
	}
	if found {
		if stored.RunID == runID && stored.Data[dataReportDigest] == digest {
			return outcome(v1.ReportDuplicate), nil
		}
		return outcome(""), domain.NewError(domain.CodeInvalidRequest, "worker_seq reused for a different report", nil)
	}

	l, err := store.LeaseByRunEpoch(ctx, tx, runID, e.LeaseEpoch)
	if errors.Is(err, store.ErrNotFound) || (err == nil && l.WorkerID != w.ID) {
		return outcome(""), domain.NewError(domain.CodeNotFound, "lease not found", nil)
	}
	if err != nil {
		return outcome(""), err
	}
	run, err := store.RunByID(ctx, tx, runID)
	if err != nil {
		return outcome(""), err
	}
	if l.Epoch < run.LeaseEpoch || l.State != domain.LeaseAccepted {
		return outcome(""), domain.NewError(domain.CodeStaleLease, "this report is not under the live lease of the Run",
			map[string]any{"lease": string(l.ID), "epoch": l.Epoch, "state": string(l.State)})
	}
	task, err := store.TaskByID(ctx, tx, run.TaskID)
	if err != nil {
		return outcome(""), err
	}

	ev := act.Event(domain.EventRunStarted, "")
	ev.Timestamp = e.At
	ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = task.ProjectID, task.ID, run.ID, w.ID
	ev.WorkerEpoch, ev.WorkerSeq = e.WorkerEpoch, &e.WorkerSeq
	ev.Actor = domain.ActorRef{Kind: domain.ActorWorker, ID: string(w.ID)}
	ev.Data = map[string]any{"lease_epoch": e.LeaseEpoch, "report": string(e.Kind), dataReportDigest: digest, "short_id": shortID(run.Short)}
	if e.Message != "" {
		ev.Data["message"] = e.Message
	}

	var apply func() error
	switch e.Kind {
	case v1.ReportStarted:
		if _, err := domain.NextRunState(domain.RunStatus{State: run.State}, domain.RunEventHarnessStarted); err != nil {
			return outcome(""), err
		}
		raw, err := json.Marshal(e.Handle)
		if err != nil {
			return outcome(""), internalError{err}
		}
		ev.Type, ev.Message = domain.EventRunStarted, "run started"
		ev.Data["handle"] = e.Handle
		apply = func() error { return tx.StartRun(ctx, run, e.At, string(raw)) }
	case v1.ReportExited:
		event, reason := domain.RunEventExitNonZero, domain.EndExitCode
		if e.ExitCode != nil && *e.ExitCode == 0 && e.Cause != v1.ExitCauseWrapperLost {
			event = domain.RunEventExitZero
		}
		if e.Cause == v1.ExitCauseWrapperLost {
			reason = domain.EndWrapperLost
		}
		next, err := domain.NextRunState(domain.RunStatus{State: run.State}, event)
		if err != nil {
			return outcome(""), err
		}
		ev.Type, ev.Message = domain.EventRunFailed, "run failed"
		if next.State == domain.RunCompleted {
			ev.Type, ev.Message = domain.EventRunCompleted, "run completed"
		}
		ev.Data["exit_code"], ev.Data["signal"], ev.Data["cause"] = e.ExitCode, e.Signal, e.Cause
		apply = func() error { return tx.ExitRun(ctx, run, next.State, reason, e.ExitCode, e.At) }
	case v1.ReportPrepareFailed:
		next, err := domain.NextRunState(domain.RunStatus{State: run.State}, domain.RunEventPrepareFailed)
		if err != nil {
			return outcome(""), err
		}
		ev.Type, ev.Message = domain.EventRunFailed, "run failed while preparing"
		ev.Data["phase"] = string(domain.RunPreparing)
		apply = func() error { return tx.FinishRun(ctx, run, next.State, domain.EndEnvironment, e.At) }
	case v1.ReportStopped:
		if run.Runtime == domain.RuntimeStopped {
			return outcome(""), domain.NewError(domain.CodeInvalidTransition, "the runtime of this Run is stopped already", nil)
		}
		if e.NeverStarted && run.Runtime == domain.RuntimeActive {
			return outcome(""), domain.NewError(domain.CodeInvalidTransition, "the harness of this Run did start", nil)
		}
		ev.Type, ev.Message = domain.EventRunStopped, "run stopped"
		ev.Data["never_started"] = e.NeverStarted
		apply = func() error {
			if err := tx.StopRuntime(ctx, run); err != nil {
				return err
			}
			run.Runtime = domain.RuntimeStopped
			return s.releaseIfAllowed(ctx, tx, act, run, l, e)
		}
	default:
		return outcome(""), input.Invalid("unknown report kind %q", e.Kind)
	}

	if err := apply(); err != nil {
		return outcome(""), internalError{err}
	}
	if _, dup, err := tx.AppendEvent(ctx, ev); err != nil || dup {
		if err == nil {
			err = errors.New("runs: a report that was not stored is stored")
		}
		return outcome(""), internalError{err}
	}
	if err := s.rederive(ctx, tx, act, task.ID); err != nil {
		return outcome(""), internalError{err}
	}
	return outcome(v1.ReportApplied), nil
}

// releaseIfAllowed releases the reservation of the lease once the rules of
// §10.5 allow it, in the transaction of the report that allows it.
func (s *Service) releaseIfAllowed(ctx context.Context, tx *store.Tx, act mutation.Act, run store.Run, l store.Lease, e v1.RunReportEntry) error {
	if !releasable(run, e.NeverStarted) {
		return nil
	}
	open, err := store.OpenReservation(ctx, tx, l.ID)
	if err != nil || !open {
		return err
	}
	return tx.ReleaseReservation(ctx, l.ID, releaseRunStopped, act.Now)
}

const dataReportDigest = "report_digest"

// reportDigest identifies what an entry says, apart from the (epoch, seq) that
// name it, so that a replay can be told from a seq that was reused.
func reportDigest(run domain.ID, e v1.RunReportEntry) string {
	e.WorkerEpoch, e.WorkerSeq = "", 0
	raw, _ := json.Marshal(struct {
		Run   domain.ID `json:"run"`
		Entry any       `json:"entry"`
	}{run, e})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validateReport(req v1.RunReportRequest) error {
	if n := len(req.Entries); n == 0 || n > v1.MaxReportEntries {
		return input.Invalid("a report has between 1 and %d entries", v1.MaxReportEntries)
	}
	if raw, err := json.Marshal(req); err != nil || len(raw) > v1.MaxReportBytes {
		return input.Invalid("a report is at most %d bytes", v1.MaxReportBytes)
	}
	var last uint64
	for _, e := range req.Entries {
		if _, err := ulid.ParseStrict(e.WorkerEpoch); err != nil {
			return input.Invalid("worker_epoch must be a ULID")
		}
		if e.WorkerSeq == 0 || e.WorkerSeq <= last || e.WorkerSeq > 1<<62 {
			return input.Invalid("worker_seq must be positive and grow along the entries")
		}
		last = e.WorkerSeq
		if err := validateEntry(e); err != nil {
			return err
		}
	}
	return nil
}

func validateEntry(e v1.RunReportEntry) error {
	switch {
	case e.LeaseEpoch == 0:
		return input.Invalid("lease_epoch is required")
	case !e.Kind.IsValid():
		return input.Invalid("kind must be started, exited, prepare_failed or stopped")
	case e.At.IsZero():
		return input.Invalid("at is required")
	case len(e.Message) > v1.MaxReportMessageLen || hasHidden(e.Message):
		return input.Invalid("message must print and be at most %d characters", v1.MaxReportMessageLen)
	case e.Kind != v1.ReportStarted && e.Handle != nil:
		return input.Invalid("only started carries a handle")
	case e.Kind != v1.ReportExited && (e.ExitCode != nil || e.Signal != "" || e.Cause != ""):
		return input.Invalid("only exited carries an exit code, a signal or a cause")
	case e.Kind != v1.ReportStopped && e.NeverStarted:
		return input.Invalid("only stopped says never_started")
	}
	switch e.Kind {
	case v1.ReportStarted:
		h := e.Handle
		if h == nil || h.Runtime == "" || h.Ref == "" || len(h.Runtime)+len(h.Ref) > v1.MaxHandleLen ||
			strings.ContainsFunc(h.Runtime+h.Ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return input.Invalid("handle needs a runtime and a ref of at most %d bytes with no spaces", v1.MaxHandleLen)
		}
	case v1.ReportExited:
		if !slices.Contains([]string{v1.ExitCauseExit, v1.ExitCauseStop, v1.ExitCauseWrapperLost}, e.Cause) {
			return input.Invalid("cause must be exit, stop or wrapper_lost")
		}
		if e.ExitCode != nil && (*e.ExitCode < 0 || *e.ExitCode > 255) {
			return input.Invalid("exit_code must be between 0 and 255")
		}
		if e.Cause == v1.ExitCauseWrapperLost && (e.ExitCode != nil || e.Signal != "") {
			return input.Invalid("a lost wrapper leaves no exit code or signal")
		}
		if len(e.Signal) > 32 || hasHidden(e.Signal) {
			return input.Invalid("signal must print and be at most 32 characters")
		}
	}
	return nil
}

func hasHidden(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}
