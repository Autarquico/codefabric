package outputship_test

import (
	"os"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

func (f *fixture) unsent() uint64 {
	f.t.Helper()
	n, err := f.ship.Unsent(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) shed(need uint64) uint64 {
	f.t.Helper()
	n, err := f.ship.Shed(f.t.Context(), need)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestShed_OnlyWhatTheControllerHasNotConfirmedCounts(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	f.write(k, strings.Repeat("a", 100))
	if got := f.unsent(); got != 100 {
		t.Fatalf("unsent = %d, want 100", got)
	}
	f.once()
	if got := f.unsent(); got != 0 {
		t.Fatalf("unsent after it was sent = %d, want 0", got)
	}
	f.write(k, strings.Repeat("b", 30))
	if got := f.unsent(); got != 30 {
		t.Fatalf("unsent = %d, want 30", got)
	}
}

func TestShed_SkipsTheRunWithMostWaitingFirstAndLeavesTheFileAndAGapForTheController(t *testing.T) {
	f := newFixture(t)
	small, big := f.run("run_small", state.PhaseRunning), f.run("run_big", state.PhaseRunning)
	f.write(small, strings.Repeat("s", 10))
	f.write(big, strings.Repeat("b", 1000))

	if got := f.shed(500); got != 1000 {
		t.Fatalf("shed %d, want the 1000 of the Run with most waiting", got)
	}
	if c := f.cursor(big); c.Sent != 1000 || !c.HasFile {
		t.Errorf("cursor of the big Run = %+v", c)
	}
	if c := f.cursor(small); c.Sent != 0 {
		t.Errorf("cursor of the small Run = %+v: it was not needed", c)
	}
	if b, err := os.ReadFile(f.logPath(big)); err != nil || len(b) != 1000 {
		t.Fatalf("the log is %d bytes (%v): it must not be touched", len(b), err)
	}

	// What comes later goes from where the cursor is: the Controller sees a gap.
	f.write(big, "tail")
	f.once()
	if got := f.sent(big); got != "tail" {
		t.Fatalf("the Controller holds %q of the big Run, want only the tail", got)
	}
	if h := f.ctl.outputs[big]; h.start != 1000 {
		t.Errorf("the output starts at %d, want 1000", h.start)
	}
}

func TestShed_IsRecordedInTheOpenTruncation(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	f.write(k, strings.Repeat("a", 64))
	f.shed(1)
	f.write(k, strings.Repeat("a", 16))
	f.shed(1)

	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		_, err := state.SealTruncation(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := state.SealedTruncations(t.Context(), f.db.Reader(), 10)
	if err != nil || len(got) != 1 || got[0].OutputBytes != 80 || got[0].LogLines != 0 {
		t.Fatalf("truncations = %+v (%v), want 80 bytes of output", got, err)
	}
}

func TestShed_NeverTouchesAnOutputThatIsDoneOrWhoseLogIsAnotherFile(t *testing.T) {
	f := newFixture(t)
	done := f.run("run_done", state.PhaseRunning)
	f.write(done, strings.Repeat("d", 50))
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		return state.SaveOutputCursor(t.Context(), tx, state.OutputCursor{Key: done, Done: true})
	}); err != nil {
		t.Fatal(err)
	}
	replaced := f.run("run_replaced", state.PhaseRunning)
	f.write(replaced, strings.Repeat("r", 50))
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		return state.SaveOutputCursor(t.Context(), tx, state.OutputCursor{Key: replaced, HasFile: true, Dev: 1, Ino: 1})
	}); err != nil {
		t.Fatal(err)
	}

	if got := f.unsent(); got != 0 {
		t.Errorf("unsent = %d, want 0", got)
	}
	if got := f.shed(1000); got != 0 {
		t.Errorf("shed %d, want 0", got)
	}
	if c := f.cursor(done); c.Sent != 0 || !c.Done {
		t.Errorf("cursor of the finished output = %+v", c)
	}
}

func TestShed_ACursorThatMovedDuringARequestIsNotMovedBackWhenItIsSettled(t *testing.T) {
	f := newFixture(t)
	k := f.run("run_a", state.PhaseRunning)
	f.write(k, strings.Repeat("a", 100))
	// While the 100 bytes are being sent, 50 more are written and the journal gives them up.
	f.ctl.duringUpload = func() {
		f.write(k, strings.Repeat("b", 50))
		f.shed(1)
		f.ctl.duringUpload = nil
	}
	f.once()

	if c := f.cursor(k); c.Sent != 150 {
		t.Fatalf("sent = %d, want 150: the settle of the request moved the cursor back", c.Sent)
	}
	f.write(k, "tail")
	f.once()
	if h := f.ctl.outputs[k]; h.end != 154 || string(h.data) != "tail" {
		t.Fatalf("the Controller holds %d..%d %q, want the tail after the gap", h.start, h.end, h.data)
	}
}
