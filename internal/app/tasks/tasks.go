// Package tasks is the use cases of Tasks (§3.3, §11): creating them, changing
// their markers and keeping their materialised state derived from their Runs.
package tasks

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const (
	maxPromptBytes = 32 << 10
	maxDerivedName = 40
)

// Service runs the Task use cases.
type Service struct {
	db    *store.DB
	clock clock.Clock
	ids   *domain.IDGenerator
	runs  *runs.Service
}

// New builds the service. runs must have been built with Rederive.
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, runs *runs.Service) *Service {
	return &Service{db: db, clock: clk, ids: ids, runs: runs}
}

// Create makes a Task in a Project and, unless req.NoStart, its first Run.
// Without a name it takes one from the prompt, made unique if need be; a name
// given is never changed, and one in use is refused.
func (s *Service) Create(ctx context.Context, by pki.Session, req v1.CreateTaskRequest) (v1.CreateTaskResponse, error) {
	if err := validateNew(req); err != nil {
		return v1.CreateTaskResponse{}, err
	}
	// Only what the user asked for identifies the call: the pair the client
	// resolved from the remote is no input, so a replay does not depend on it.
	explicit := req
	explicit.ResolvedRef, explicit.SourceCommit = "", ""
	return mutation.Do(ctx, s.db, s.clock, by, "task.create", req.RequestID, explicit,
		func(tx *store.Tx, act mutation.Act) (v1.CreateTaskResponse, error) {
			if err := requireSource(req); err != nil {
				return v1.CreateTaskResponse{}, err
			}
			projectID, err := refs.Project(ctx, tx, req.Project)
			if err != nil {
				return v1.CreateTaskResponse{}, err
			}
			project, err := store.ProjectByID(ctx, tx, projectID)
			if err != nil {
				return v1.CreateTaskResponse{}, err
			}
			name, err := freeName(ctx, tx, projectID, req)
			if err != nil {
				return v1.CreateTaskResponse{}, err
			}
			short, err := tx.NextShortID(ctx, domain.KindTask)
			if err != nil {
				return v1.CreateTaskResponse{}, err
			}
			task := store.Task{
				ID: s.ids.New(domain.KindTask), Short: short, ProjectID: projectID, Name: name, Prompt: req.Prompt,
				Autostart: !req.NoStart, SourceRef: req.SourceRef, Mode: req.Mode, Workflow: req.Workflow,
				Environment: req.Environment, Requirements: req.Requirements, CompletionOverride: req.Completion,
				RetryOverride: req.Retry, CreatedBy: creator(by), CreatedAt: act.Now,
			}
			if task.State, task.WaitingReason, err = derive(task, project, nil); err != nil {
				return v1.CreateTaskResponse{}, err
			}
			if err := tx.InsertTask(ctx, task); err != nil {
				return v1.CreateTaskResponse{}, err
			}
			ev := act.Event(domain.EventTaskCreated, "task created")
			ev.ProjectID, ev.TaskID = projectID, task.ID
			ev.Data = map[string]any{
				"short_id": shortID(short), "name": name, "autostart": task.Autostart,
				"state": string(task.State), "waiting_reason": string(task.WaitingReason),
			}
			if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
				return v1.CreateTaskResponse{}, err
			}

			var out v1.CreateTaskResponse
			if task.Autostart {
				run, err := s.runs.CreateIn(ctx, tx, act, task.ID, runs.Source{Ref: req.ResolvedRef, Commit: req.SourceCommit})
				if err != nil {
					return out, err
				}
				out.Run = &run
			}
			out.Task, err = viewOf(ctx, tx, task.ID)
			return out, err
		})
}

// Cancel marks the Task cancelled and cancels its live Runs, in one transaction.
// Cancelling a cancelled Task changes nothing; a Task that ended cannot be cancelled.
func (s *Service) Cancel(ctx context.Context, by pki.Session, ref, project string, req v1.ActionRequest) (v1.Task, error) {
	return s.change(ctx, by, "task.cancel", ref, project, req.RequestID, req, func(tx *store.Tx, act mutation.Act, t *store.Task) error {
		if t.CancelledAt != nil {
			return nil
		}
		if t.ArchivedAt != nil || t.State.IsTerminal() {
			return notAllowed(*t, "cancelled")
		}
		t.CancelledAt = &act.Now
		if err := tx.UpdateTask(ctx, *t); err != nil {
			return err
		}
		if err := s.runs.CancelLiveIn(ctx, tx, act, t.ID); err != nil {
			return err
		}
		return audit(ctx, tx, act, "task.cancelled", *t, nil, nil)
	})
}

