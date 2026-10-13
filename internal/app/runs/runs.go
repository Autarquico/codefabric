// Package runs is the use cases of Runs (§3.4): creating one with its logical
// configuration frozen (§3.3), cancelling the live ones of a Task, and reading.
package runs

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Rederive brings the materialised state of a Task up to date inside the
// transaction of whatever changed it (tasks.Rederive). It is injected because
// the tasks package imports this one.
type Rederive func(ctx context.Context, tx *store.Tx, act mutation.Act, task domain.ID) error

// Service runs the Run use cases.
type Service struct {
	db       *store.DB
	clock    clock.Clock
	ids      *domain.IDGenerator
	rederive Rederive
	relay    Relay
	tokenKey []byte
}

// Option adjusts New.
type Option func(*Service)

// WithRelay gives the service the git relay it verifies publications against.
// Without one, a `published` report cannot be verified and is retried.
func WithRelay(r Relay) Option { return func(s *Service) { s.relay = r } }

// New builds the service. The clock is the one the store was opened with. The
// key that signs relay tokens is drawn here and lives in memory only: a
// Controller that restarts invalidates the tokens, and the Workers ask for new ones.
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, rederive Rederive, opts ...Option) *Service {
	s := &Service{db: db, clock: clk, ids: ids, rederive: rederive, tokenKey: make([]byte, 32)}
	if _, err := rand.Read(s.tokenKey); err != nil {
		panic(fmt.Sprintf("runs: drawing the relay token key: %v", err)) // no caller can recover from a broken entropy source
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Source is what a Run starts from: the commit the client resolved and the ref
// it resolved it from, which travel together (ADR 0002 D1).
type Source struct{ Ref, Commit string }

// CreateIn queues a Run of the Task in tx, freezing what the Run will execute:
// the Task's configuration resolved against its Project and the defaults, and
// the source the client resolved.
//
// A Task that ended, by a marker or by its derived state, gets no more Runs.
//
// The first Run also freezes the Task's completion and retry policy (§3.3). A Project or Task
// that names a Workflow or an Environment cannot get a Run yet: resolving them
// to a version is Phase 2, and freezing the bare reference would pin nothing.
func (s *Service) CreateIn(ctx context.Context, tx *store.Tx, act mutation.Act, taskID domain.ID, source Source) (v1.Run, error) {
	commit := source.Commit
	if !domain.ValidCommitID(commit) {
		return v1.Run{}, input.Invalid("source_commit must be a full lower-case hex commit ID")
	}
	if err := input.Ref("resolved_ref", source.Ref); err != nil {
		return v1.Run{}, err
	}
	task, err := store.TaskByID(ctx, tx, taskID)
	if err != nil {
		return v1.Run{}, err
	}
	if ended(task) {
		return v1.Run{}, domain.NewError(domain.CodeInvalidTransition,
			fmt.Sprintf("task %s is %s: it cannot start Runs", task.Name, task.State), map[string]any{"state": string(task.State)})
	}
	project, err := store.ProjectByID(ctx, tx, task.ProjectID)
	if err != nil {
		return v1.Run{}, err
	}
	in := Inherit(task, project)
	if in.Workflow != "" || in.Environment != "" {
		return v1.Run{}, domain.NewError(domain.CodeNotImplemented,
			"Workflow and Environment references are not resolved yet: a Run cannot be created while the Task or its Project names one",
			map[string]any{"workflow": in.Workflow, "environment": in.Environment})
	}

	raw, digest, err := newSpec(in, task.SourceSnapshot, source).encode()
	if err != nil {
		return v1.Run{}, fmt.Errorf("runs: encoding the spec: %w", err)
	}
	previous, err := store.RunSummaries(ctx, tx, taskID)
	if err != nil {
		return v1.Run{}, err
	}
	short, err := tx.NextShortID(ctx, domain.KindRun)
	if err != nil {
		return v1.Run{}, err
	}
	run := store.Run{
		ID: s.ids.New(domain.KindRun), Short: short, TaskID: taskID, Attempt: len(previous) + 1,
		Lineage: domain.LineageInitial, Spec: raw, SpecDigest: digest, SourceCommit: commit,
		Branch: domain.BranchOf(task.Name, domain.ShortID{Kind: domain.KindTask, N: task.Short}, domain.ShortID{Kind: domain.KindRun, N: short}),
		State:  domain.RunQueued, Publication: domain.PublicationNone, Evaluation: domain.EvaluationNone,
		Runtime: domain.RuntimeNone, CreatedAt: act.Now,
	}
	if task.Completion == "" {
		task.Completion, task.Retry = in.Completion, &in.Retry
		if err := tx.UpdateTask(ctx, task); err != nil {
			return v1.Run{}, err
		}
	}
	if err := tx.InsertRun(ctx, run); err != nil {
		return v1.Run{}, err
	}
	ev := act.Event(domain.EventRunQueued, "run queued")
	ev.ProjectID, ev.TaskID, ev.RunID = task.ProjectID, taskID, run.ID
	ev.Data = map[string]any{"short_id": shortID(run.Short), "attempt": run.Attempt, "spec_digest": digest, "source_commit": commit}
	if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
		return v1.Run{}, err
	}
	if err := s.rederive(ctx, tx, act, taskID); err != nil {
		return v1.Run{}, err
	}
	return view(run, false), nil
}

// ended says whether the Task can no longer get Runs: it carries a marker
// that finishes it, or its derived state is terminal.
func ended(t store.Task) bool {
	return t.CompletedManuallyAt != nil || t.CancelledAt != nil || t.ArchivedAt != nil || t.State.IsTerminal()
}

// CancelLiveIn cancels the Runs of the Task that have not ended. A Run that
// waits for its Worker's answer to a lease loses the lease and its reservation
// in the same transaction (§10.5). One whose lease was accepted keeps both: its
// process may be starting, so in the same transaction its Worker is ordered to
// stop it (§10.7), and a Run whose session was created gets publication
// pending, since its worktree is to be sealed. The reservation is released by
// RunStopped and the sealing (§10.5).
func (s *Service) CancelLiveIn(ctx context.Context, tx *store.Tx, act mutation.Act, taskID domain.ID) error {
	task, err := store.TaskByID(ctx, tx, taskID)
	if err != nil {
		return err
	}
	all, err := store.Runs(ctx, tx, store.RunFilter{TaskID: taskID})
	if err != nil {
		return err
	}
	for _, run := range all {
		if run.State.IsTerminal() {
			continue
		}
		next, err := domain.NextRunState(domain.RunStatus{State: run.State}, domain.RunEventCancel)
		if err != nil {
			return err
		}
		if err := leases.InvalidateIn(ctx, tx, act, &run); err != nil {
			return err
		}
		if err := tx.FinishRun(ctx, run, next.State, "", act.Now); errors.Is(err, store.ErrNoRowChanged) {
			return domain.NewError(domain.CodeInvalidTransition, "the Run changed state meanwhile", map[string]any{"run": string(run.ID)})
		} else if err != nil {
			return err
		}
		from := run.State
		run.State = next.State
		if run.LeaseState == domain.LeaseAccepted {
			if err := EndByControllerIn(ctx, tx, act, &run, task, domain.StopCancel); err != nil {
				return err
			}
		}
		ev := act.Event(domain.EventRunCancelled, "run cancelled")
		ev.ProjectID, ev.TaskID, ev.RunID = task.ProjectID, taskID, run.ID
		ev.Data = map[string]any{"short_id": shortID(run.Short), "from": string(from), "publication": string(run.Publication)}
		if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
			return err
		}
		if err := s.rederive(ctx, tx, act, taskID); err != nil {
			return err
		}
	}
	return nil
}

