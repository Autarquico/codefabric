// Package inbox is what needs a person (§19.5). In this phase that is the
// requests for input of Runs and the Tasks that failed and wait for a decision.
// Approvals, workflow conflicts and budget warnings are deferred: nothing
// produces them yet.
package inbox

import (
	"context"
	"slices"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Service reads the inbox.
type Service struct{ db *store.DB }

// New builds the service.
func New(db *store.DB) *Service { return &Service{db: db} }

// List returns the items that need a person, the oldest first:
//
//   - needs_input: one per prompt of a Run in waiting_input that nobody has
//     answered, of a Task that is neither archived nor cancelled;
//   - failed: a Task, not archived, whose last Run failed or was lost and
//     whose state says a person must decide: failed (the retry policy gave up,
//     or never retried) or waiting for a decision (the policy is to ask).
func (s *Service) List(ctx context.Context) (v1.InboxResponse, error) {
	out := v1.InboxResponse{Items: []v1.InboxItem{}}
	err := s.db.Read(ctx, func(q store.Querier) error {
		asked, err := needsInput(ctx, q)
		if err != nil {
			return err
		}
		failed, err := failedTasks(ctx, q)
		if err != nil {
			return err
		}
		out.Items = append(asked, failed...)
		slices.SortStableFunc(out.Items, func(a, b v1.InboxItem) int { return a.Since.Compare(b.Since) })
		return nil
	})
	return out, err
}

func needsInput(ctx context.Context, q store.Querier) ([]v1.InboxItem, error) {
	waiting, err := store.RunsWaitingInput(ctx, q)
	if err != nil {
		return nil, err
	}
	var items []v1.InboxItem
	for _, run := range waiting {
		prompts, err := store.OpenPrompts(ctx, q, run.ID, run.LeaseEpoch)
		if err != nil {
			return nil, err
		}
		task, err := store.TaskByID(ctx, q, run.TaskID)
		if err != nil {
			return nil, err
		}
		can, harness, err := canRespond(ctx, q, run)
		if err != nil {
			return nil, err
		}
		for _, p := range prompts {
			if p.State != domain.PromptPending {
				continue
			}
			it := item(v1.InboxNeedsInput, task, run, p.RequestedAt)
			it.Run.Harness, it.Run.Respondable = harness, can
			it.Prompt = &v1.InboxPrompt{ID: p.PromptID, Kind: p.Kind, Summary: p.Summary, Options: p.Options}
			items = append(items, it)
		}
	}
	return items, nil
}

func failedTasks(ctx context.Context, q store.Querier) ([]v1.InboxItem, error) {
	tasks, err := store.Tasks(ctx, q, store.TaskFilter{States: []domain.TaskState{domain.TaskFailed, domain.TaskWaiting}})
	if err != nil {
		return nil, err
	}
	var items []v1.InboxItem
	for _, task := range tasks {
		if task.State == domain.TaskWaiting && task.WaitingReason != domain.WaitingDecision {
			continue
		}
		all, err := store.Runs(ctx, q, store.RunFilter{TaskID: task.ID})
		if err != nil {
			return nil, err
		}
		if len(all) == 0 {
			continue
		}
		last := all[len(all)-1]
		if last.State != domain.RunFailed && last.State != domain.RunLost {
			continue
		}
		since := last.CreatedAt
		if last.FinishedAt != nil {
			since = *last.FinishedAt
		}
		it := item(v1.InboxFailed, task, last, since)
		it.EndReason, it.FailureCode = last.EndReason, domain.Code(last.FailureCode)
		items = append(items, it)
	}
	return items, nil
}

func item(kind string, task store.Task, run store.Run, since time.Time) v1.InboxItem {
	return v1.InboxItem{
		Kind: kind, Since: since,
		Task: v1.InboxTask{
			ID: string(task.ID), Short: domain.ShortID{Kind: domain.KindTask, N: task.Short}.String(),
			Name: task.Name, Project: string(task.ProjectID),
		},
		Run: v1.InboxRun{
			ID: string(run.ID), Short: domain.ShortID{Kind: domain.KindRun, N: run.Short}.String(), State: run.State,
		},
	}
}

// canRespond says whether the Worker of the Run announces structured input for
// its harness, and which harness that is.
func canRespond(ctx context.Context, q store.Querier, run store.Run) (bool, string, error) {
	placement, err := runs.PlacementOf(run.Spec)
	if err != nil {
		return false, "", err
	}
	if run.WorkerID == "" {
		return false, placement.Harness, nil
	}
	w, err := store.WorkerByID(ctx, q, run.WorkerID)
	if err != nil {
		return false, "", err
	}
	return domain.HasStructuredInput(w.Report.Capabilities, placement.Harness), placement.Harness, nil
}