// Complete marks the Task completed by hand (§11.7, the manual policy), which
// also cancels its live Runs: a finished Task has none.
func (s *Service) Complete(ctx context.Context, by pki.Session, ref, project string, req v1.ActionRequest) (v1.Task, error) {
	return s.change(ctx, by, "task.complete", ref, project, req.RequestID, req, func(tx *store.Tx, act mutation.Act, t *store.Task) error {
		if t.CompletedManuallyAt != nil || t.State == domain.TaskCompleted {
			return nil
		}
		if t.ArchivedAt != nil || t.CancelledAt != nil {
			return notAllowed(*t, "completed")
		}
		t.CompletedManuallyAt = &act.Now
		if err := tx.UpdateTask(ctx, *t); err != nil {
			return err
		}
		if err := s.runs.CancelLiveIn(ctx, tx, act, t.ID); err != nil {
			return err
		}
		return audit(ctx, tx, act, "task.completed", *t, nil, nil)
	})
}

// Archive marks the Task archived, which frees its name. Archiving an archived
// Task changes nothing; a Task with live Runs must be cancelled first.
func (s *Service) Archive(ctx context.Context, by pki.Session, ref, project string, req v1.ActionRequest) (v1.Task, error) {
	return s.change(ctx, by, "task.archive", ref, project, req.RequestID, req, func(tx *store.Tx, act mutation.Act, t *store.Task) error {
		if t.ArchivedAt != nil {
			return nil
		}
		live, err := liveRuns(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if live > 0 {
			return domain.NewError(domain.CodeInvalidTransition,
				fmt.Sprintf("task %s has %d live Run(s): cancel it before archiving", t.Name, live), map[string]any{"live_runs": live})
		}
		t.ArchivedAt = &act.Now
		if err := tx.UpdateTask(ctx, *t); err != nil {
			return err
		}
		return audit(ctx, tx, act, "task.archived", *t, nil, nil)
	})
}

// Set renames the Task or changes its completion or retry policy. A policy
// given is explicit: it replaces the one frozen with the first Run (§3.3).
func (s *Service) Set(ctx context.Context, by pki.Session, ref, project string, req v1.UpdateTaskRequest) (v1.Task, error) {
	if err := validateUpdate(req); err != nil {
		return v1.Task{}, err
	}
	return s.change(ctx, by, "task.set", ref, project, req.RequestID, req,
		func(tx *store.Tx, act mutation.Act, t *store.Task) error {
			before := map[string]any{"name": t.Name, "completion": string(t.Completion), "completion_override": string(t.CompletionOverride), "retry": t.Retry, "retry_override": t.RetryOverride}
			if req.Name != nil {
				if t.ArchivedAt == nil {
					if taken, err := store.TaskNameTaken(ctx, tx, t.ProjectID, *req.Name, t.ID); err != nil || taken {
						if err != nil {
							return err
						}
						return input.Invalid("a Task named %q already exists in the Project", *req.Name)
					}
				}
				t.Name = *req.Name
			}
			if req.Completion != nil {
				t.CompletionOverride = *req.Completion
				if t.Completion != "" {
					t.Completion = *req.Completion
				}
			}
			if req.Retry != nil {
				t.RetryOverride = req.Retry
				if t.Retry != nil {
					t.Retry = req.Retry
				}
			}
			if err := tx.UpdateTask(ctx, *t); err != nil {
				return err
			}
			after := map[string]any{"name": t.Name, "completion": string(t.Completion), "completion_override": string(t.CompletionOverride), "retry": t.Retry, "retry_override": t.RetryOverride}
			return audit(ctx, tx, act, "task.updated", *t, before, after)
		})
}

// change is the shape of every operation on an existing Task: in the one
// transaction, resolve it, apply the change, derive its state again and
// answer with the Task as it is then. The request, with the reference,
// identifies the call for idempotency.
func (s *Service) change(ctx context.Context, by pki.Session, operation, ref, project, requestID string, request any,
	apply func(*store.Tx, mutation.Act, *store.Task) error,
) (v1.Task, error) {
	return mutation.Do(ctx, s.db, s.clock, by, operation, requestID,
		struct {
			Ref, Project string
			Request      any
		}{ref, project, request},
		func(tx *store.Tx, act mutation.Act) (v1.Task, error) {
			id, err := resolveIn(ctx, tx, ref, project)
			if err != nil {
				return v1.Task{}, err
			}
			task, err := store.TaskByID(ctx, tx, id)
			if err != nil {
				return v1.Task{}, err
			}
			if err := apply(tx, act, &task); err != nil {
				return v1.Task{}, err
			}
			if err := Rederive(ctx, tx, act, id); err != nil {
				return v1.Task{}, err
			}
			return viewOf(ctx, tx, id)
		})
}

// ListFilter selects Tasks. Archived ones show only with All or an explicit state.
type ListFilter struct {
	Project string
	States  []domain.TaskState
	All     bool
}

// List returns the Tasks that match the filter, oldest first.
func (s *Service) List(ctx context.Context, f ListFilter) ([]v1.Task, error) {
	var out []v1.Task
	err := s.db.Read(ctx, func(q store.Querier) error {
		scope, err := refs.Scope(ctx, q, f.Project)
		if err != nil {
			return err
		}
		rows, err := store.Tasks(ctx, q, store.TaskFilter{ProjectID: scope, States: f.States, IncludeArchived: f.All})
		if err != nil {
			return err
		}
		projects := map[domain.ID]store.Project{}
		for _, t := range rows {
			p, ok := projects[t.ProjectID]
			if !ok {
				if p, err = store.ProjectByID(ctx, q, t.ProjectID); err != nil {
					return err
				}
				projects[t.ProjectID] = p
			}
			out = append(out, View(t, p))
		}
		return nil
	})
	return out, err
}

// Get returns the Task a reference points to; project narrows an ambiguous name.
func (s *Service) Get(ctx context.Context, ref, project string) (v1.Task, error) {
	var out v1.Task
	err := s.db.Read(ctx, func(q store.Querier) error {
		id, err := resolveIn(ctx, q, ref, project)
		if err != nil {
			return err
		}
		out, err = viewOf(ctx, q, id)
		return err
	})
	return out, err
}

func resolveIn(ctx context.Context, q store.Querier, ref, project string) (domain.ID, error) {
	scope, err := refs.Scope(ctx, q, project)
	if err != nil {
		return "", err
	}
	return refs.Task(ctx, q, ref, scope)
}

func viewOf(ctx context.Context, q store.Querier, id domain.ID) (v1.Task, error) {
	t, err := store.TaskByID(ctx, q, id)
	if err != nil {
		return v1.Task{}, err
	}
	p, err := store.ProjectByID(ctx, q, t.ProjectID)
	if err != nil {
		return v1.Task{}, err
	}
	return View(t, p), nil
}

func liveRuns(ctx context.Context, q store.Querier, task domain.ID) (int, error) {
	all, err := store.Runs(ctx, q, store.RunFilter{TaskID: task})
	live := 0
	for _, r := range all {
		if !r.State.IsTerminal() {
			live++
		}
	}
	return live, err
}

func notAllowed(t store.Task, to string) error {
	return domain.NewError(domain.CodeInvalidTransition,
		fmt.Sprintf("task %s is %s: it cannot be %s", t.Name, t.State, to), map[string]any{"state": string(t.State)})
}

func creator(by pki.Session) domain.Creator {
	if by.Principal.ActorKind() == domain.ActorHuman {
		return domain.CreatorHuman
	}
	return domain.CreatorAutomation
}

func shortID(n uint64) string { return domain.ShortID{Kind: domain.KindTask, N: n}.String() }

func audit(ctx context.Context, tx *store.Tx, act mutation.Act, action string, t store.Task, before, after any) error {
	return tx.AppendAudit(ctx, store.AuditEntry{
		Time: act.Now, ActorKind: act.Actor.Kind, ActorID: act.Actor.ID, Action: action,
		ResourceKind: "task", ResourceID: string(t.ID), Before: before, After: after, Result: "ok",
	})
}

// freeName is the name the new Task gets: the one asked for if it is free, or
// else one derived from the prompt, with -2, -3... to make it so.
func freeName(ctx context.Context, q store.Querier, project domain.ID, req v1.CreateTaskRequest) (string, error) {
	if req.Name != "" {
		taken, err := store.TaskNameTaken(ctx, q, project, req.Name, "")
		if err == nil && taken {
			err = input.Invalid("a Task named %q already exists in the Project", req.Name)
		}
		return req.Name, err
	}
	base := nameFromPrompt(req.Prompt)
	for n := 1; ; n++ {
		name, ok := candidate(base, n)
		if !ok {
			continue
		}
		taken, err := store.TaskNameTaken(ctx, q, project, name, "")
		if err != nil || !taken {
			return name, err
		}
	}
}

// candidate is the nth try at a name: base, base-2, base-3... A suffix must
// never make the name read as an ID, and "task-2" reads as a short ID: that
// one becomes "task-n2". ok is false if neither spelling is a valid name.
func candidate(base string, n int) (name string, ok bool) {
	if n == 1 {
		return base, true
	}
	for _, format := range []string{"%s-%d", "%s-n%d"} {
		if name = fmt.Sprintf(format, base, n); input.Name("name", name) == nil {
			return name, true
		}
	}
	return "", false
}

// nameFromPrompt makes a slug from the first words of the prompt.
func nameFromPrompt(prompt string) string {
	var b strings.Builder
	dash := true // no leading dash
	for _, r := range strings.ToLower(prompt) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= maxDerivedName {
			break
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "task"
	}
	if input.Name("name", name) != nil { // it reads as an ID
		name = "task-" + name
	}
	return name
}

func validateNew(req v1.CreateTaskRequest) error {
	var name error
	if req.Name != "" {
		name = input.Name("name", req.Name)
	}
	var ref, resolved error
	if req.SourceRef != "" {
		ref = input.Ref("source_ref", req.SourceRef)
	}
	if req.ResolvedRef != "" {
		resolved = input.Ref("resolved_ref", req.ResolvedRef)
	}
	return input.First(
		name, ref, resolved, validatePrompt(req.Prompt), input.Mode(req.Mode), input.Completion(req.Completion),
		input.Label("workflow", req.Workflow), input.Label("environment", req.Environment),
		input.Requirements(req.Requirements), input.Retry(req.Retry), validateStart(req),
	)
}

func validatePrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" || len(prompt) > maxPromptBytes {
		return input.Invalid("prompt must not be empty and at most %d bytes", maxPromptBytes)
	}
	return nil
}

