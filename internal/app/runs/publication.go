package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Relay is the git relay as the publication of a Run sees it: it can say where
// a branch of a Project stands. *relay.Relay implements it.
type Relay interface {
	BranchTip(ctx context.Context, project domain.ID, branch string) (oid string, found bool, err error)
}

// maxVerificationsPerReport bounds the rev-parse calls one report makes inside
// its transaction: each is a child process that the single writer waits for.
// The `published` entries past it are not applied and are sent again.
const maxVerificationsPerReport = 8

// verifyBudget is what is left of them in one report.
type verifyBudget int

// publicationEntry decides the entries that tell of the publication of a result
// (§22.6): sealed, published and publication_failed. Everything that can reject
// one comes before the closure it returns, which is what writes. early is set
// instead when the entry is not for this request.
func (s *Service) publicationEntry(ctx context.Context, tx *store.Tx, act mutation.Act, ev *domain.Event,
	run store.Run, task store.Task, l store.Lease, e v1.RunReportEntry, budget *verifyBudget,
) (apply func() error, early *v1.RunReportOutcome, err error) {
	if run.Publication != domain.PublicationPending {
		return nil, nil, domain.NewError(domain.CodeInvalidTransition,
			fmt.Sprintf("the publication of this Run is %s, not pending", run.Publication), nil)
	}
	switch e.Kind {
	case v1.ReportSealed:
		if run.StartedAt == nil {
			return nil, nil, domain.NewError(domain.CodeInvalidTransition, "the harness of this Run never started: there is nothing to seal", nil)
		}
		if run.ResultCommit != "" && run.ResultCommit != e.ResultCommit {
			return nil, nil, domain.NewError(domain.CodeInvalidTransition, "this Run was sealed to another commit: a result never changes", nil)
		}
		ev.Type, ev.Message = domain.EventRunSealed, "run sealed"
		ev.Data["result_commit"], ev.Data["excluded"], ev.Data["excluded_truncated"] = e.ResultCommit, len(e.Excluded), e.ExcludedTruncated
		return func() error {
			if run.ResultCommit == "" {
				if err := tx.SealRun(ctx, run, e.ResultCommit, e.Excluded, e.ExcludedTruncated); err != nil {
					return err
				}
			}
			run.ResultCommit = e.ResultCommit
			return s.releaseIfAllowed(ctx, tx, act, run, l)
		}, nil, nil

	case v1.ReportPublished:
		if run.ResultCommit != e.ResultCommit {
			return nil, nil, domain.NewError(domain.CodeInvalidRequest, "result_commit is not the commit this Run was sealed to", nil)
		}
		if *budget <= 0 {
			return nil, &v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: v1.ReportNotApplied}, nil
		}
		*budget--
		if s.relay == nil {
			return nil, nil, internalError{errors.New("runs: there is no git relay to verify a publication against")}
		}
		branch := branchOf(task, run)
		tip, found, err := s.relay.BranchTip(ctx, task.ProjectID, branch)
		if err != nil {
			return nil, nil, internalError{fmt.Errorf("runs: verifying a publication: %w", err)}
		}
		if found && tip == run.ResultCommit {
			ev.Type, ev.Message = domain.EventRunPublished, "run published"
			ev.Data["result_commit"], ev.Data["branch"] = run.ResultCommit, branch
			return func() error {
				if err := tx.PublishRun(ctx, run, run.ResultCommit); err != nil {
					return err
				}
				run.Publication = domain.PublicationPublished
				return s.releaseIfAllowed(ctx, tx, act, run, l)
			}, nil, nil
		}
		// The Worker says it pushed and the relay does not have it: the publication failed.
		// It is applied, not rejected: a rejection would fence the Run.
		ev.Type, ev.Level, ev.Message = domain.EventRunPublicationFailed, "warn", "run publication failed: the relay does not hold the result"
		ev.Data["phase"], ev.Data["result_commit"], ev.Data["branch"] = v1.PhaseVerify, run.ResultCommit, branch
		return s.failPublication(ctx, tx, act, run, l), nil, nil

	default: // publication_failed
		if (e.Phase == v1.PhaseSeal) != (run.ResultCommit == "") {
			return nil, nil, domain.NewError(domain.CodeInvalidTransition,
				fmt.Sprintf("a %s failure does not fit a Run whose result commit is %q", e.Phase, run.ResultCommit), nil)
		}
		ev.Type, ev.Level, ev.Message = domain.EventRunPublicationFailed, "warn", "run publication failed"
		ev.Data["phase"] = e.Phase
		return s.failPublication(ctx, tx, act, run, l), nil, nil
	}
}

