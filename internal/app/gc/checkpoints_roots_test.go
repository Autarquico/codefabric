package gc_test

import (
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/app/gc"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// A checkpoint that a finished move or a continuing Run names is a root: the
// foreign keys keep its row, so the GC leaves it, its ref and the passes after.
func TestCheckpointGC_KeepsTheCheckpointsThatAMoveOrAContinuingRunNames(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	moved, movedRef := f.checkpoint("run_live", 1, domain.CheckpointDefer, 1, "")
	parent, parentRef := f.checkpoint("run_live", 2, domain.CheckpointDefer, 2, "")
	plain, plainRef := f.checkpoint("run_live", 3, domain.CheckpointDefer, 3, "")
	_, lastRef := f.checkpoint("run_live", 4, domain.CheckpointDrain, 4, "")
	f.runOfTask("task_live", "run_next")
	f.tx(func(tx *store.Tx) error {
		if _, err := tx.Exec(t.Context(), `UPDATE runs SET lineage_checkpoint_id = ? WHERE id = 'run_next'`, string(parent)); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(),
			`INSERT INTO moves (id, task_id, source_run_id, stage, checkpoint_id, created_at, updated_at)
			 VALUES ('move_1', 'task_live', 'run_live', 'done', ?, 0, 0)`, string(moved))
		return err
	})
	rel := f.relayWith(movedRef, parentRef, plainRef, lastRef)

	svc := f.service(gc.Policy{}, gc.WithRelay(rel))
	for pass := 1; pass <= 2; pass++ {
		if _, err := svc.Run(t.Context(), false); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if !f.hasCheckpoint(moved) || !f.hasCheckpoint(parent) || f.hasCheckpoint(plain) {
		t.Errorf("rows kept: moved=%v parent=%v plain=%v", f.hasCheckpoint(moved), f.hasCheckpoint(parent), f.hasCheckpoint(plain))
	}
	if want := []string{plainRef}; !slices.Equal(rel.deleted, want) {
		t.Errorf("deleted refs %v, want %v", rel.deleted, want)
	}
	if len(rel.collected) == 0 {
		t.Error("the relay sweep did not run after the checkpoints")
	}
}

// A row that a Run started to name between the listing and the delete stays,
// and no ref is dropped for it.
func TestCheckpointGC_DropsNoRefForARowThatCannotGo(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	f.checkpoint("run_live", 1, domain.CheckpointDefer, 1, "")
	f.runOfTask("task_live", "run_next")
	f.tx(func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET lineage_checkpoint_id = 'cp_run_live_1' WHERE id = 'run_next'`)
		return err
	})

	dropped := false
	gone, err := f.db.DeleteCheckpoints(t.Context(), []domain.ID{"cp_run_live_1"}, false, func([]domain.ID) error { dropped = true; return nil })
	if err != nil || len(gone) != 0 || dropped {
		t.Errorf("gone=%v dropped=%v err=%v, want nothing", gone, dropped, err)
	}
	if !f.hasCheckpoint("cp_run_live_1") {
		t.Error("the row went")
	}
}
