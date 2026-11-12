package tasks

import (
	"context"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// seedOf checks the session a client wants a Run to start from (lineage.native_seed) and
// gives it as the domain has it; nil if there is none. The Project must keep native state
// (the session may hold what the harness was shown), and the blob must be one that a
// client uploaded for this Project: it is not enough to know a digest.
func seedOf(ctx context.Context, q store.Querier, project store.Project, ref *v1.NativeStateRef) (*domain.NativeState, error) {
	if ref == nil {
		return nil, nil
	}
	if !project.NativeState {
		return nil, domain.NewError(domain.CodePolicyDenied,
			fmt.Sprintf("project %s does not keep native state: enable it with `cf project set %s --native-state on` to seed a Run with a session", project.Name, project.Name), nil)
	}
	if ref.Adapter == "" {
		return nil, input.Invalid("a session seed names the adapter that wrote it")
	}
	if _, err := blob.ParseDigest(ref.Digest); err != nil {
		return nil, input.Invalid("the digest of a session seed must be sha256:<64 lower-case hex characters>")
	}
	uploaded, err := store.NativeSeedUploaded(ctx, q, project.ID, ref.Digest)
	if err != nil {
		return nil, err
	}
	if !uploaded {
		return nil, input.Invalid("the session seed %s was not uploaded for this Project", ref.Digest)
	}
	return &domain.NativeState{Adapter: ref.Adapter, AdapterVersion: ref.AdapterVersion, Digest: ref.Digest}, nil
}

// Pulled marks the Task as brought to the checkout of the caller (TaskPulled): the work
// is there now, so a resume that would start another writer is refused until the same
// client seeds it or the user forces it (§11.7). It is what `cf task pull` calls.
func (s *Service) Pulled(ctx context.Context, by pki.Session, ref, project string, req v1.PulledTaskRequest) (v1.PulledTaskResponse, error) {
	return mutation.Do(ctx, s.db, s.clock, by, "task.pulled", req.RequestID,
		struct{ Ref, Project string }{ref, project},
		func(tx *store.Tx, act mutation.Act) (v1.PulledTaskResponse, error) {
			id, err := resolveIn(ctx, tx, ref, project)
			if err != nil {
				return v1.PulledTaskResponse{}, err
			}
			task, err := store.TaskByID(ctx, tx, id)
			if err != nil {
				return v1.PulledTaskResponse{}, err
			}
			if err := tx.InsertTaskPull(ctx, id, act.Actor.ID, act.Now); err != nil {
				return v1.PulledTaskResponse{}, err
			}
			ev := act.Event(domain.EventTaskPulled, "task pulled to a local checkout")
			ev.ProjectID, ev.TaskID = task.ProjectID, task.ID
			ev.Data = map[string]any{"client": act.Actor.ID}
			if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
				return v1.PulledTaskResponse{}, err
			}
			out := v1.PulledTaskResponse{}
			out.Task, err = viewOf(ctx, tx, id)
			return out, err
		})
}

// guardPull decides what a resume does about a pull of the Task that no checkpoint followed
// (the double writer of §11.7). A seed from the client that pulled, or Force, lets it
// through and consumes the marker; Force is audited. Anything else is refused and nothing
// changes.
func guardPull(ctx context.Context, tx *store.Tx, act mutation.Act, task store.Task, seed *domain.NativeState, force bool) error {
	pull, err := store.PendingTaskPull(ctx, tx, task.ID)
	if err != nil || pull == nil {
		return err
	}
	switch {
	case force:
		ev := act.Event(domain.EventTaskResumeForced, "resume forced after a pull")
		ev.Level = domain.LevelWarn
		ev.ProjectID, ev.TaskID = task.ProjectID, task.ID
		ev.Data = map[string]any{"pulled_by": pull.Client, "pulled_at": pull.PulledAt, "seeded": seed != nil}
		if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
			return err
		}
		if err := audit(ctx, tx, act, "task.resume_forced", task, nil, map[string]any{"pulled_by": pull.Client}); err != nil {
			return err
		}
	case seed != nil && pull.Client == act.Actor.ID:
	default:
		return domain.NewError(domain.CodeConfirmationRequired,
			fmt.Sprintf("task %s was pulled to a local checkout at %s and nothing was checkpointed since: resuming it would put a second writer on the work. "+
				"Seed the Run from the session of that checkout with --session, or pass --force to resume anyway",
				task.Name, pull.PulledAt.Format("2006-01-02 15:04:05Z")),
			map[string]any{"pulled_at": pull.PulledAt})
	}
	return tx.ConsumeTaskPulls(ctx, task.ID, act.Now)
}

// seedRun gives the Run the session it starts from, and says so in the journal.
func (s *Service) seedRun(ctx context.Context, tx *store.Tx, act mutation.Act, task store.Task, run domain.ID, seed domain.NativeState) error {
	if err := tx.SetRunSeed(ctx, run, seed); err != nil {
		return err
	}
	ev := act.Event(domain.EventRunContinued, "run seeded with a session of the client")
	ev.ProjectID, ev.TaskID, ev.RunID = task.ProjectID, task.ID, run
	ev.Data = map[string]any{"native_seed": map[string]any{"adapter": seed.Adapter, "version": seed.AdapterVersion, "digest": seed.Digest}}
	_, _, err := tx.AppendEvent(ctx, ev)
	return err
}
