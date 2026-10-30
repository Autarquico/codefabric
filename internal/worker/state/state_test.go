package state_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) (*state.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker", "state.db")
	db, err := state.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func tx(t *testing.T, db *state.DB, fn func(*state.Tx) error) {
	t.Helper()
	if err := db.WithTx(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

func assignment(id string, epoch, seq uint64) state.Run {
	return state.Run{
		Key: state.Key{RunID: id, LeaseEpoch: epoch}, LeaseID: "lease_" + id, RunShort: "run-1", TaskID: "task_1", ProjectID: "project_1",
		CommandSeq: seq, AcceptBy: t0.Add(time.Minute), SourceCommit: "abc", CPU: 2, MemoryMB: 4096,
		Spec: []byte(`{"schema":2}`), Harness: "command", Mode: "headless", CreatedAt: t0,
	}
}

func TestOpenUsesWALAndFullSynchronous(t *testing.T) {
	db, _ := open(t)
	var mode string
	var sync int
	if err := db.Reader().QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q (%v)", mode, err)
	}
	if err := db.Reader().QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&sync); err != nil || sync != 2 {
		t.Errorf("synchronous = %d (%v), want 2 (FULL)", sync, err)
	}
}

func TestStateSurvivesReopening(t *testing.T) {
	db, path := open(t)
	tx(t, db, func(x *state.Tx) error {
		_, err := state.InsertAssigned(t.Context(), x, assignment("run_a", 1, 1))
		return err
	})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := state.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	got, err := state.RunBy(t.Context(), again.Reader(), state.Key{RunID: "run_a", LeaseEpoch: 1})
	if err != nil || got.Phase != state.PhaseAssigned || got.CPU != 2 || string(got.Spec) != `{"schema":2}` || !got.AcceptBy.Equal(t0.Add(time.Minute)) {
		t.Errorf("run = %+v (%v)", got, err)
	}
}

func TestInsertAssignedKeepsWhatIsStoredWhenTheOrderComesAgain(t *testing.T) {
	db, _ := open(t)
	k := state.Key{RunID: "run_a", LeaseEpoch: 1}
	tx(t, db, func(x *state.Tx) error {
		if ok, err := state.InsertAssigned(t.Context(), x, assignment("run_a", 1, 1)); err != nil || !ok {
			t.Errorf("first insert: %v %v", ok, err)
		}
		return state.Advance(t.Context(), x, k, state.PhaseAssigned, state.PhaseAccepted, t0)
	})
	tx(t, db, func(x *state.Tx) error {
		if ok, err := state.InsertAssigned(t.Context(), x, assignment("run_a", 1, 1)); err != nil || ok {
			t.Errorf("second insert: %v %v", ok, err)
		}
		return nil
	})
	if got, _ := state.RunBy(t.Context(), db.Reader(), k); got.Phase != state.PhaseAccepted {
		t.Errorf("the redelivered order reset the phase to %s", got.Phase)
	}
}

func TestTransitionsAreCompareAndSet(t *testing.T) {
	db, _ := open(t)
	k := state.Key{RunID: "run_a", LeaseEpoch: 1}
	tx(t, db, func(x *state.Tx) error {
		_, err := state.InsertAssigned(t.Context(), x, assignment("run_a", 1, 1))
		return err
	})
	err := db.WithTx(t.Context(), func(x *state.Tx) error {
		return state.Advance(t.Context(), x, k, state.PhaseAccepted, state.PhaseStarting, t0)
	})
	if !errors.Is(err, state.ErrChanged) {
		t.Errorf("advancing from the wrong phase: %v", err)
	}
	tx(t, db, func(x *state.Tx) error {
		if err := state.Advance(t.Context(), x, k, state.PhaseAssigned, state.PhaseAccepted, t0); err != nil {
			return err
		}
		if err := state.Advance(t.Context(), x, k, state.PhaseAccepted, state.PhaseStarting, t0); err != nil {
			return err
		}
		return state.SetRunning(t.Context(), x, k, `{"runtime":"tmux"}`, t0.Add(time.Second))
	})
	got, _ := state.RunBy(t.Context(), db.Reader(), k)
	if got.Phase != state.PhaseRunning || got.Handle != `{"runtime":"tmux"}` || got.StartedAt == nil || !got.StartedAt.Equal(t0.Add(time.Second)) {
		t.Errorf("run = %+v", got)
	}
	// started is recorded once.
	err = db.WithTx(t.Context(), func(x *state.Tx) error { return state.SetStarted(t.Context(), x, k, "h", t0) })
	if !errors.Is(err, state.ErrChanged) {
		t.Errorf("SetStarted twice: %v", err)
	}
	tx(t, db, func(x *state.Tx) error {
		return state.Close(t.Context(), x, k, []state.Phase{state.PhaseRunning, state.PhaseStopping}, state.PhaseStopped, false, t0)
	})
	if got, _ := state.RunBy(t.Context(), db.Reader(), k); got.Phase != state.PhaseStopped || got.Phase.Live() {
		t.Errorf("run = %+v", got)
	}
}

func TestOccupiedCountsOnlyThePhasesThatHoldCapacity(t *testing.T) {
	db, _ := open(t)
	tx(t, db, func(x *state.Tx) error {
		for i, phase := range []state.Phase{state.PhaseAssigned, state.PhaseAccepted, state.PhaseRunning, state.PhaseStopped, state.PhaseStopUnverified} {
			id := string(rune('a' + i))
			k := state.Key{RunID: id, LeaseEpoch: 1}
			if _, err := state.InsertAssigned(t.Context(), x, assignment(id, 1, uint64(i+1))); err != nil {
				return err
			}
			if phase != state.PhaseAssigned {
				if err := state.Close(t.Context(), x, k, []state.Phase{state.PhaseAssigned}, phase, false, t0); err != nil {
					return err
				}
			}
		}
		return nil
	})
	load, err := state.Occupied(t.Context(), db.Reader())
	if err != nil || load != (state.Load{Runs: 3, CPU: 6, MemoryMB: 12288}) {
		t.Errorf("load = %+v (%v)", load, err)
	}
	live, _ := state.LiveRuns(t.Context(), db.Reader())
	if len(live) != 4 || live[0].RunID != "a" {
		t.Errorf("live = %d runs", len(live))
	}
}

func TestCursorOnlyGoesForward(t *testing.T) {
	db, _ := open(t)
	if n, err := state.Cursor(t.Context(), db.Reader()); err != nil || n != 0 {
		t.Fatalf("empty cursor = %d (%v)", n, err)
	}
	for _, seq := range []uint64{5, 3, 9, 9, 10} {
		tx(t, db, func(x *state.Tx) error { return state.SetCursor(t.Context(), x, seq) })
	}
	if n, _ := state.Cursor(t.Context(), db.Reader()); n != 10 {
		t.Errorf("cursor = %d", n)
	}
	tx(t, db, func(x *state.Tx) error { return state.SetCursor(t.Context(), x, 4) })
	if n, _ := state.Cursor(t.Context(), db.Reader()); n != 10 {
		t.Errorf("cursor went back to %d", n)
	}
}

func TestStopOrdersKeepTheFirstReason(t *testing.T) {
	db, _ := open(t)
	k := state.Key{RunID: "run_a", LeaseEpoch: 1}
	tx(t, db, func(x *state.Tx) error {
		if ok, err := state.AddStopOrder(t.Context(), x, state.StopOrder{Key: k, Reason: "first", Source: "controller", CreatedAt: t0}); err != nil || !ok {
			t.Errorf("first: %v %v", ok, err)
		}
		if ok, err := state.AddStopOrder(t.Context(), x, state.StopOrder{Key: k, Reason: "second", Source: "worker", CreatedAt: t0}); err != nil || ok {
			t.Errorf("second: %v %v", ok, err)
		}
		return nil
	})
	o, err := state.StopOrderOf(t.Context(), db.Reader(), k)
	if err != nil || o.Reason != "first" || o.DoneAt != nil {
		t.Fatalf("order = %+v (%v)", o, err)
	}
	tx(t, db, func(x *state.Tx) error { return state.CompleteStopOrder(t.Context(), x, k, t0.Add(time.Second)) })
	if o, _ := state.StopOrderOf(t.Context(), db.Reader(), k); o.DoneAt == nil {
		t.Error("the order is not done")
	}
	if _, err := state.StopOrderOf(t.Context(), db.Reader(), state.Key{RunID: "x", LeaseEpoch: 1}); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown order: %v", err)
	}
}

