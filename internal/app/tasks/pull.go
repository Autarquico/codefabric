package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// BlobOpener reads a blob of the Controller by digest.
type BlobOpener interface {
	Open(d blob.Digest) (io.ReadCloser, error)
}

// WithBlobs gives the service the store the native state of a checkpoint is read from
// when a client pulls it. Without one, a pull cannot bring the session.
func WithBlobs(b BlobOpener) Option { return func(s *Service) { s.blobs = b } }

// PullPlan says what `cf task pull` brings (§11.7): the last checkpoint the last Run of
// the Task published, with the commit its ref has in the relay. A Run that is live has
// none yet, which Live says; the client decides whether to defer it. It changes nothing:
// the pull is marked (TaskPulled) once the client has what it came for.
func (s *Service) PullPlan(ctx context.Context, ref, project string) (v1.PullPlan, error) {
	var (
		out  v1.PullPlan
		task store.Task
		cp   *store.Checkpoint
	)
	err := s.db.Read(ctx, func(q store.Querier) error {
		id, err := resolveIn(ctx, q, ref, project)
		if err != nil {
			return err
		}
		if task, err = store.TaskByID(ctx, q, id); err != nil {
			return err
		}
		owner, err := store.ProjectByID(ctx, q, task.ProjectID)
		if err != nil {
			return err
		}
		all, err := store.Runs(ctx, q, store.RunFilter{TaskID: task.ID})
		if err != nil {
			return err
		}
		if len(all) == 0 {
			return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("task %s has no Run", task.Name), nil)
		}
		run := all[len(all)-1] // ordered by short ID
		out = v1.PullPlan{
			TaskID: string(task.ID), Task: task.Name, ProjectID: string(owner.ID), Project: owner.Name,
			RunID: string(run.ID), RunShort: domain.ShortID{Kind: domain.KindRun, N: run.Short}.String(),
			RunState: run.State, Live: !run.State.IsTerminal(),
		}
		if run.WorkerID != "" {
			w, err := store.WorkerByID(ctx, q, run.WorkerID)
			if err != nil {
				return err
			}
			out.Worker, out.WorkerState = w.Name, w.State
		}
		if out.SourceBranch, out.SeedDigest, err = originOf(ctx, q, all); err != nil {
			return err
		}
		if out.Live {
			return nil
		}
		last, err := lastCheckpoint(ctx, q, run)
		if err != nil {
			return domain.NewError(domain.CodeNotFound, fmt.Sprintf("task %s has no published checkpoint to pull: its last Run is %s", task.Name, run.State), nil)
		}
		cp = &last
		return nil
	})
	if err != nil || cp == nil {
		return out, err
	}
	if out.Commit, err = s.checkpointCommit(ctx, task, *cp); err != nil {
		return v1.PullPlan{}, err
	}
	view := v1.Checkpoint{
		ID: string(cp.ID), RunID: string(cp.RunID), Seq: cp.Seq, Trigger: cp.Trigger, Ref: cp.SourceRef,
		Excluded: append([]string{}, cp.Excluded...), Warnings: append([]string{}, cp.Warnings...),
		Summary: v1.CheckpointSummary(cp.Summary), CreatedAt: cp.CreatedAt,
	}
	if n := cp.NativeState; n != nil {
		view.NativeState = &v1.NativeStateRef{Adapter: n.Adapter, AdapterVersion: n.AdapterVersion, Digest: n.Digest}
	}
	out.Checkpoint = &view
	return out, nil
}

// originOf is where the work of the Task began: the local branch of the snapshot it
// started from, and the digest of the session it was seeded with, from the latest Run
// that has each (later Runs continue from checkpoints and may carry neither).
func originOf(ctx context.Context, q store.Querier, runs []store.Run) (branch, seed string, err error) {
	for i := len(runs) - 1; i >= 0 && (branch == "" || seed == ""); i-- {
		if branch == "" {
			var spec struct {
				Source struct {
					Branch string `json:"branch"`
				} `json:"source"`
			}
			if json.Unmarshal(runs[i].Spec, &spec) == nil {
				branch = spec.Source.Branch
			}
		}
		if seed == "" {
			s, err := store.RunSeed(ctx, q, runs[i].ID)
			if err != nil {
				return "", "", err
			}
			if s != nil {
				seed = s.Digest
			}
		}
	}
	return branch, seed, nil
}

// OpenPullNativeState is the native state of the checkpoint that PullPlan names, and
// its size, for the client that pulls it. The Project must keep native state.
func (s *Service) OpenPullNativeState(ctx context.Context, ref, project string) (io.ReadCloser, int64, error) {
	if s.blobs == nil {
		return nil, 0, domain.NewError(domain.CodeInternal, "the Controller has no blob store", nil)
	}
	plan, err := s.PullPlan(ctx, ref, project)
	if err != nil {
		return nil, 0, err
	}
	if plan.Checkpoint == nil || plan.Checkpoint.NativeState == nil {
		return nil, 0, domain.NewError(domain.CodeNotFound, "the checkpoint to pull has no native state", nil)
	}
	var meta store.Blob
	err = s.db.Read(ctx, func(q store.Querier) error {
		p, err := store.ProjectByID(ctx, q, domain.ID(plan.ProjectID))
		if err != nil {
			return err
		}
		if !p.NativeState {
			return domain.NewError(domain.CodePolicyDenied, "the Project does not keep native state", nil)
		}
		if meta, err = store.BlobMeta(ctx, q, plan.Checkpoint.NativeState.Digest); err != nil {
			return domain.NewError(domain.CodeNotFound, "the native state of the checkpoint is not in the Controller", nil)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	d, err := blob.ParseDigest(meta.Digest)
	if err != nil {
		return nil, 0, err
	}
	rc, err := s.blobs.Open(d)
	return rc, meta.Size, err
}