// ListFilter selects Runs by the references of their Task and Project.
type ListFilter struct{ Task, Project string }

// List returns the Runs that match the filter, oldest first.
func (s *Service) List(ctx context.Context, f ListFilter) ([]v1.Run, error) {
	var out []v1.Run
	err := s.db.Read(ctx, func(q store.Querier) error {
		var filter store.RunFilter
		scope, err := refs.Scope(ctx, q, f.Project)
		if err != nil {
			return err
		}
		filter.ProjectID = scope
		if f.Task != "" {
			if filter.TaskID, err = refs.Task(ctx, q, f.Task, scope); err != nil {
				return err
			}
		}
		rows, err := store.Runs(ctx, q, filter)
		reach := reachCache{}
		for _, r := range rows {
			v, verr := s.viewWith(ctx, q, r, false, reach)
			if verr != nil {
				return verr
			}
			out = append(out, v)
		}
		return err
	})
	return out, err
}

// Get returns the Run a reference points to, with its frozen spec.
func (s *Service) Get(ctx context.Context, ref string) (v1.Run, error) {
	var out v1.Run
	err := s.db.Read(ctx, func(q store.Querier) error {
		id, err := refs.Run(ctx, q, ref)
		if err != nil {
			return err
		}
		r, err := store.RunByID(ctx, q, id)
		if err != nil {
			return err
		}
		out, err = s.viewOf(ctx, q, r, true)
		return err
	})
	return out, err
}

