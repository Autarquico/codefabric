package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/host"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// ReasonInputRequired is the reason of the stop the Worker orders when the
// harness waits for input (§43 Fase 2: needs-input is unsupported).
const ReasonInputRequired = "input_required"

// eventsChunk bounds what one turn gives the adapter.
const eventsChunk = 1 << 20

// consumeEvents gives the adapter the structured stream the harness has written
// since the last turn (events.jsonl) and acts on what it says: the model is
// kept for the exit report, and a harness waiting for input is stopped with a
// stop of the Worker (§10.7). The caller holds the lock of the Run. A failure is
// logged and the same bytes are read again at the next turn.
//
// Each turn parses its bytes on their own, whole lines only: an adapter's
// Events must not depend on lines of an earlier turn.
func (e *Executor) consumeEvents(ctx context.Context, r state.Run) {
	if _, err := e.drainEvents(ctx, r, false); err != nil {
		e.log.WarnContext(ctx, "consuming the events of a Run failed; it will be tried again", "run", r.RunID, "err", err)
	}
}

// drainEventsToEOF is the final pass, with the harness over: everything it
// wrote is consumed, the last line even if it has no newline, and nothing is
// stopped. An error leaves the Run unsettled, to be tried again; see
// noteFinalFailure for when to stop trying.
func (e *Executor) drainEventsToEOF(ctx context.Context, r state.Run) error {
	for {
		more, err := e.drainEvents(ctx, r, true)
		if err != nil || !more {
			return err
		}
	}
}

// drainEvents reads one chunk from the cursor and says whether it filled the
// chunk (there may be more). final is the harness being over.
func (e *Executor) drainEvents(ctx context.Context, r state.Run, final bool) (more bool, err error) {
	spec := parseSpec(r.Spec)
	adapter, ok := e.harnesses.Get(spec.Requirements.Harness)
	if !ok || spec.Mode == execwrap.ModeInteractive {
		return false, nil
	}
	dir := e.runDir(r.Key)
	f, err := os.Open(filepath.Join(dir, execwrap.EventsFile)) //nolint:gosec // a file of the run directory
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // the adapter did not ask for a structured stream
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	cur, err := state.EventCursorOf(ctx, e.o.DB.Reader(), r.Key)
	if err != nil {
		return false, err
	}
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() <= int64(cur.Consumed) { //nolint:gosec // a file size
		return false, nil
	}
	buf := make([]byte, min(info.Size()-int64(cur.Consumed), eventsChunk)) //nolint:gosec // a file size
	n, err := f.ReadAt(buf, int64(cur.Consumed))                           //nolint:gosec // an offset of a file we wrote
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	full := n == eventsChunk
	data := buf[:n]
	next := cur
	next.FinalFailures = 0
	if cur.Skipping {
		// The rest of a line too long to read: drop it up to its newline.
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			i = len(data) - 1
		} else {
			next.Skipping = false
		}
		next.Consumed += uint64(i + 1)
		data = data[i+1:]
		if len(data) == 0 {
			return full, e.saveCursor(ctx, next)
		}
	}
	base := len(buf[:n]) - len(data) // bytes dropped above
	if !final || full {
		// Whole lines only: a line the harness is still writing waits for its
		// newline, and a full chunk ends where its last line does.
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	if len(data) == 0 {
		if full && base == 0 {
			// No newline in a whole chunk: a line longer than the bound. It is
			// not read, neither live nor at the end; its end is looked for next.
			e.log.WarnContext(ctx, "a line of the events of a Run is longer than the bound: skipping it", "run", r.RunID, "offset", cur.Consumed, "bound", eventsChunk)
			next.Consumed += uint64(n)
			next.Skipping = true
			return true, e.saveCursor(ctx, next)
		}
		if base > 0 {
			return full, e.saveCursor(ctx, next) // the line starts at the cursor now; read it whole next time
		}
		return false, nil
	}
	more = full
	events, err := e.parseEvents(ctx, adapter, r, spec, data)
	if err != nil {
		if errors.Is(err, harness.ErrUnsupported) {
			events = nil // nothing to learn from this stream; the cursor still moves
		} else {
			return false, err
		}
	}
	next.Consumed += uint64(len(data))
	needsInput, message := false, ""
	for _, ev := range events {
		switch ev.Kind {
		case harness.EventSessionStarted:
			if next.Model != "" {
				break
			}
			if !reportableModel(ev.Model) {
				e.log.WarnContext(ctx, "the model a harness says is not reportable: dropped", "run", r.RunID, "len", len(ev.Model))
				break
			}
			next.Model = ev.Model
		case harness.EventInputRequired:
			needsInput, message = true, ev.Message
		}
	}
	// The stop is ordered, and persisted, before the cursor moves: a daemon that
	// dies in between reads the line again and finds the order there. The
	// observation itself moves with the cursor, in the same write.
	if needsInput {
		next.InputRequired = true
		if err := e.stopForInput(ctx, r, final, message); err != nil {
			return false, err
		}
	}
	return more, e.saveCursor(ctx, next)
}

