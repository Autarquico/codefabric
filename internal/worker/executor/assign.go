package executor

import (
	"context"
	"errors"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

const reasonUnknownCommand = "unknown_command"

// assignLoop takes the orders of the Controller: it holds the long-poll open,
// persists each assignment and the cursor in one transaction, and only then
// acknowledges them. An order of a type this Worker does not know blocks the
// queue where it is and leaves the Worker degraded: skipping it would lose it,
// and an old Worker must not act on what it does not understand.
func (e *Executor) assignLoop(ctx context.Context) {
	var (
		acked   uint64
		backoff time.Duration
	)
	for ctx.Err() == nil {
		cursor, err := state.Cursor(ctx, e.o.DB.Reader())
		if err != nil {
			e.log.ErrorContext(ctx, "reading the cursor of the queue", "err", err)
			e.pause(ctx, retryMax)
			continue
		}
		if cursor > acked {
			// The orders up to the cursor are persisted; saying so is retried at every turn until it is heard.
			if _, err := e.o.Controller.AckCommands(ctx, cursor); err == nil {
				acked = cursor
			} else if ctx.Err() == nil {
				e.log.DebugContext(ctx, "acknowledging orders failed; it will be retried", "err", err)
			}
		}
		page, err := e.o.Controller.Assignments(ctx, cursor, assignWait, 0)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			backoff = nextBackoff(backoff)
			e.log.WarnContext(ctx, "fetching assignments failed; retrying", "err", err, "in", backoff)
			e.pause(ctx, backoff)
			continue
		}
		blocked, err := e.persist(ctx, page.Commands)
		if err != nil {
			e.log.ErrorContext(ctx, "persisting assignments", "err", err)
			e.pause(ctx, retryMax)
			continue
		}
		if blocked {
			backoff = nextBackoff(backoff)
			e.pause(ctx, backoff)
			continue
		}
		backoff = 0
		e.setDegraded("")
		e.kickDecider()
	}
}

func nextBackoff(d time.Duration) time.Duration {
	return min(max(d*2, retryInitial), retryMax)
}

// pause waits on the clock, or until ctx ends.
func (e *Executor) pause(ctx context.Context, d time.Duration) {
	timer := e.clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C():
	}
}

// persist stores the orders of a page, in order, and the cursor with them. It
// stops at the first order it does not know: blocked says so. What precedes
// it is stored, and the cursor stays right before it.
func (e *Executor) persist(ctx context.Context, cmds []v1.WorkerCommand) (blocked bool, err error) {
	if len(cmds) == 0 {
		return false, nil
	}
	now := e.clock.Now()
	err = e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		for _, c := range cmds {
			if c.Type != domain.CommandAssign || c.Assign == nil {
				blocked = true
				e.setDegraded(reasonUnknownCommand)
				e.log.WarnContext(ctx, "an order of a type this Worker does not know blocks its queue", "seq", c.Seq, "type", c.Type)
				return nil
			}
			a := c.Assign
			spec := parseSpec(a.Spec)
			if _, err := state.InsertAssigned(ctx, tx, state.Run{
				Key: state.Key{RunID: a.RunID, LeaseEpoch: a.Epoch}, LeaseID: a.LeaseID, RunShort: a.RunShort, TaskID: a.TaskID,
				ProjectID: a.ProjectID, CommandSeq: c.Seq, AcceptBy: a.AcceptBy, SourceCommit: a.SourceCommit,
				CPU: a.Resources.CPU, MemoryMB: a.Resources.MemoryMB, Spec: a.Spec,
				Harness: spec.Requirements.Harness, Mode: spec.Mode, CreatedAt: now,
			}); err != nil {
				return err
			}
			if err := state.SetCursor(ctx, tx, c.Seq); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, context.Canceled) {
		return blocked, nil
	}
	return blocked, err
}
