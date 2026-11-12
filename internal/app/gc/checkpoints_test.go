package gc_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/app/gc"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// checkpoint stores and publishes a checkpoint of run taken at minute, with the
// native state native if any, and returns its ID and ref.
func (f *fixture) checkpoint(run domain.ID, n int, trigger domain.CheckpointTrigger, minute int, native blob.Digest) (domain.ID, string) {
	f.t.Helper()
	id := f.reservation(run, n, trigger, minute)
	var ns *domain.NativeState
	if native != "" {
		ns = &domain.NativeState{Adapter: "claude", AdapterVersion: "1", Digest: string(native)}
	}
	f.tx(func(tx *store.Tx) error {
		return tx.PublishCheckpoint(f.t.Context(), run, id, ns, nil, domain.CheckpointSummary{}, epoch)
	})
	return id, domain.CheckpointRef(id)
}

// reservation stores a checkpoint that was never published.
func (f *fixture) reservation(run domain.ID, n int, trigger domain.CheckpointTrigger, minute int) domain.ID {
	f.t.Helper()
	id := domain.ID(fmt.Sprintf("cp_%s_%d", run, n))
	f.tx(func(tx *store.Tx) error {
		_, err := tx.InsertCheckpoint(f.t.Context(), store.Checkpoint{Checkpoint: domain.Checkpoint{
			ID: id, RunID: run, Trigger: trigger, SourceRef: domain.CheckpointRef(id),
			CreatedAt: epoch.Add(time.Duration(minute) * time.Minute),
		}})
		return err
	})
	return id
}

func (f *fixture) hasCheckpoint(id domain.ID) bool {
	f.t.Helper()
	var n int
	if err := f.db.Reader().QueryRowContext(f.t.Context(), `SELECT COUNT(*) FROM checkpoints WHERE id = ?`, string(id)).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

func (f *fixture) relayWith(refs ...string) *fakeRelay {
	return &fakeRelay{refs: map[domain.ID][]string{relayProject: slices.Clone(refs)}}
}

func (f *fixture) runOfTask(task, id domain.ID) {
	f.t.Helper()
	f.tx(func(tx *store.Tx) error {
		return tx.InsertRun(f.t.Context(), store.Run{
			ID: id, Short: 100, TaskID: task, Attempt: 2, Lineage: domain.LineageInitial,
			Spec: []byte(`{}`), SpecDigest: "d", SourceCommit: "c", State: domain.RunQueued, Publication: domain.PublicationNone,
			Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, CreatedAt: epoch,
		})
	})
}

func TestCheckpointGC_KeepsTheLatestOfEveryLiveTaskAndDeletesTheRest(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	old, oldRef := f.checkpoint("run_live", 1, domain.CheckpointManual, 1, "")
	mid, midRef := f.checkpoint("run_live", 2, domain.CheckpointDefer, 2, "")
	last, lastRef := f.checkpoint("run_live", 3, domain.CheckpointDrain, 3, "")
	rel := f.relayWith(oldRef, midRef, lastRef, snapshotRef)

	rep, err := f.service(gc.Policy{}, gc.WithRelay(rel)).Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if f.hasCheckpoint(old) || f.hasCheckpoint(mid) || !f.hasCheckpoint(last) {
		t.Errorf("rows kept: old=%v mid=%v last=%v", f.hasCheckpoint(old), f.hasCheckpoint(mid), f.hasCheckpoint(last))
	}
	if want := []string{oldRef, midRef}; !slices.Equal(rel.deleted, want) {
		t.Errorf("deleted refs %v, want %v", rel.deleted, want)
	}
	if got := items(rep)["checkpoints"]; got != 2 {
		t.Errorf("checkpoints = %d, want 2", got)
	}
	if left := rel.refs[relayProject]; !slices.Contains(left, snapshotRef) || !slices.Contains(left, lastRef) {
		t.Errorf("refs left %v: the snapshot and the latest checkpoint must stay", left)
	}
}

func TestCheckpointGC_TheLatestOfATaskIsTheLatestOfAnyOfItsRuns(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	f.runOfTask("task_live", "run_live2")
	older, _ := f.checkpoint("run_live", 1, domain.CheckpointManual, 5, "")
	last, ref := f.checkpoint("run_live2", 1, domain.CheckpointManual, 9, "")

	if _, err := f.service(gc.Policy{}, gc.WithRelay(f.relayWith(ref))).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if f.hasCheckpoint(older) || !f.hasCheckpoint(last) {
		t.Errorf("older=%v last=%v", f.hasCheckpoint(older), f.hasCheckpoint(last))
	}
}

func TestCheckpointGC_AnArchivedTaskKeepsItsCheckpointsAsLongAsItsBranches(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "gone", 31)
	f.taskRuns(11, "recent", 29)
	gone, goneRef := f.checkpoint("run_gone", 1, domain.CheckpointManual, 1, "")
	recent, recentRef := f.checkpoint("run_recent", 1, domain.CheckpointManual, 1, "")
	rel := f.relayWith(goneRef, recentRef)

	if _, err := f.service(gc.Policy{}, gc.WithRelay(rel)).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if f.hasCheckpoint(gone) || !f.hasCheckpoint(recent) {
		t.Errorf("gone=%v recent=%v", f.hasCheckpoint(gone), f.hasCheckpoint(recent))
	}
	if !slices.Equal(rel.deleted, []string{goneRef}) {
		t.Errorf("deleted %v", rel.deleted)
	}
}