func (e *Executor) saveCursor(ctx context.Context, c state.EventCursor) error {
	return e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.SaveEventCursor(ctx, tx, c) })
}

// reportableModel is the Controller's rule for a model in an exit report: a
// harness's own word is not allowed to get the report refused.
func reportableModel(m string) bool {
	return len(m) <= v1.MaxModelLen && domain.Printable(m)
}

// maxFinalFailures bounds the consecutive failures of the final pass: a stream
// that an adapter cannot read must not hold the Run unsettled forever.
const maxFinalFailures = 5

// noteFinalFailure counts a failure of the final pass, with the cursor, and says
// how many there are and whether the bound is reached.
func (e *Executor) noteFinalFailure(ctx context.Context, r state.Run) (failures int, giveUp bool, err error) {
	cur, err := state.EventCursorOf(ctx, e.o.DB.Reader(), r.Key)
	if err != nil {
		return 0, false, err
	}
	cur.FinalFailures++
	if err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error { return state.SaveEventCursor(ctx, tx, cur) }); err != nil {
		return 0, false, err
	}
	return cur.FinalFailures, cur.FinalFailures >= maxFinalFailures, nil
}

// parseEvents runs the adapter over data and collects what it says.
func (e *Executor) parseEvents(ctx context.Context, adapter harness.Adapter, r state.Run, spec specView, data []byte) ([]harness.Event, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := adapter.Events(ctx, e.handleOf(adapter, r, spec), io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		return nil, err
	}
	var out []harness.Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out, ctx.Err()
}

// handleOf rebuilds the Handle of a Run from disk.
func (e *Executor) handleOf(adapter harness.Adapter, r state.Run, spec specView) harness.Handle {
	dir := e.runDir(r.Key)
	h := harness.Handle{
		RunID: domain.ID(r.RunID), LeaseEpoch: r.LeaseEpoch, Mode: domain.Mode(spec.Mode), RunDir: dir,
		Worktree: e.worktree(r.Key), OutputPath: filepath.Join(dir, execwrap.OutputLog), EventsPath: filepath.Join(dir, execwrap.EventsFile),
	}
	if adapter.Capabilities().ConfigIsolation {
		h.ConfigDir = host.ConfigDir(dir)
	}
	return h
}

// stopForInput orders the stop of a harness that waits for input, once. A Run
// that is stopping already, or over, keeps the stop it has.
func (e *Executor) stopForInput(ctx context.Context, r state.Run, final bool, message string) error {
	if final || (r.Phase != state.PhaseStarting && r.Phase != state.PhaseRunning) {
		return nil
	}
	if _, err := state.StopOrderOf(ctx, e.o.DB.Reader(), r.Key); err == nil {
		return nil
	} else if !errors.Is(err, state.ErrNotFound) {
		return err
	}
	e.log.InfoContext(ctx, "the harness of a Run waits for input: stopping it", "run", r.RunID, "message", message)
	if err := e.stopLocked(ctx, r.Key, ReasonInputRequired, SourceWorker); err != nil {
		return fmt.Errorf("stopping a Run that waits for input: %w", err)
	}
	return nil
}
