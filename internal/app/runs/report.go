package runs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
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
	if err := validateReport(req, s.clock.Now()); err != nil {
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
			return v1.RunReportResponse{}, domain.NewError(domain.CodeUnauthenticated, "this identity is not a Worker", nil)
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
		if out.Run, err = s.viewOf(ctx, tx, current, false); err != nil {
			return v1.RunReportResponse{}, err
		}
		if !ownsLive(current, w.ID, req.Entries) {
			// A lease that is no longer the Run's tells nothing of who has it now.
			out.Run = v1.Run{ID: out.Run.ID, State: current.State, Runtime: current.Runtime, Publication: current.Publication}
		}
		return out, nil
	})
}

// ownsLive says whether the Run is held by this Worker under one of the lease
// epochs it reported with.
func ownsLive(run store.Run, worker domain.ID, entries []v1.RunReportEntry) bool {
	return run.WorkerID == worker && slices.ContainsFunc(entries, func(e v1.RunReportEntry) bool { return e.LeaseEpoch == run.LeaseEpoch })
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
	task, err := store.TaskByID(ctx, tx, run.TaskID)
	if err != nil {
		return outcome(""), err
	}
	if l.Epoch < run.LeaseEpoch || l.State != domain.LeaseAccepted {
		if e.Kind == v1.ReportOrphanPreserved {
			return s.orphanPreserved(ctx, tx, act, w, run, task, l, e, digest)
		}
		// Something reports under a lease the Run is no longer under: whatever runs
		// with it is an orphan. Its stop and RunOrphaned are written although the
		// entry is rejected: the rejection does not roll the request back (B5).
		// A stopped says that nothing runs any more under it: there is no process to
		// stop, and it was an orphan only if it says the process was alive.
		if l.Epoch < run.LeaseEpoch && (e.Kind != v1.ReportStopped || e.WasRunning) {
			if _, err := OrphanIn(ctx, tx, act, run, task, l); err != nil {
				return outcome(""), internalError{err}
			}
		}
		return outcome(""), domain.NewError(domain.CodeStaleLease, "this report is not under the live lease of the Run",
			map[string]any{"lease": string(l.ID), "epoch": l.Epoch, "state": string(l.State)})
	}
	if e.Kind == v1.ReportOrphanPreserved {
		return outcome(""), domain.NewError(domain.CodeInvalidTransition, "the Run is not an orphan under this lease", nil)
	}

	ev := act.Event("", "") // each kind below gives it its type
	ev.Timestamp = e.At
	ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = task.ProjectID, task.ID, run.ID, w.ID
	ev.WorkerEpoch, ev.WorkerSeq = e.WorkerEpoch, &e.WorkerSeq
	ev.Actor = domain.ActorRef{Kind: domain.ActorWorker, ID: string(w.ID)}
	ev.Data = map[string]any{"lease_epoch": e.LeaseEpoch, "report": string(e.Kind), dataReportDigest: digest, "short_id": shortID(run.Short)}
	if e.Message != "" {
		ev.Data["message"] = e.Message
	}

	var (
		apply   func() error
		noEvent bool // the entry says what the Controller knows already: applied, with nothing to tell
	)
	switch {
	case ControllerEnded(run) && (e.Kind == v1.ReportStarted || e.Kind == v1.ReportExited || e.Kind == v1.ReportPrepareFailed):
		if apply, err = s.lateEntry(ctx, tx, act, &ev, run, task, l, e); err != nil {
			return outcome(""), err
		}
	case e.Kind == v1.ReportStarted:
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
	case e.Kind == v1.ReportExited:
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
		if next.State == domain.RunCompleted {
			ev.Type, ev.Message = domain.EventRunCompleted, "run completed"
		} else {
			ev.Type, ev.Message = domain.EventRunFailed, "run failed"
		}
		ev.Data["exit_code"], ev.Data["signal"], ev.Data["cause"] = e.ExitCode, e.Signal, e.Cause
		apply = func() error { return tx.ExitRun(ctx, run, next.State, reason, e.ExitCode, e.At) }
	case e.Kind == v1.ReportPrepareFailed:
		// `started` means that the session was created. The harness can still fail to
		// spawn, and then the Run was `running` for the Controller: it fails all the same,
		// as a Run that never ran (publication none).
		event, phase := domain.RunEventPrepareFailed, domain.RunPreparing
		if run.State == domain.RunRunning {
			event, phase = domain.RunEventExitNonZero, domain.RunRunning
		}
		next, err := domain.NextRunState(domain.RunStatus{State: run.State}, event)
		if err != nil {
			return outcome(""), err
		}
		ev.Type, ev.Message = domain.EventRunFailed, "run failed before the harness ran"
		ev.Data["phase"] = string(phase)
		apply = func() error {
			if err := tx.FinishRun(ctx, run, next.State, domain.EndEnvironment, e.At); err != nil {
				return err
			}
			return tx.SetHarnessRan(ctx, run, false)
		}
	case e.Kind == v1.ReportStopped:
		if e.NeverStarted && run.Runtime == domain.RuntimeActive {
			return outcome(""), domain.NewError(domain.CodeInvalidTransition, "the harness of this Run did start", nil)
		}
		if !e.NeverStarted && !run.State.IsTerminal() {
			return outcome(""), domain.NewError(domain.CodeInvalidTransition, "the Run is not over: only a Run that never started can be stopped before it ends", nil)
		}
		// A process alive when the stop came needs a start the Controller knows of:
		// without one the claim is refused, and nothing is orphaned or released by it.
		if e.WasRunning && run.StartedAt == nil && (run.HarnessRan == nil || !*run.HarnessRan) {
			return outcome(""), input.Invalid("was_running needs a Run that started")
		}
		ev.Type, ev.Message = domain.EventRunStopped, "run stopped"
		ev.Data["never_started"], ev.Data["was_running"] = e.NeverStarted, e.WasRunning
		// A runtime stopped already was confirmed so by a reconciliation: the
		// Worker's own stopped says the same and is taken as it is, with no
		// second RunStopped, so that the reports behind it are not held up.
		confirmed := run.Runtime == domain.RuntimeStopped
		noEvent = confirmed
		apply = func() error {
			if !confirmed {
				if err := tx.StopRuntime(ctx, run); err != nil {
					return err
				}
			}
			// A lost Run whose process was alive when its stop arrived is an orphan,
			// whether or not a reconciliation saw it first (§27.2).
			if e.WasRunning && run.State == domain.RunLost {
				if _, err := OrphanIn(ctx, tx, act, run, task, l); err != nil {
					return err
				}
			}
			run.Runtime = domain.RuntimeStopped
			return s.releaseIfAllowed(ctx, tx, act, run, l)
		}
	case e.Kind == v1.ReportPublicationFailed && run.Publication == domain.PublicationFailed:
		// The publication was failed already (by a reconciliation that found the
		// result lost): the Worker giving it up says the same.
		noEvent, apply = true, func() error { return nil }
	case e.Kind == v1.ReportSealed || e.Kind == v1.ReportPublished || e.Kind == v1.ReportPublicationFailed:
		if apply, err = s.publicationEntry(ctx, tx, act, &ev, run, task, l, e); err != nil {
			return outcome(""), err
		}
	default:
		return outcome(""), input.Invalid("unknown report kind %q", e.Kind)
	}

	if err := apply(); err != nil {
		return outcome(""), internalError{err}
	}
	if noEvent {
		return outcome(v1.ReportApplied), nil
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
func (s *Service) releaseIfAllowed(ctx context.Context, tx *store.Tx, act mutation.Act, run store.Run, l store.Lease) error {
	return ReleaseIfAllowedIn(ctx, tx, act, run, l)
}

// ReleaseIfAllowedIn releases the reservation of the lease in tx if §10.5
// allows it for run as it is now; it does nothing otherwise, or if it is released.
func ReleaseIfAllowedIn(ctx context.Context, tx *store.Tx, act mutation.Act, run store.Run, l store.Lease) error {
	if !releasable(run) {
		return nil
	}
	open, err := store.OpenReservation(ctx, tx, l.ID)
	if err != nil || !open {
		return err
	}
	return tx.ReleaseReservation(ctx, l.ID, releaseRunStopped, act.Now)
}

const dataReportDigest = "report_digest"

// Bounds of what a Worker may say: numbers that fit the database, and times that
// are not the other side of the world.
const (
	maxSeq   = 1 << 62
	maxAhead = 24 * time.Hour
)

var earliestAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

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

func validateReport(req v1.RunReportRequest, now time.Time) error {
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
		if e.WorkerSeq == 0 || e.WorkerSeq <= last || e.WorkerSeq > maxSeq {
			return input.Invalid("worker_seq must be positive and grow along the entries")
		}
		last = e.WorkerSeq
		if err := validateEntry(e, now); err != nil {
			return err
		}
	}
	return nil
}

func validateEntry(e v1.RunReportEntry, now time.Time) error {
	switch {
	case e.LeaseEpoch == 0 || e.LeaseEpoch > maxSeq:
		return input.Invalid("lease_epoch must be between 1 and %d", maxSeq)
	case !e.Kind.IsValid():
		return input.Invalid("kind must be started, exited, prepare_failed, stopped, sealed, published, publication_failed or orphan_preserved")
	case e.At.Before(earliestAt) || e.At.After(now.Add(maxAhead)):
		return input.Invalid("at must be after the year 2000 and at most %s ahead of the Controller", maxAhead)
	case len(e.Message) > v1.MaxReportMessageLen || !domain.Printable(e.Message):
		return input.Invalid("message must print and be at most %d characters", v1.MaxReportMessageLen)
	case e.Kind != v1.ReportSealed && e.Kind != v1.ReportPublished && e.Kind != v1.ReportOrphanPreserved && e.ResultCommit != "":
		return input.Invalid("only sealed, published and orphan_preserved carry a result commit")
	case e.Kind != v1.ReportSealed && (len(e.Excluded) > 0 || e.ExcludedTruncated):
		return input.Invalid("only sealed carries excluded paths")
	case e.Kind != v1.ReportPublicationFailed && e.Phase != "":
		return input.Invalid("only publication_failed carries a phase")
	case e.Kind != v1.ReportStarted && e.Handle != nil:
		return input.Invalid("only started carries a handle")
	case e.Kind != v1.ReportExited && (e.ExitCode != nil || e.Signal != "" || e.Cause != ""):
		return input.Invalid("only exited carries an exit code, a signal or a cause")
	case e.Kind != v1.ReportStopped && e.NeverStarted:
		return input.Invalid("only stopped says never_started")
	case e.WasRunning && (e.Kind != v1.ReportStopped || e.NeverStarted):
		return input.Invalid("only a stopped of a Run that started says was_running")
	}
	switch e.Kind {
	case v1.ReportStarted:
		h := e.Handle
		if h == nil || h.Runtime == "" || h.Ref == "" || len(h.Runtime)+len(h.Ref) > v1.MaxHandleLen ||
			strings.ContainsFunc(h.Runtime+h.Ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return input.Invalid("handle needs a runtime and a ref of at most %d bytes with no spaces", v1.MaxHandleLen)
		}
	case v1.ReportSealed, v1.ReportPublished, v1.ReportOrphanPreserved:
		if !domain.ValidCommitID(e.ResultCommit) {
			return input.Invalid("result_commit must be a full lower-case hex commit ID")
		}
		return validateExcluded(e)
	case v1.ReportPublicationFailed:
		if e.Phase != v1.PhaseSeal && e.Phase != v1.PhasePush {
			return input.Invalid("phase must be seal or push")
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
		if len(e.Signal) > 32 || !domain.Printable(e.Signal) {
			return input.Invalid("signal must print and be at most 32 characters")
		}
	}
	return nil
}