func TestCheckpointGC_KeepsTheLastNIntervalCheckpointsOfEachRun(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "a", 0)
	f.taskRuns(11, "b", 0)
	want := map[domain.ID]bool{}
	for _, run := range []domain.ID{"run_a", "run_b"} {
		for n := 1; n <= 4; n++ {
			id, _ := f.checkpoint(run, n, domain.CheckpointInterval, n, "")
			want[id] = n >= 3
		}
	}
	// Older than the intervals and not one of them: nothing but the latest-of-Task root could keep it.
	manual, _ := f.checkpoint("run_a", 5, domain.CheckpointManual, 0, "")

	if _, err := f.service(gc.Policy{AutoCheckpoints: 2}, gc.WithRelay(f.relayWith())).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	for id, keep := range want {
		if got := f.hasCheckpoint(id); got != keep {
			t.Errorf("%s kept = %v, want %v", id, got, keep)
		}
	}
	if f.hasCheckpoint(manual) {
		t.Error("a manual checkpoint that is not the latest of its Task was kept")
	}
}

func TestCheckpointGC_AutoCheckpointsDefaultToFive(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "a", 0)
	var ids []domain.ID
	for n := 1; n <= 7; n++ {
		id, _ := f.checkpoint("run_a", n, domain.CheckpointInterval, n, "")
		ids = append(ids, id)
	}
	if _, err := f.service(gc.Policy{}, gc.WithRelay(f.relayWith())).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		if want := i >= 2; f.hasCheckpoint(id) != want {
			t.Errorf("checkpoint %d kept = %v, want %v", i+1, f.hasCheckpoint(id), want)
		}
	}
}

// The move table does not exist yet: a stub root stands for "the checkpoint a
// move that is not over resumes from".
func TestCheckpointGC_ARootKeepsWhatItNames(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	held, heldRef := f.checkpoint("run_live", 1, domain.CheckpointMove, 1, "")
	free, freeRef := f.checkpoint("run_live", 2, domain.CheckpointManual, 2, "")
	_, lastRef := f.checkpoint("run_live", 3, domain.CheckpointManual, 3, "")
	moveRoot := func(_ context.Context, _ store.Querier, keep func(domain.ID)) error { keep(held); return nil }
	rel := f.relayWith(heldRef, freeRef, lastRef)

	if _, err := f.service(gc.Policy{}, gc.WithRelay(rel), gc.WithCheckpointRoots(moveRoot)).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if !f.hasCheckpoint(held) || f.hasCheckpoint(free) {
		t.Errorf("held=%v free=%v", f.hasCheckpoint(held), f.hasCheckpoint(free))
	}
	if slices.Contains(rel.deleted, heldRef) {
		t.Error("the ref of a checkpoint a move holds was deleted")
	}
}