func TestJournalSeqNeverRepeatsAndFencedRunsAreLeftOut(t *testing.T) {
	db, _ := open(t)
	a, b := state.Key{RunID: "run_a", LeaseEpoch: 1}, state.Key{RunID: "run_b", LeaseEpoch: 1}
	var seqs []uint64
	tx(t, db, func(x *state.Tx) error {
		for _, id := range []string{"a", "b"} {
			if _, err := state.InsertAssigned(t.Context(), x, assignment("run_"+id, 1, 1)); err != nil {
				return err
			}
		}
		for _, k := range []state.Key{a, b, a} {
			seq, err := state.AppendJournal(t.Context(), x, state.JournalRow{Epoch: "E1", Key: k, Kind: "started", Payload: []byte("{}"), CreatedAt: t0})
			if err != nil {
				return err
			}
			seqs = append(seqs, seq)
		}
		return nil
	})
	if seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("seqs = %v", seqs)
	}

	tx(t, db, func(x *state.Tx) error { return state.DeleteJournal(t.Context(), x, seqs) })
	tx(t, db, func(x *state.Tx) error {
		seq, err := state.AppendJournal(t.Context(), x, state.JournalRow{Epoch: "E2", Key: a, Kind: "exited", Payload: []byte("{}"), CreatedAt: t0})
		if seq != 4 {
			t.Errorf("seq after deleting everything = %d: a seq was reused", seq)
		}
		if _, err := state.AppendJournal(t.Context(), x, state.JournalRow{Epoch: "E2", Key: b, Kind: "exited", Payload: []byte("{}"), CreatedAt: t0}); err != nil {
			return err
		}
		return err
	})

	tx(t, db, func(x *state.Tx) error { _, err := state.Fence(t.Context(), x, a, "CF_STALE_LEASE"); return err })
	pending, err := state.PendingJournal(t.Context(), db.Reader(), 10)
	if err != nil || len(pending) != 1 || pending[0].Key != b || pending[0].Epoch != "E2" || pending[0].Seq != 5 {
		t.Errorf("pending = %+v (%v)", pending, err)
	}
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM journal`).Scan(&n); err != nil || n != 2 {
		t.Errorf("the fenced row was dropped: %d rows (%v)", n, err)
	}
}

func TestAFailedTransactionLeavesNothing(t *testing.T) {
	db, _ := open(t)
	err := db.WithTx(context.Background(), func(x *state.Tx) error {
		if _, err := state.InsertAssigned(t.Context(), x, assignment("run_a", 1, 1)); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("no error")
	}
	if live, _ := state.LiveRuns(t.Context(), db.Reader()); len(live) != 0 {
		t.Errorf("%d rows stayed", len(live))
	}
}

func TestTheStateIsPrivateToTheUserOfTheDaemon(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // the point of the test // made by someone else, open to all
		t.Fatal(err)
	}
	db, err := state.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tx(t, db, func(x *state.Tx) error {
		_, err := state.InsertAssigned(t.Context(), x, assignment("run_a", 1, 1))
		return err
	})
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "state.db"): 0o600, filepath.Join(dir, "state.db-wal"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", path, info, err, want)
		}
	}
}

func TestReconcileState_AdoptUnfenceOrphanAndPendingStops(t *testing.T) {
	db, _ := open(t)
	ctx := t.Context()
	a := assignment("run_a", 1, 1)
	a.Phase = state.PhaseRunning
	started := t0
	a.StartedAt = &started
	var inserted []bool
	for range 2 {
		tx(t, db, func(x *state.Tx) error {
			ok, err := state.InsertAdopted(ctx, x, a)
			inserted = append(inserted, ok)
			return err
		})
	}
	if !inserted[0] || inserted[1] {
		t.Errorf("inserted = %v", inserted)
	}
	got, err := state.RunBy(ctx, db.Reader(), a.Key)
	if err != nil || !got.Adopted || got.Phase != state.PhaseRunning || got.StartedAt == nil {
		t.Fatalf("adopted = %+v, %v", got, err)
	}
	tx(t, db, func(x *state.Tx) error { _, err := state.Fence(ctx, x, a.Key, "CF_INVALID_TRANSITION"); return err })
	if err := db.WithTx(ctx, func(x *state.Tx) error { return state.Unfence(ctx, x, a.Key, "CF_STALE_LEASE") }); !errors.Is(err, state.ErrChanged) {
		t.Errorf("unfencing another code: %v", err)
	}
	tx(t, db, func(x *state.Tx) error { return state.Unfence(ctx, x, a.Key, "CF_INVALID_TRANSITION") })
	tx(t, db, func(x *state.Tx) error { _, err := state.MarkOrphaned(ctx, x, a.Key, t0); return err })
	if got, _ := state.RunBy(ctx, db.Reader(), a.Key); got.FencedCode != "" || got.OrphanedAt == nil {
		t.Errorf("after unfence and orphan: %+v", got)
	}
	tx(t, db, func(x *state.Tx) error {
		_, err := state.AddStopOrder(ctx, x, state.StopOrder{Key: a.Key, Reason: "cancel", Source: "controller", CreatedAt: t0})
		return err
	})
	pending, err := state.PendingStopOrders(ctx, db.Reader())
	if err != nil || len(pending) != 1 || pending[0].Reason != "cancel" {
		t.Fatalf("pending stops = %+v, %v", pending, err)
	}
	tx(t, db, func(x *state.Tx) error { return state.CompleteStopOrder(ctx, x, a.Key, t0) })
	if pending, _ := state.PendingStopOrders(ctx, db.Reader()); len(pending) != 0 {
		t.Errorf("pending after done = %+v", pending)
	}
	tx(t, db, func(x *state.Tx) error {
		_, err := state.AppendJournal(ctx, x, state.JournalRow{Epoch: "e", Key: a.Key, Kind: "started", Payload: []byte("{}"), CreatedAt: t0})
		return err
	})
	counts, err := state.JournalCounts(ctx, db.Reader())
	if err != nil || counts[a.Key] != 1 {
		t.Errorf("counts = %v, %v", counts, err)
	}
	if last, err := state.LastJournalSeq(ctx, db.Reader()); err != nil || last != 1 {
		t.Errorf("last seq = %d, %v", last, err)
	}
	tx(t, db, func(x *state.Tx) error { return state.DeleteJournalOf(ctx, x, a.Key) })
	if counts, _ := state.JournalCounts(ctx, db.Reader()); len(counts) != 0 {
		t.Errorf("counts after dropping = %v", counts)
	}
}

func TestEventCursorsResumeWhereTheyWere(t *testing.T) {
	db, _ := open(t)
	k := state.Key{RunID: "run_1", LeaseEpoch: 1}
	if c, err := state.EventCursorOf(t.Context(), db.Reader(), k); err != nil || c.Consumed != 0 || c.Model != "" {
		t.Fatalf("fresh cursor = %+v, %v", c, err)
	}
	tx(t, db, func(x *state.Tx) error {
		return state.SaveEventCursor(t.Context(), x, state.EventCursor{Key: k, Consumed: 42, Model: "m", InputRequired: true})
	})
	if c, err := state.EventCursorOf(t.Context(), db.Reader(), k); err != nil || c.Consumed != 42 || c.Model != "m" || !c.InputRequired {
		t.Errorf("cursor = %+v, %v", c, err)
	}
}
