package executor

import (
	"context"
	"fmt"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

// recoverRuns brings back what the previous daemon left (§6.4). It runs before the
// queue is read, so that nothing is accepted on a Worker whose Runs are not
// accounted for. Each Run that is not over gets its supervisor, whose first
// step looks at the evidence on disk and does what the table of the milestone
// says: assignments not yet answered wait for the decider, accepted ones
// start, started ones are adopted or settled from their markers, a stop in
// progress is signalled again, and a Run whose processes could not be verified
// is verified again.
func (e *Executor) recoverRuns(ctx context.Context) error {
	// A stop of the Controller that the previous daemon persisted and did not
	// carry out is carried out first: an accepted Run with one never starts.
	pending, err := state.PendingStopOrders(ctx, e.o.DB.Reader())
	if err != nil {
		return fmt.Errorf("executor: reading the stops to resume: %w", err)
	}
	for _, o := range pending {
		r, err := state.RunBy(ctx, e.o.DB.Reader(), o.Key)
		if err != nil || o.Source != SourceController || r.Phase == state.PhaseStopping || r.Phase == state.PhaseStopUnverified {
			continue
		}
		if err := e.Stop(ctx, o.RunID, o.LeaseEpoch, o.Reason, o.Source); err != nil {
			e.log.ErrorContext(ctx, "resuming a stop", "run", o.RunID, "err", err)
		}
	}
	live, err := state.LiveRuns(ctx, e.o.DB.Reader())
	if err != nil {
		return fmt.Errorf("executor: reading the Runs to recover: %w", err)
	}
	for _, r := range live {
		if r.Phase == state.PhaseAssigned {
			continue
		}
		if e.step(ctx, r.Key) || ctx.Err() != nil {
			continue
		}
		e.spawn(r.Key)
	}
	e.kickDecider()
	return nil
}