// validateStart: a Task that does not start has nothing to freeze, so sending
// the pair of the client is a mistake. That a Task that starts has one is
// checked later (requireSource), inside the transaction.
func validateStart(req v1.CreateTaskRequest) error {
	if req.NoStart && (req.SourceCommit != "" || req.ResolvedRef != "") {
		return input.Invalid("source_commit and resolved_ref are for Tasks that start: the Run freezes them")
	}
	return nil
}

// requireSource: a Task that starts needs the commit the client resolved and
// the ref it resolved it from (D1). It is checked after the saved response of a
// repeated request_id was looked up, which needs neither.
func requireSource(req v1.CreateTaskRequest) error {
	if !req.NoStart && (req.SourceCommit == "" || req.ResolvedRef == "") {
		return input.Invalid("source commit required: the client resolves source_commit and resolved_ref from the source ref")
	}
	return nil
}

func validateUpdate(req v1.UpdateTaskRequest) error {
	if req.Name == nil && req.Completion == nil && req.Retry == nil {
		return input.Invalid("nothing to change: send a name, a completion or a retry policy")
	}
	var name error
	if req.Name != nil {
		name = input.Name("name", *req.Name)
	}
	var completion error
	if req.Completion != nil {
		completion = input.Completion(*req.Completion)
		if *req.Completion == "" {
			completion = input.Invalid("completion must be first_success, promote or manual")
		}
	}
	return input.First(name, completion, input.Retry(req.Retry))
}