func shortID(n uint64) string { return domain.ShortID{Kind: domain.KindRun, N: n}.String() }

// viewOf is view with the branch of a Run that has none stored, which are those
// created before it was: the same rule that froze it for the others.
func (s *Service) viewOf(ctx context.Context, q store.Querier, r store.Run, withSpec bool) (v1.Run, error) {
	return s.viewWith(ctx, q, r, withSpec, reachCache{})
}

// reachCache remembers whether the Workers a listing meets are reachable.
type reachCache map[domain.ID]bool

func (c reachCache) of(ctx context.Context, q store.Querier, id domain.ID) (bool, error) {
	if reach, ok := c[id]; ok {
		return reach, nil
	}
	w, err := store.WorkerByID(ctx, q, id)
	if err != nil {
		return false, err
	}
	c[id] = w.State.Reachable()
	return c[id], nil
}

func (s *Service) viewWith(ctx context.Context, q store.Querier, r store.Run, withSpec bool, reach reachCache) (v1.Run, error) {
	if r.Branch == "" {
		task, err := store.TaskByID(ctx, q, r.TaskID)
		if err != nil {
			return v1.Run{}, err
		}
		r.Branch = branchOf(task, r)
	}
	out := view(r, withSpec)
	if r.WorkerID != "" {
		reachable, err := reach.of(ctx, q, r.WorkerID)
		if err != nil {
			return v1.Run{}, err
		}
		out.WorkerReachable = &reachable
	}
	return out, nil
}

// specMode is the mode a Run froze (§7.4), "" if its spec says none.
func specMode(spec []byte) domain.Mode {
	var doc struct {
		Mode domain.Mode `json:"mode"`
	}
	if json.Unmarshal(spec, &doc) != nil {
		return ""
	}
	return doc.Mode
}

// branchOf is the branch of a Run: the frozen one, or the one the rule gives.
func branchOf(task store.Task, r store.Run) string {
	return domain.RunBranch(r.Branch, task.Name, domain.ShortID{Kind: domain.KindTask, N: task.Short}, domain.ShortID{Kind: domain.KindRun, N: r.Short})
}

func view(r store.Run, withSpec bool) v1.Run {
	out := v1.Run{
		ID: string(r.ID), Short: shortID(r.Short), TaskID: string(r.TaskID), Attempt: r.Attempt,
		State: r.State, EndReason: r.EndReason, ExitCode: r.ExitCode, QueuedReason: r.QueuedReason, Lineage: r.Lineage,
		LineageParent: string(r.LineageParent),
		SpecDigest:    r.SpecDigest, SourceCommit: r.SourceCommit, Publication: r.Publication,
		Evaluation: r.Evaluation, Runtime: r.Runtime, Mode: specMode(r.Spec), Promoted: r.Promoted, WorkerID: string(r.WorkerID),
		CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		Branch: r.Branch, ResultCommit: r.ResultCommit, ResultExcluded: r.ResultExcluded, ResultExcludedTruncated: r.ResultExcludedTruncated,
	}
	if r.LeaseID != "" {
		out.Lease = &v1.RunLease{ID: string(r.LeaseID), Epoch: r.LeaseEpoch, State: r.LeaseState}
	}
	if withSpec {
		out.Spec = json.RawMessage(r.Spec)
	}
	return out
}
