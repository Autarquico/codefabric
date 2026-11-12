package checkpoints

import (
	"context"
	"errors"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// pollEvery is how often CaptureBeforeDestroy looks whether the captures it asked are done.
const pollEvery = 250 * time.Millisecond

// CaptureBeforeDestroy asks each Run a checkpoint of trigger pre-destructive and
// waits for the captures, for at most wait, so that what is about to be thrown
// away is kept if it can be. It never fails the destructive action: a Run whose
// checkpoint could not be asked, failed, or was not published in time is
// reported with why, and the caller goes on. The answers are in the order of runs.
func (s *Service) CaptureBeforeDestroy(ctx context.Context, runs []domain.ID, wait time.Duration) []v1.ForcedRun {
	out := make([]v1.ForcedRun, len(runs))
	pending := map[int]domain.ID{}
	for i, run := range runs {
		out[i].RunID = string(run)
		var got Requested
		err := s.db.WithTx(ctx, func(tx *store.Tx) error {
			var err error
			got, err = s.RequestIn(ctx, tx, mutation.Act{Actor: systemActor, Now: s.clock.Now()}, run, domain.CheckpointPreDestructive)
			return err
		})
		if err != nil {
			out[i].Reason = reasonOf(err)
			continue
		}
		out[i].CheckpointID = got.CheckpointID
		pending[i] = domain.ID(got.CheckpointID)
	}
	if len(pending) == 0 {
		return out
	}
	deadline := s.clock.NewTimer(wait)
	defer deadline.Stop()
	ticker := s.clock.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		s.settle(ctx, out, pending)
		if len(pending) == 0 {
			return out
		}
		select {
		case <-ctx.Done():
			giveUp(out, pending, "the request ended")
			return out
		case <-deadline.C():
			s.settle(ctx, out, pending)
			giveUp(out, pending, "the capture was not done in time")
			return out
		case <-ticker.C():
		}
	}
}

// settle takes out of pending the captures that are published or aborted.
func (s *Service) settle(ctx context.Context, out []v1.ForcedRun, pending map[int]domain.ID) {
	for i, id := range pending {
		var published *time.Time
		err := s.db.Read(ctx, func(q store.Querier) (err error) {
			published, err = store.CheckpointPublishedAt(ctx, q, id)
			return err
		})
		switch {
		case errors.Is(err, store.ErrNotFound):
			out[i].CheckpointID, out[i].Reason = "", "the Worker could not capture it"
			delete(pending, i)
		case err != nil:
			// Read again at the next look.
		case published != nil:
			out[i].Captured = true
			delete(pending, i)
		}
	}
}

func giveUp(out []v1.ForcedRun, pending map[int]domain.ID, why string) {
	for i := range pending {
		out[i].Reason = why
	}
}

func reasonOf(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Message
	}
	return "the checkpoint could not be requested"
}
