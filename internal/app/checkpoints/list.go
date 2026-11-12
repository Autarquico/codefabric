package checkpoints

import (
	"context"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/controller/store"
)

// OfRun lists the published checkpoints of the Run a reference points to, by seq.
func (s *Service) OfRun(ctx context.Context, ref string) ([]v1.Checkpoint, error) {
	return s.list(ctx, func(q store.Querier) ([]store.Checkpoint, error) {
		id, err := refs.Run(ctx, q, ref)
		if err != nil {
			return nil, err
		}
		return store.CheckpointsOfRun(ctx, q, id)
	})
}

// OfTask lists the published checkpoints of every Run of a Task, oldest first;
// project narrows a Task name that more than one Project has.
func (s *Service) OfTask(ctx context.Context, ref, project string) ([]v1.Checkpoint, error) {
	return s.list(ctx, func(q store.Querier) ([]store.Checkpoint, error) {
		scope, err := refs.Scope(ctx, q, project)
		if err != nil {
			return nil, err
		}
		id, err := refs.Task(ctx, q, ref, scope)
		if err != nil {
			return nil, err
		}
		return store.CheckpointsOfTask(ctx, q, id)
	})
}

func (s *Service) list(ctx context.Context, read func(store.Querier) ([]store.Checkpoint, error)) ([]v1.Checkpoint, error) {
	var out []v1.Checkpoint
	err := s.db.Read(ctx, func(q store.Querier) error {
		rows, err := read(q)
		for _, c := range rows {
			out = append(out, viewOf(c))
		}
		return err
	})
	return out, err
}

func viewOf(c store.Checkpoint) v1.Checkpoint {
	v := v1.Checkpoint{
		ID: string(c.ID), RunID: string(c.RunID), Seq: c.Seq, Trigger: c.Trigger, Consistency: c.Consistency, Ref: c.SourceRef,
		Excluded: c.Excluded, CreatedAt: c.CreatedAt,
		Summary: v1.CheckpointSummary(c.Summary),
	}
	if v.Excluded == nil {
		v.Excluded = []string{}
	}
	v.Warnings = append([]string{}, c.Warnings...)
	if n := c.NativeState; n != nil {
		v.NativeState = &v1.NativeStateRef{Adapter: n.Adapter, AdapterVersion: n.AdapterVersion, Digest: n.Digest}
	}
	return v
}
