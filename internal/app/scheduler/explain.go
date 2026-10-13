package scheduler

import (
	"context"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Explain answers what the scheduler makes of a Run (§10.6). A Run that holds
// a lease, or has held one that was accepted, shows that lease and its Worker,
// the real ones, and no verdicts: the decision was taken, and simulating
// another would contradict it. A queued Run without a lease is decided now,
// against the Workers as they are in one snapshot, and shows the verdict on each:
// the filters it fails or its score. Chosen is then a diagnosis of what the next
// pass would do, not a promise. Any other Run is not queued.
func (s *Service) Explain(ctx context.Context, ref string) (v1.RunExplanation, error) {
	var out v1.RunExplanation
	err := s.db.Read(ctx, func(q store.Querier) error {
		id, err := refs.Run(ctx, q, ref)
		if err != nil {
			return err
		}
		run, err := store.RunByID(ctx, q, id)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		out = v1.RunExplanation{
			RunID: string(run.ID), Short: domain.ShortID{Kind: domain.KindRun, N: run.Short}.String(), State: run.State,
			QueuedReason: run.QueuedReason, EvaluatedAt: now, Workers: []v1.WorkerVerdict{}, Decision: v1.DecisionNotQueued,
		}
		// The spec is read when the answer needs it. What it asks for is shown if it can be read; a
		// Run that holds a lease, or is not queued, is explained without it.
		placement, specErr := placementOf(run)
		out.Requested = v1.Resources{CPU: placement.Resources.CPU, MemoryMB: placement.Resources.MemoryMB}
		switch {
		case run.LeaseID != "":
			return s.explainLease(ctx, q, run, &out)
		case run.State != domain.RunQueued:
			return nil
		case specErr != nil:
			// A Run that cannot be read cannot be placed: it waits, and says why.
			out.Decision, out.Problem = v1.DecisionNoWorker, "the frozen spec of the Run cannot be read"
			return nil
		}
		return s.explainQueued(ctx, q, run, placement, now, &out)
	})
	return out, err
}

func (s *Service) explainLease(ctx context.Context, q store.Querier, run store.Run, out *v1.RunExplanation) error {
	l, err := store.LeaseByID(ctx, q, run.LeaseID)
	if err != nil {
		return err
	}
	w, err := store.WorkerByID(ctx, q, l.WorkerID)
	if err != nil {
		return err
	}
	view := leases.View(l)
	out.Decision, out.Chosen, out.Lease = v1.DecisionAssigned, w.Name, &view
	return nil
}

func (s *Service) explainQueued(ctx context.Context, q store.Querier, run store.Run, placement domain.Placement, now time.Time, out *v1.RunExplanation) error {
	task, err := store.TaskByID(ctx, q, run.TaskID)
	if err != nil {
		return err
	}
	decision, _, err := decide(ctx, q, run, task.ProjectID, placement, now, s.startedAt)
	if err != nil {
		return err
	}
	out.Decision = v1.DecisionNoWorker
	if decision.Chosen != nil {
		out.Decision, out.Chosen = v1.DecisionWouldAssign, decision.Chosen.Name
	}
	for _, v := range decision.Verdicts {
		out.Workers = append(out.Workers, verdictView(v, decision.Chosen != nil && decision.Chosen.ID == v.ID))
	}
	return nil
}

func verdictView(v Verdict, chosen bool) v1.WorkerVerdict {
	out := v1.WorkerVerdict{ID: string(v.ID), Name: v.Name, State: v.State, Eligible: v.Eligible(), Chosen: chosen, Failed: []v1.FilterResult{}}
	for _, f := range v.Failed {
		out.Failed = append(out.Failed, v1.FilterResult{Filter: f.Filter, Code: f.Code, Message: f.Message, Details: f.Details})
	}
	if v.Score != nil {
		out.Score = &v1.ScoreBreakdown{Total: v.Score.Total, Prefer: v.Score.Prefer, Free: v.Score.Free, Load: v.Score.Load, Affinity: v.Score.Affinity}
	}
	return out
}