func (s *Service) failPublication(ctx context.Context, tx *store.Tx, act mutation.Act, run store.Run, l store.Lease) func() error {
	return func() error {
		if err := tx.FailPublication(ctx, run); err != nil {
			return err
		}
		run.Publication = domain.PublicationFailed
		return s.releaseIfAllowed(ctx, tx, act, run, l)
	}
}

// validateExcluded checks the paths a sealing left out: a short, sorted list
// of printable paths. The Worker cuts it before it sends it.
func validateExcluded(e v1.RunReportEntry) error {
	if len(e.Excluded) > v1.MaxExcludedPaths {
		return input.Invalid("excluded has at most %d paths", v1.MaxExcludedPaths)
	}
	bytes := 0
	for _, p := range e.Excluded {
		bytes += len(p)
		if p == "" || hasHidden(p) {
			return input.Invalid("excluded paths must print")
		}
	}
	if bytes > v1.MaxExcludedBytes {
		return input.Invalid("excluded is at most %d bytes", v1.MaxExcludedBytes)
	}
	if !slices.IsSorted(e.Excluded) {
		return input.Invalid("excluded must be sorted")
	}
	return nil
}

// Publish is `cf run publish`: another try at publishing the result of a Run
// whose publication failed (§22.6). The publication goes back to pending and the
// Worker that holds the result is told to publish it again, without
// reserving anything: a pending publication does not hold the reservation
// (§10.5). A second request with the same request_id answers what the first did.
func (s *Service) Publish(ctx context.Context, by pki.Session, ref string, req v1.ActionRequest) (v1.Run, error) {
	return mutation.Do(ctx, s.db, s.clock, by, "run.publish", req.RequestID, struct {
		Ref string `json:"ref"`
	}{ref}, func(tx *store.Tx, act mutation.Act) (v1.Run, error) {
		id, err := refs.Run(ctx, tx, ref)
		if err != nil {
			return v1.Run{}, err
		}
		run, err := store.RunByID(ctx, tx, id)
		if err != nil {
			return v1.Run{}, err
		}
		if !run.State.IsTerminal() || run.Publication != domain.PublicationFailed {
			return v1.Run{}, domain.NewError(domain.CodeInvalidTransition,
				fmt.Sprintf("only a finished Run whose publication failed can be published again (state %s, publication %s)", run.State, run.Publication),
				map[string]any{"state": string(run.State), "publication": string(run.Publication)})
		}
		w, err := store.WorkerByID(ctx, tx, run.WorkerID)
		if err != nil {
			return v1.Run{}, err
		}
		if w.State == domain.WorkerRemoved {
			return v1.Run{}, domain.NewError(domain.CodeInvalidTransition, "the Worker that holds the result was removed", nil)
		}
		l, err := store.LeaseByRunEpoch(ctx, tx, run.ID, run.LeaseEpoch)
		if err != nil {
			return v1.Run{}, err
		}
		task, err := store.TaskByID(ctx, tx, run.TaskID)
		if err != nil {
			return v1.Run{}, err
		}
		if err := tx.RetryPublication(ctx, run); err != nil {
			return v1.Run{}, err
		}
		payload, err := orderJSON(v1.PublishCommand{RunID: string(run.ID), LeaseID: string(l.ID), Epoch: l.Epoch})
		if err != nil {
			return v1.Run{}, err
		}
		if _, err := tx.EnqueueCommand(ctx, store.WorkerCommand{
			WorkerID: w.ID, Type: domain.CommandPublish, RunID: run.ID, LeaseID: l.ID, Epoch: l.Epoch,
			Payload: payload, CreatedAt: act.Now,
		}); err != nil {
			return v1.Run{}, err
		}
		ev := act.Event(domain.EventRunPublicationRequested, "run publication requested")
		ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = task.ProjectID, task.ID, run.ID, w.ID
		ev.Data = map[string]any{"short_id": shortID(run.Short), "result_commit": run.ResultCommit, "lease_epoch": l.Epoch}
		if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
			return v1.Run{}, err
		}
		if err := s.rederive(ctx, tx, act, task.ID); err != nil {
			return v1.Run{}, err
		}
		run.Publication = domain.PublicationPending
		return s.viewOf(ctx, tx, run, false)
	})
}

func orderJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("runs: encoding an order: %w", err)
	}
	return raw, nil
}
