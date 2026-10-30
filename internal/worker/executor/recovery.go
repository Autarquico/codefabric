package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/worker/host"
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
		if o.Source != SourceController {
			continue
		}
		r, err := state.RunBy(ctx, e.o.DB.Reader(), o.Key)
		if errors.Is(err, state.ErrNotFound) && len(o.Payload) > 0 {
			// The previous daemon persisted the order and died before it rebuilt the record.
			var cmd v1.StopCommand
			if jerr := json.Unmarshal(o.Payload, &cmd); jerr == nil && cmd.RunID == o.RunID && cmd.Epoch == o.LeaseEpoch {
				if err := e.stopOrdered(ctx, cmd); err != nil {
					e.log.ErrorContext(ctx, "resuming a stop of a Run with no record", "run", o.RunID, "err", err)
				}
			}
			continue
		}
		if err != nil || r.Phase == state.PhaseStopping || r.Phase == state.PhaseStopUnverified {
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
		e.retrackAuth(r)
		if e.step(ctx, r.Key) || ctx.Err() != nil {
			continue
		}
		e.spawn(r.Key)
	}
	// The Environments of Runs that are over, which a crash left, are settled.
	e.settleStrays(ctx)
	// What a crash left of the secrets of a Run is swept before anything new is taken.
	e.sweep(ctx)
	e.kickDecider()
	return nil
}

// retrackAuth registers again the auth copies of a Run the previous daemon
// materialised, from its record. A Run with none, or not materialised yet,
// has nothing to register.
func (e *Executor) retrackAuth(r state.Run) {
	adapter, ok := e.harnesses.Get(parseSpec(r.Spec).Requirements.Harness)
	if !ok {
		return
	}
	if mat, err := host.ReadMaterialization(e.runDir(r.Key)); err == nil {
		e.trackAuth(r.Key, adapter, mat)
	}
}
