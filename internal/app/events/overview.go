package events

import (
	"context"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Overview answers GET /v1/overview. Counters, Workers, the activity tail and
// the cursor come from one read snapshot, and the cursor is the highest
// sequence in it: the Notifier is advanced after the commit, so it may lag the
// state that the counters already show.
func (s *Service) Overview(ctx context.Context, by pki.Session, activity int) (v1.Overview, error) {
	switch {
	case activity < 0 || activity > v1.OverviewMaxActivity:
		return v1.Overview{}, input.Invalid("activity must be between 0 and %d", v1.OverviewMaxActivity)
	case activity == 0:
		activity = v1.OverviewDefaultActivity
	}
	var out v1.Overview
	err := s.db.Read(ctx, func(r store.Querier) error {
		if _, err := pki.RequireCaller(ctx, r, by, s.clock.Now()); err != nil {
			return err
		}
		latest, _, err := store.EventBounds(ctx, r)
		if err != nil {
			return err
		}
		counts, err := store.CountOverview(ctx, r)
		if err != nil {
			return err
		}
		workers, err := store.Workers(ctx, r, false)
		if err != nil {
			return err
		}
		tail, err := s.tail(ctx, r, by, store.EventFilter{}, latest, activity)
		if err != nil {
			return err
		}
		out = v1.Overview{
			Cursor: formatCursor(latest), Tasks: counts.Tasks, NeedsInput: counts.NeedsInput, Runs: counts.Runs,
			Workers: map[domain.WorkerState]int{}, WorkerHealth: map[string]int{}, Activity: tail.Events,
		}
		for _, w := range workers {
			out.Workers[w.State]++
			status := w.Health.Status
			if status == "" {
				status = v1.WorkerHealthUnknown
			}
			out.WorkerHealth[status]++
		}
		return nil
	})
	return out, err
}
