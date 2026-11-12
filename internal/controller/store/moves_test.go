package store

import (
	"errors"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

func (f *leaseFixture) move(id string, run domain.ID) Move {
	f.t.Helper()
	m := Move{ID: domain.ID(id), TaskID: "task_1", SourceRunID: run, Stage: MoveRequested, CreatedAt: epoch0, UpdatedAt: epoch0}
	f.tx(func(tx *Tx) error { return tx.InsertMove(f.t.Context(), m) })
	return m
}

func (f *leaseFixture) advanceMove(id domain.ID, from, to MoveStage, change MoveChange) error {
	f.t.Helper()
	return f.db.WithTx(f.t.Context(), func(tx *Tx) error {
		return tx.AdvanceMove(f.t.Context(), id, from, to, change, epoch0.Add(time.Minute))
	})
}

func (f *leaseFixture) moveByID(id domain.ID) Move {
	f.t.Helper()
	var m Move
	if err := f.db.Read(f.t.Context(), func(q Querier) (err error) {
		m, err = MoveByID(f.t.Context(), q, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func TestMoves_StageOnlyAdvancesAndOnlyFromTheStageSeen(t *testing.T) {
	f := newLeaseFixture(t)
	run := f.queuedRun(1)
	cp := f.insertCheckpoint(checkpointOf("cp_1", run.ID))
	m := f.move("op_1", run.ID)

	if err := f.advanceMove(m.ID, MoveRequested, MoveCheckpointed, MoveChange{CheckpointID: cp.ID}); err != nil {
		t.Fatal(err)
	}
	if got := f.moveByID(m.ID); got.Stage != MoveCheckpointed || got.CheckpointID != cp.ID {
		t.Fatalf("move = %+v", got)
	}
	// A caller that still sees the old stage loses the race and changes nothing.
	if err := f.advanceMove(m.ID, MoveRequested, MoveCheckpointing, MoveChange{}); err == nil {
		t.Error("a stage that was left was advanced from")
	}
	if err := f.advanceMove(m.ID, MoveCheckpointed, MoveCheckpointing, MoveChange{}); err == nil {
		t.Error("a stage went back")
	}
	if err := f.advanceMove(m.ID, MoveRequested, MoveCheckpointed, MoveChange{}); !errors.Is(err, ErrMoveStage) {
		t.Errorf("repeating a stage = %v, want ErrMoveStage", err)
	}
	// A change that sets nothing keeps the checkpoint.
	if err := f.advanceMove(m.ID, MoveCheckpointed, MoveResuming, MoveChange{TargetRunID: "run_b"}); err != nil {
		t.Fatal(err)
	}
	if got := f.moveByID(m.ID); got.CheckpointID != cp.ID || got.TargetRunID != "run_b" || got.FinishedAt != nil {
		t.Fatalf("move = %+v", got)
	}
	if err := f.advanceMove(m.ID, MoveResuming, MoveDone, MoveChange{}); err != nil {
		t.Fatal(err)
	}
	if got := f.moveByID(m.ID); got.FinishedAt == nil || got.Active() {
		t.Fatalf("move = %+v", got)
	}
	if err := f.advanceMove(m.ID, MoveDone, MoveFailed, MoveChange{}); err == nil {
		t.Error("a move that is over was failed")
	}
}

func TestMoves_OneActivePerTaskAndTheCheckpointIsARoot(t *testing.T) {
	f := newLeaseFixture(t)
	run := f.queuedRun(1)
	cp := f.insertCheckpoint(checkpointOf("cp_1", run.ID))
	m := f.move("op_1", run.ID)

	second := Move{ID: "op_2", TaskID: m.TaskID, SourceRunID: run.ID, Stage: MoveRequested, CreatedAt: epoch0, UpdatedAt: epoch0}
	if err := f.db.WithTx(t.Context(), func(tx *Tx) error { return tx.InsertMove(t.Context(), second) }); err == nil {
		t.Fatal("a Task got a second move while the first is not over")
	}
	if err := f.advanceMove(m.ID, MoveRequested, MoveCheckpointed, MoveChange{CheckpointID: cp.ID}); err != nil {
		t.Fatal(err)
	}
	roots := func() []domain.ID {
		var out []domain.ID
		if err := f.db.Read(t.Context(), func(q Querier) (err error) {
			out, err = ActiveMoveCheckpoints(t.Context(), q)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := roots(); len(got) != 1 || got[0] != cp.ID {
		t.Fatalf("roots = %v", got)
	}
	if err := f.advanceMove(m.ID, MoveCheckpointed, MoveFailed, MoveChange{Failure: "gone"}); err != nil {
		t.Fatal(err)
	}
	if got := roots(); len(got) != 0 {
		t.Fatalf("a move that is over still holds %v", got)
	}
	if err := f.db.WithTx(t.Context(), func(tx *Tx) error { return tx.InsertMove(t.Context(), second) }); err != nil {
		t.Fatalf("a Task whose move is over could not have another: %v", err)
	}
}
