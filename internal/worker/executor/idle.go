package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// ReasonIdleAfterResult is the reason of the stop the Worker orders when a
// harness has given its result and stayed alive, idle, for too long.
const ReasonIdleAfterResult = "idle_after_result"

// DefaultIdleAfterResult is how long such a harness is left alone.
const DefaultIdleAfterResult = 120 * time.Second

// stopIfIdleAfterResult stops a headless harness that has finished, and has not
// exited: its last event is a result that is not an error, its input is closed
// with no prompt open, and neither its events nor its stderr have grown for
// idleAfter. The caller holds the lock of the Run.
//
// Where the count stands (the size last seen and since when) is kept with the
// cursor, so that a daemon that restarts goes on with it. Any growth, a prompt,
// or an event that is not a result starts it over. A harness that exits by
// itself, or a stop that is ordered first, is settled as it is: the order is
// only given to a Run that has none.
func (e *Executor) stopIfIdleAfterResult(ctx context.Context, r state.Run) {
	if err := e.idleCheck(ctx, r); err != nil && ctx.Err() == nil {
		e.log.WarnContext(ctx, "looking at whether the harness of a Run is idle failed; it will be tried again", "run", r.RunID, "err", err)
	}
}

func (e *Executor) idleCheck(ctx context.Context, r state.Run) error {
	if !inRun(r) {
		return nil
	}
	spec := parseSpec(r.Spec)
	adapter, ok := e.harnesses.Get(spec.Requirements.Harness)
	if !ok || !adapter.Capabilities().IdleAfterResult || spec.Mode == execwrap.ModeInteractive {
		return nil
	}
	cur, err := state.EventCursorOf(ctx, e.o.DB.Reader(), r.Key)
	if err != nil {
		return err
	}
	size, eligible, err := e.idleEvidence(ctx, r.Key, cur)
	if err != nil {
		return err
	}
	if !eligible {
		if cur.IdleSize == 0 && cur.IdleSince.IsZero() {
			return nil
		}
		cur.IdleSize, cur.IdleSince = 0, time.Time{}
		return e.saveCursor(ctx, cur)
	}
	now := e.clock.Now()
	if cur.IdleSince.IsZero() || cur.IdleSize != size {
		cur.IdleSize, cur.IdleSince = size, now
		return e.saveCursor(ctx, cur)
	}
	if now.Sub(cur.IdleSince) < e.idleAfter {
		return nil
	}
	if _, err := state.StopOrderOf(ctx, e.o.DB.Reader(), r.Key); err == nil {
		return nil // a stop is under way: it keeps its reason
	} else if !errors.Is(err, state.ErrNotFound) {
		return err
	}
	e.log.InfoContext(ctx, "the harness of a Run gave its result and stayed idle: stopping it", "run", r.RunID, "idle", now.Sub(cur.IdleSince).String())
	return e.stopLocked(ctx, r.Key, ReasonIdleAfterResult, SourceWorker)
}

// idleEvidence is the sum of the sizes of what the harness has written, and
// whether the Run is in the state in which idleness counts at all.
func (e *Executor) idleEvidence(ctx context.Context, k state.Key, cur state.EventCursor) (size int64, eligible bool, err error) {
	if !cur.ResultOK {
		return 0, false, nil
	}
	dir := e.runDir(k)
	if _, err := os.Stat(filepath.Join(dir, execwrap.InputEOFFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	open, err := state.OpenInputPrompts(ctx, e.o.DB.Reader(), k)
	if err != nil || open > 0 {
		return 0, false, err
	}
	events, err := sizeOf(filepath.Join(dir, execwrap.EventsFile))
	if err != nil {
		return 0, false, err
	}
	if events > int64(cur.Consumed) { //nolint:gosec // an offset of a file we read
		return 0, false, nil // what it wrote last is not consumed yet
	}
	stderr, err := sizeOf(filepath.Join(dir, execwrap.OutputLog))
	if err != nil {
		return 0, false, err
	}
	return events + stderr, true, nil
}

func sizeOf(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
