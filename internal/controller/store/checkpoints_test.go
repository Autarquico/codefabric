package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

func (f *leaseFixture) insertCheckpoint(c Checkpoint) Checkpoint {
	f.t.Helper()
	var stored Checkpoint
	f.tx(func(tx *Tx) (err error) {
		stored, err = tx.InsertCheckpoint(f.t.Context(), c)
		return err
	})
	return stored
}

func (f *leaseFixture) checkpoint(id domain.ID) (Checkpoint, error) {
	f.t.Helper()
	var c Checkpoint
	err := f.db.Read(f.t.Context(), func(q Querier) (err error) {
		c, err = CheckpointByID(f.t.Context(), q, id)
		return err
	})
	return c, err
}

func checkpointOf(id string, run domain.ID) Checkpoint {
	return Checkpoint{Checkpoint: domain.Checkpoint{
		ID: domain.ID(id), RunID: run, Trigger: domain.CheckpointManual,
		SourceRef: "refs/cf/checkpoints/" + id, CreatedAt: epoch0.Add(time.Minute),
	}}
}

func TestCheckpoints_SeqCountsFromOneWithinEachRun(t *testing.T) {
	f := newLeaseFixture(t)
	a, b := f.queuedRun(1).ID, f.queuedRun(2).ID
	var got []int
	for _, c := range []Checkpoint{checkpointOf("cp_1", a), checkpointOf("cp_2", a), checkpointOf("cp_3", b), checkpointOf("cp_4", a)} {
		got = append(got, f.insertCheckpoint(c).Seq)
	}
	if want := []int{1, 2, 1, 3}; !slices.Equal(got, want) {
		t.Fatalf("seq = %v, want %v", got, want)
	}
}

func TestCheckpoints_RoundTripKeepsEverything(t *testing.T) {
	f := newLeaseFixture(t)
	in := checkpointOf("cp_1", f.queuedRun(1).ID)
	in.Trigger = domain.CheckpointMove
	in.Summary = domain.CheckpointSummary{Progress: "p", Next: "n", Decisions: "d", Commands: "c"}
	in.NativeState = &domain.NativeState{Adapter: "claude-code", AdapterVersion: "1.4.0", Digest: "sha256:abc"}
	in.ArtifactIDs = []domain.ID{"art_1", "art_2"}
	in.Excluded = []string{".env", "keys/a.pem"}
	stored := f.insertCheckpoint(in)
	in.Seq = 1

	got, err := f.checkpoint("cp_1")
	if err != nil {
		t.Fatal(err)
	}
	in.CreatedAt, got.CreatedAt = in.CreatedAt.UTC(), got.CreatedAt.UTC()
	if !reflect.DeepEqual(got, in) || !reflect.DeepEqual(stored, in) {
		t.Fatalf("got %+v, stored %+v, want %+v", got, stored, in)
	}
}

func TestCheckpoints_WithoutNativeStateHasNone(t *testing.T) {
	f := newLeaseFixture(t)
	f.insertCheckpoint(checkpointOf("cp_1", f.queuedRun(1).ID))
	got, err := f.checkpoint("cp_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.NativeState != nil || len(got.Excluded) != 0 || len(got.ArtifactIDs) != 0 {
		t.Fatalf("got %+v, want no native state, exclusions or artifacts", got)
	}
}

func TestCheckpoints_UnknownIDIsNotFound(t *testing.T) {
	f := newLeaseFixture(t)
	if _, err := f.checkpoint("cp_x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCheckpoints_RefusesWhatTheTableDoesNotAllow(t *testing.T) {
	f := newLeaseFixture(t)
	run := f.queuedRun(1).ID
	badTrigger := checkpointOf("cp_1", run)
	badTrigger.Trigger = "whenever"
	for name, c := range map[string]Checkpoint{
		"unknown run":     checkpointOf("cp_1", "run_nope"),
		"unknown trigger": badTrigger,
	} {
		err := f.db.WithTx(f.t.Context(), func(tx *Tx) error {
			_, err := tx.InsertCheckpoint(f.t.Context(), c)
			return err
		})
		if err == nil {
			t.Errorf("%s: the checkpoint was stored", name)
		}
	}
}

func TestCheckpoints_ListByRunAndByTask(t *testing.T) {
	f := newLeaseFixture(t)
	a, b := f.queuedRun(1).ID, f.queuedRun(2).ID
	f.insertCheckpoint(checkpointOf("cp_1", a))
	later := checkpointOf("cp_2", b)
	later.CreatedAt = epoch0.Add(2 * time.Minute)
	f.insertCheckpoint(later)
	f.insertCheckpoint(checkpointOf("cp_3", a))

	var byRun, byTask []Checkpoint
	if err := f.db.Read(f.t.Context(), func(q Querier) (err error) {
		if byRun, err = CheckpointsOfRun(f.t.Context(), q, a); err != nil {
			return err
		}
		byTask, err = CheckpointsOfTask(f.t.Context(), q, "task_1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ids := func(list []Checkpoint) (out []domain.ID) {
		for _, c := range list {
			out = append(out, c.ID)
		}
		return out
	}
	if got, want := ids(byRun), []domain.ID{"cp_1", "cp_3"}; !slices.Equal(got, want) || byRun[1].Seq != 2 {
		t.Fatalf("by run = %v, want %v", got, want)
	}
	if got, want := ids(byTask), []domain.ID{"cp_1", "cp_3", "cp_2"}; !slices.Equal(got, want) {
		t.Fatalf("by task = %v, want %v", got, want)
	}
}
