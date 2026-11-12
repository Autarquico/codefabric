package gc_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/app/gc"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// failingRelay refuses to delete the refs named in fail, until fail is emptied.
type failingRelay struct {
	*fakeRelay
	fail []string
}

func (r *failingRelay) DeleteRefs(ctx context.Context, p domain.ID, refs []string) error {
	if slices.ContainsFunc(refs, func(n string) bool { return slices.Contains(r.fail, n) }) {
		return errors.New("relay down")
	}
	return r.fakeRelay.DeleteRefs(ctx, p, refs)
}

// The refs go only once the rows are committed: a row that is still there when
// the ref goes would point at nothing if the transaction rolled back.
func TestCheckpointGC_RefsAreDroppedAfterTheRowsCommitted(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	id, _ := f.checkpoint("run_live", 1, domain.CheckpointDefer, 1, "")

	rowThere := true
	gone, err := f.db.DeleteCheckpoints(t.Context(), []domain.ID{id}, false, func([]domain.ID) error {
		rowThere = f.hasCheckpoint(id)
		return nil
	})
	if err != nil || len(gone) != 1 {
		t.Fatalf("gone=%v err=%v", gone, err)
	}
	if rowThere {
		t.Error("the refs were dropped before the rows were committed")
	}
}

// A ref that fails to go after its row did is an orphan the next pass collects.
func TestCheckpointGC_ARefThatFailedToGoIsCollectedByTheNextPass(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	id := domain.ID("cp_01J00000000000000000000001")
	ref := domain.CheckpointRef(id)
	f.tx(func(tx *store.Tx) error {
		if _, err := tx.InsertCheckpoint(t.Context(), store.Checkpoint{Checkpoint: domain.Checkpoint{
			ID: id, RunID: "run_live", Trigger: domain.CheckpointDefer, SourceRef: ref, CreatedAt: epoch,
		}}); err != nil {
			return err
		}
		return tx.PublishCheckpoint(t.Context(), "run_live", id, nil, nil, domain.CheckpointSummary{}, epoch)
	})
	f.checkpoint("run_live", 2, domain.CheckpointDrain, 2, "")
	rel := &failingRelay{fakeRelay: f.relayWith(ref), fail: []string{ref}}

	svc := f.service(gc.Policy{}, gc.WithRelay(rel))
	if _, err := svc.Run(t.Context(), false); err == nil {
		t.Fatal("pass 1: the failed delete was not reported")
	}
	if f.hasCheckpoint(id) || !slices.Contains(rel.refs[relayProject], ref) {
		t.Fatalf("after pass 1: row=%v refs=%v, want no row and the orphan ref", f.hasCheckpoint(id), rel.refs[relayProject])
	}

	rel.fail = nil
	if _, err := svc.Run(t.Context(), false); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if len(rel.refs[relayProject]) != 0 {
		t.Errorf("after pass 2 the orphan ref is still there: %v", rel.refs[relayProject])
	}
}

// A ref whose row exists is never an orphan.
func TestCheckpointGC_ARefWithARowIsNotAnOrphan(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	_, ref := f.checkpoint("run_live", 1, domain.CheckpointDrain, 1, "")
	rel := f.relayWith(ref)
	if _, err := f.service(gc.Policy{}, gc.WithRelay(rel)).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if len(rel.deleted) != 0 {
		t.Errorf("deleted %v", rel.deleted)
	}
}