func TestCheckpointGC_ADeletedCheckpointFreesItsNativeStateUnlessAnotherNamesIt(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	own := f.blob("own native state", 2*day)
	shared := f.blob("shared native state", 2*day)
	f.checkpoint("run_live", 1, domain.CheckpointManual, 1, own)
	f.checkpoint("run_live", 2, domain.CheckpointManual, 2, shared)
	f.checkpoint("run_live", 3, domain.CheckpointManual, 3, shared) // the latest

	if _, err := f.service(gc.Policy{}, gc.WithRelay(f.relayWith())).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if f.has(own) {
		t.Error("the native state of a deleted checkpoint was kept")
	}
	if !f.has(shared) {
		t.Error("a native state another checkpoint names was deleted")
	}
}

func TestCheckpointGC_TheNativeStateOfACheckpointThatStaysIsNeverSwept(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	d := f.blob("native state", 30*day)
	f.checkpoint("run_live", 1, domain.CheckpointManual, 1, d)

	if _, err := f.service(gc.Policy{}).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if !f.has(d) {
		t.Error("the native state of the latest checkpoint was swept")
	}
}

func TestCheckpointGC_AReservationGoesAfterItsTermAndNotBefore(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	id := f.reservation("run_live", 1, domain.CheckpointManual, 0)
	ref := domain.CheckpointRef(id)
	rel := f.relayWith(ref)
	svc := f.service(gc.Policy{CheckpointReservation: 3 * day}, gc.WithRelay(rel))

	f.clk.Advance(3*day - time.Hour)
	if _, err := svc.Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if !f.hasCheckpoint(id) {
		t.Fatal("a reservation went before its term")
	}
	f.clk.Advance(2 * time.Hour)
	rep, err := svc.Run(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if f.hasCheckpoint(id) || !slices.Equal(rel.deleted, []string{ref}) {
		t.Errorf("reservation kept = %v, deleted refs %v", f.hasCheckpoint(id), rel.deleted)
	}
	if got := items(rep)["checkpoint_reservations"]; got != 1 {
		t.Errorf("checkpoint_reservations = %d", got)
	}
}

func TestCheckpointGC_AReservationOfARunStillCheckpointingStays(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	id := f.reservation("run_live", 1, domain.CheckpointManual, 0)
	f.exec(`UPDATE runs SET state = 'checkpointing', checkpoint_origin = 'running' WHERE id = 'run_live'`)
	f.clk.Advance(30 * day)
	if _, err := f.service(gc.Policy{}, gc.WithRelay(f.relayWith())).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if !f.hasCheckpoint(id) {
		t.Error("the reservation of a capture under way was dropped")
	}
}

func TestCheckpointGC_DryRunReportsAndChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	old, oldRef := f.checkpoint("run_live", 1, domain.CheckpointManual, 1, "")
	f.checkpoint("run_live", 2, domain.CheckpointManual, 2, "")
	rel := f.relayWith(oldRef)

	rep, err := f.service(gc.Policy{}, gc.WithRelay(rel)).Run(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got := items(rep)["checkpoints"]; got != 1 {
		t.Errorf("checkpoints = %d, want 1", got)
	}
	if !f.hasCheckpoint(old) || len(rel.deleted) != 0 {
		t.Error("a dry run deleted")
	}
}

func TestCheckpointGC_WithNoRelayNothingIsDeleted(t *testing.T) {
	f := newFixture(t)
	f.taskRuns(10, "live", 0)
	old, _ := f.checkpoint("run_live", 1, domain.CheckpointManual, 1, "")
	f.checkpoint("run_live", 2, domain.CheckpointManual, 2, "")
	if _, err := f.service(gc.Policy{}).Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if !f.hasCheckpoint(old) {
		t.Error("a row went while its ref could not")
	}
}
