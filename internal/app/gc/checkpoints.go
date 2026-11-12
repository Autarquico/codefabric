package gc

import (
	"context"
	"slices"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Items of the report for the checkpoints.
const (
	checkpointsKind            = "checkpoints"
	checkpointReservationsKind = "checkpoint_reservations"
)

// CheckpointRoot names checkpoints that must not be collected, for a reason the
// GC does not know by itself (the move that is not over holds the checkpoint it
// resumes from). It calls keep with each ID and reads through q.
type CheckpointRoot func(ctx context.Context, q store.Querier, keep func(domain.ID)) error

// WithCheckpointRoots adds roots to the ones the store knows: the latest
// checkpoint of every Task that is not archived and the last Policy.AutoCheckpoints
// of each Run's interval trigger.
func WithCheckpointRoots(roots ...CheckpointRoot) Option {
	return func(s *Service) { s.roots = append(s.roots, roots...) }
}

// checkpointGarbage is what a pass would delete: the published checkpoints no
// root keeps, and the reservations that never completed. Snapshots live under
// refs/cf/snapshots/ and are not looked at: nothing here can name them.
type checkpointGarbage struct {
	published, reservations []store.CheckpointGarbage
}

func (s *Service) checkpointGarbage(ctx context.Context, now time.Time) (checkpointGarbage, error) {
	var out checkpointGarbage
	err := s.db.Read(ctx, func(q store.Querier) error {
		kept := map[domain.ID]bool{}
		for _, root := range s.roots {
			if err := root(ctx, q, func(id domain.ID) { kept[id] = true }); err != nil {
				return err
			}
		}
		unprotected, err := store.UnprotectedCheckpoints(ctx, q, s.policy.AutoCheckpoints, now.Add(-s.policy.RelayBranches))
		if err != nil {
			return err
		}
		out.published = slices.DeleteFunc(unprotected, func(c store.CheckpointGarbage) bool { return kept[c.ID] })
		out.reservations, err = store.StaleCheckpointReservations(ctx, q, now.Add(-s.policy.CheckpointReservation))
		return err
	})
	return out, err
}

func (s *Service) planCheckpoints(ctx context.Context, now time.Time) ([]v1.GCCount, error) {
	g, err := s.checkpointGarbage(ctx, now)
	return []v1.GCCount{
		{Kind: checkpointsKind, Count: int64(len(g.published))},
		{Kind: checkpointReservationsKind, Count: int64(len(g.reservations))},
	}, err
}

// sweepCheckpoints deletes, a batch at a time, the ref of each checkpoint in the
// relay and then its row. The ref goes first: a failure between the two leaves
// the row, and the next pass finds it again, where a ref with no row would be
// found by nobody. The native state goes with the blob sweep that follows, once
// no row names it. With no relay nothing is deleted: the rows would orphan the refs.
func (s *Service) sweepCheckpoints(ctx context.Context, rep *v1.GCReport) error {
	if s.relay == nil {
		return nil
	}
	g, err := s.checkpointGarbage(ctx, s.clock.Now())
	if err != nil {
		return err
	}
	touched := map[domain.ID]bool{}
	for _, part := range []struct {
		kind         string
		list         []store.CheckpointGarbage
		reservations bool
	}{{checkpointsKind, g.published, false}, {checkpointReservationsKind, g.reservations, true}} {
		done := v1.GCCount{Kind: part.kind}
		err := s.deleteCheckpoints(ctx, part.list, part.reservations, &done, touched)
		rep.Items = append(rep.Items, done)
		if err != nil {
			return err
		}
	}
	projects := make([]domain.ID, 0, len(touched))
	for p := range touched {
		projects = append(projects, p)
	}
	slices.Sort(projects)
	for _, p := range projects {
		if err := s.collect(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) deleteCheckpoints(ctx context.Context, list []store.CheckpointGarbage, reservations bool, done *v1.GCCount, touched map[domain.ID]bool) error {
	for batch := range chunks(list, s.policy.BatchSize) {
		refs := map[domain.ID][]string{}
		ids := make([]domain.ID, 0, len(batch))
		for _, c := range batch {
			refs[c.Project] = append(refs[c.Project], c.Ref)
			ids = append(ids, c.ID)
		}
		for project, names := range refs {
			if err := s.deleteRefs(ctx, project, names); err != nil {
				return err
			}
			touched[project] = true
		}
		n, err := s.db.DeleteCheckpoints(ctx, ids, reservations)
		done.Count += n
		if err != nil {
			return err
		}
		if s.hooks.AfterBatch != nil {
			s.hooks.AfterBatch(done.Kind, n)
		}
		if err := yield(ctx); err != nil {
			return err
		}
	}
	return nil
}
