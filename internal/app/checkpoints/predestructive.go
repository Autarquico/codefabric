package checkpoints

import (
	"context"
	"errors"
	"slices"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// pollEvery is how often AwaitCaptures looks whether the captures it asked are done.
const pollEvery = 250 * time.Millisecond

// RequestBeforeDestroy asks the Run a checkpoint of trigger pre-destructive as
// by, the user who wants the destructive action: the event carries their actor
// and a repeated requestID answers what the first one did. It never fails: a
// checkpoint that cannot be asked is reported with why.
func (s *Service) RequestBeforeDestroy(ctx context.Context, by pki.Session, run domain.ID, requestID string) v1.ForcedRun {
	got, err := s.Request(ctx, by, run, domain.CheckpointPreDestructive, requestID)
	if err != nil {
		if id := s.captureUnderWay(ctx, run); id != "" {
			return v1.ForcedRun{RunID: string(run), CheckpointID: string(id)}
		}
		return v1.ForcedRun{RunID: string(run), Reason: reasonOf(err)}
	}
	return v1.ForcedRun{RunID: string(run), CheckpointID: got.CheckpointID}
}

// captureUnderWay is the checkpoint a Run is being captured for already (an
// interval capture, a manual one), which a forced drain waits for instead of
// cancelling the Run under it; "" if there is none.
func (s *Service) captureUnderWay(ctx context.Context, run domain.ID) domain.ID {
	var id domain.ID
	_ = s.db.Read(ctx, func(q store.Querier) error {
		r, err := store.RunByID(ctx, q, run)
		if err != nil || r.State != domain.RunCheckpointing {
			return err
		}
		held, err := store.CheckpointReservationsOfRun(ctx, q, run)
		if len(held) > 0 {
			id = held[len(held)-1].ID
		}
		return err
	})
	return id
}

// AwaitCaptures waits, for at most wait, for the pre-destructive checkpoints
// that forced names, asked beforehand with Request in the mutation of the user
// who wants the destructive action, so that what is about to be thrown away is
// kept if it can be. It never fails that action: a Run whose checkpoint could
// not be asked, failed, or was not published in time is reported with why, and
// the caller goes on. The answers are in the order of forced.
func (s *Service) AwaitCaptures(ctx context.Context, forced []v1.ForcedRun, wait time.Duration) []v1.ForcedRun {
	out := slices.Clone(forced)
	pending := map[int]domain.ID{}
	for i, f := range out {
		if f.CheckpointID != "" && !f.Captured {
			pending[i] = domain.ID(f.CheckpointID)
		}
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
