//go:build linux || darwin

package executor

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// A daemon that dies after taking the order and before the capture starts comes back
// to a Run in checkpointing with its capture row: it quiesces again, captures
// and reports, and the ConfigDir with the session is still there to export.
func TestScenario_CheckpointCapture_DaemonRestartsBeforeCapture_Worker(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.order(k)
	if got := r.run(k).Phase; got != state.PhaseCheckpointing {
		t.Fatalf("phase = %s, want checkpointing", got)
	}

	r.restart("epoch-2")
	if got := r.run(k).Phase; got != state.PhaseCheckpointing {
		t.Fatalf("phase after restart = %s: the capture row must keep the Run", got)
	}
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.DoneAt != nil {
		t.Fatalf("capture after restart = %+v, %v", c, err)
	}
	if len(r.ctrl.completes)+len(r.ctrl.fails) != 0 {
		t.Fatal("the restart reported something")
	}

	r.settled(k, w)

	if len(r.ctrl.completes) != 1 || len(r.ctrl.fails) != 0 || r.ctrl.completes[0].NativeState == nil {
		t.Fatalf("completes = %+v, fails = %+v: the capture was not resumed with its session", r.ctrl.completes, r.ctrl.fails)
	}
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.Outcome != state.CaptureCompleted {
		t.Errorf("capture = %+v, %v", c, err)
	}
}

// codexLike is a harness that cannot name its session when it starts: its
// events do, with a line "session <id>".
type codexLike struct{ lineAdapter }

func (a codexLike) Events(ctx context.Context, h harness.Handle, src io.ReadCloser) (<-chan harness.Event, error) {
	in, err := a.lineAdapter.Events(ctx, h, src)
	if err != nil {
		return nil, err
	}
	out := make(chan harness.Event, 16)
	go func() {
		defer close(out)
		for ev := range in {
			if ev.Kind == harness.EventSessionStarted && strings.HasPrefix(ev.Model, "session ") {
				ev.SessionID, ev.Model = strings.TrimPrefix(ev.Model, "session "), ""
			}
			out <- ev
		}
	}()
	return out, nil
}

func TestTheSessionIdTheEventsSayIsKeptAndReturnedInTheHandle(t *testing.T) {
	a := codexLike{newLineAdapter()}
	f, k, _ := eventsFixtureOf(t, a)
	if got := f.run(k).SessionID; got != "" {
		t.Fatalf("Launch.SessionID was empty, but the Run has %q", got)
	}
	f.appendEvents(k, "model session sess-from-events\n")
	f.e.step(t.Context(), k)
	if got := f.run(k).SessionID; got != "sess-from-events" {
		t.Fatalf("session id = %q, want the one of the event", got)
	}
	// A later one does not replace it.
	f.appendEvents(k, "model session other\n")
	f.e.step(t.Context(), k)
	r := f.run(k)
	if r.SessionID != "sess-from-events" {
		t.Errorf("session id = %q after a second event", r.SessionID)
	}
	h, err := f.e.handleOf(a, r, parseSpec(r.Spec))
	if err != nil || h.SessionID != "sess-from-events" {
		t.Errorf("handle session = %q, %v", h.SessionID, err)
	}
}
