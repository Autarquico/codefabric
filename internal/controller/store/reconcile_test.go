package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

func TestOpen_UpgradesFrom0105MakesOnlyTheUnambiguousCancelledRunsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	seedUntil(t, path, 105)
	sqlDB, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// run_2: cancelled after it started, runtime active (no RunStopped yet): pending.
	// run_3: cancelled after it started and stopped: left to the reconciliation.
	// run_4: failed in preparation after its session started: stays none.
	// run_5: lost while running: pending.
	for _, stmt := range []string{
		`INSERT INTO runs (id, short_n, task_id, attempt, lineage_kind, spec, spec_digest, source_commit, state, publication,
		     evaluation, runtime, created_at, started_at) VALUES ('run_2', 2, 'task_1', 2, 'initial', '{}', 'd', 'c', 'cancelled', 'none', 'none', 'active', 1, 2)`,
		`INSERT INTO runs (id, short_n, task_id, attempt, lineage_kind, spec, spec_digest, source_commit, state, publication,
		     evaluation, runtime, created_at, started_at) VALUES ('run_3', 3, 'task_1', 3, 'initial', '{}', 'd', 'c', 'cancelled', 'none', 'none', 'stopped', 1, 2)`,
		`INSERT INTO runs (id, short_n, task_id, attempt, lineage_kind, spec, spec_digest, source_commit, state, end_reason, publication,
		     evaluation, runtime, created_at, started_at) VALUES ('run_4', 4, 'task_1', 4, 'initial', '{}', 'd', 'c', 'cancelled', 'environment', 'none', 'none', 'active', 1, 2)`,
		`INSERT INTO runs (id, short_n, task_id, attempt, lineage_kind, spec, spec_digest, source_commit, state, publication,
		     evaluation, runtime, created_at, started_at) VALUES ('run_5', 5, 'task_1', 5, 'initial', '{}', 'd', 'c', 'lost', 'none', 'none', 'active', 1, 2)`,
	} {
		if _, err := sqlDB.ExecContext(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = sqlDB.Close()

	db, err := Open(t.Context(), path, Options{Clock: clock.NewFake(epoch0)})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	want := map[domain.ID]domain.Publication{
		"run_1": domain.PublicationNone, "run_2": domain.PublicationPending, "run_3": domain.PublicationNone,
		"run_4": domain.PublicationNone, "run_5": domain.PublicationPending,
	}
	for id, pub := range want {
		var r Run
		if err := db.Read(t.Context(), func(q Querier) (err error) {
			r, err = RunByID(t.Context(), q, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if r.Publication != pub || r.HarnessRan != nil || r.LineageParent != "" {
			t.Errorf("%s: publication %s, harness_ran %v, parent %q; want %s, nil, none", id, r.Publication, r.HarnessRan, r.LineageParent, pub)
		}
	}
}

// acceptedRun is a Run in preparing under an accepted lease of alpha.
func (f *leaseFixture) acceptedRun(n int) (Run, Lease) {
	f.t.Helper()
	ctx := f.t.Context()
	run := f.queuedRun(n)
	l := f.assign(run, f.alpha, v1.Capacity{CPU: 1})
	run = f.run(run.ID)
	f.tx(func(tx *Tx) error {
		if err := tx.AcceptLease(ctx, l.ID, epoch0); err != nil {
			return err
		}
		return tx.AdvanceRun(ctx, run, domain.RunPreparing)
	})
	l.State = domain.LeaseAccepted
	return f.run(run.ID), l
}

func stopOf(w Worker, run Run, l Lease) WorkerCommand {
	return WorkerCommand{WorkerID: w.ID, Type: domain.CommandStop, RunID: run.ID, LeaseID: l.ID, Epoch: l.Epoch, Payload: []byte(`{}`), CreatedAt: epoch0}
}

func TestStops_OnePerWorkerRunAndEpochAndTheyNameTheirRun(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run, l := f.acceptedRun(1)
	var created []bool
	for range 2 {
		f.tx(func(tx *Tx) error {
			ok, err := tx.EnqueueStopOnce(ctx, stopOf(f.alpha, run, l))
			created = append(created, ok)
			return err
		})
	}
	if !created[0] || created[1] {
		t.Errorf("created = %v, want a stop once", created)
	}
	// The index is the guard behind EnqueueStopOnce.
	err := f.db.WithTx(ctx, func(tx *Tx) error { _, err := tx.EnqueueCommand(ctx, stopOf(f.alpha, run, l)); return err })
	if err == nil {
		t.Error("a second stop of the same Run and epoch was stored")
	}
	bare := stopOf(f.alpha, run, l)
	bare.LeaseID = ""
	err = f.db.WithTx(ctx, func(tx *Tx) error { _, err := tx.EnqueueCommand(ctx, bare); return err })
	if err == nil {
		t.Error("a stop with no lease was stored")
	}
}

func TestOpenStops_LiveEpochUntilStoppedOtherEpochsUntilAcknowledged(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run, l := f.acceptedRun(1)
	old := stopOf(f.alpha, run, l)
	old.Epoch, old.LeaseID = 7, l.ID // an orphan under an epoch that is not the Run's
	f.tx(func(tx *Tx) error {
		if _, err := tx.EnqueueStopOnce(ctx, stopOf(f.alpha, run, l)); err != nil {
			return err
		}
		_, err := tx.EnqueueStopOnce(ctx, old)
		return err
	})
	open := func() []uint64 {
		var stops []WorkerCommand
		if err := f.db.Read(ctx, func(q Querier) (err error) {
			stops, err = OpenStopsOf(ctx, q, f.alpha.ID, 0, 10)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var epochs []uint64
		for _, s := range stops {
			epochs = append(epochs, s.Epoch)
		}
		return epochs
	}
	if got := open(); len(got) != 2 {
		t.Fatalf("open stops %v, want both", got)
	}
	f.tx(func(tx *Tx) error { _, err := tx.AckCommandsThrough(ctx, f.alpha.ID, 10, epoch0); return err })
	if got := open(); len(got) != 1 || got[0] != l.Epoch {
		t.Fatalf("open stops after the ACK %v, want the live epoch's", got)
	}
	r := f.run(run.ID)
	f.tx(func(tx *Tx) error { return tx.StopRuntime(ctx, r) })
	if got := open(); len(got) != 0 {
		t.Errorf("open stops once the runtime stopped %v", got)
	}
}

func TestOrphans_DetectedOnceAndPreservedOnce(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run, _ := f.acceptedRun(1)
	var news []bool
	for range 2 {
		f.tx(func(tx *Tx) error {
			ok, err := tx.InsertOrphan(ctx, f.alpha.ID, run.ID, 1, epoch0)
			news = append(news, ok)
			return err
		})
	}
	if !news[0] || news[1] {
		t.Errorf("new = %v", news)
	}
	f.tx(func(tx *Tx) error { return tx.PreserveOrphan(ctx, f.alpha.ID, run.ID, 1, sealed1, epoch0) })
	err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.PreserveOrphan(ctx, f.alpha.ID, run.ID, 1, sealed1, epoch0) })
	if !errors.Is(err, ErrNoRowChanged) {
		t.Errorf("preserving twice: %v", err)
	}
	var o Orphan
	if err := f.db.Read(ctx, func(q Querier) (err error) { o, err = OrphanOf(ctx, q, f.alpha.ID, run.ID, 1); return err }); err != nil {
		t.Fatal(err)
	}
	if o.Preserved != OrphanInRelay || o.ResultCommit != sealed1 || o.PreservedAt == nil {
		t.Errorf("orphan = %+v", o)
	}
	if err := f.db.Read(ctx, func(q Querier) error { _, err := OrphanOf(ctx, q, f.beta.ID, run.ID, 1); return err }); !errors.Is(err, ErrNotFound) {
		t.Errorf("another Worker's orphan: %v", err)
	}
}

func TestLateReports_AreCompareAndSetsThatNeverRestartARuntime(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run, _ := f.acceptedRun(1)
	f.tx(func(tx *Tx) error { return tx.FinishRun(ctx, run, domain.RunCancelled, "", epoch0) })
	run = f.run(run.ID)
	f.tx(func(tx *Tx) error { return tx.NoteStart(ctx, run, epoch0, `{"runtime":"tmux"}`) })
	if err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.NoteStart(ctx, run, epoch0, "x") }); !errors.Is(err, ErrNoRowChanged) {
		t.Errorf("a second late start: %v", err)
	}
	run = f.run(run.ID)
	if run.State != domain.RunCancelled || run.Runtime != domain.RuntimeActive || run.StartedAt == nil {
		t.Fatalf("after a late start %+v", run)
	}
	f.tx(func(tx *Tx) error { return tx.MarkExecuted(ctx, run) })
	code := 3
	f.tx(func(tx *Tx) error { return tx.NoteExit(ctx, run, &code) })
	run = f.run(run.ID)
	if run.Publication != domain.PublicationPending || run.HarnessRan == nil || !*run.HarnessRan || *run.ExitCode != 3 {
		t.Errorf("after a late exit %+v", run)
	}
	if err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.SetHarnessRan(ctx, run, false) }); !errors.Is(err, ErrNoRowChanged) {
		t.Errorf("harness_ran is set once: %v", err)
	}
	f.tx(func(tx *Tx) error { return tx.StopRuntime(ctx, run) })
	run = f.run(run.ID)
	if err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.NoteStart(ctx, run, epoch0, "x") }); !errors.Is(err, ErrNoRowChanged) {
		t.Errorf("a start after the runtime stopped: %v", err)
	}
}

func TestReconcileQueries_OpenAcceptedLeasesAndPendingPublications(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run1, l1 := f.acceptedRun(1)
	_, l2 := f.exitedRun(2)
	queued := f.queuedRun(3)
	f.assign(queued, f.alpha, v1.Capacity{CPU: 1}) // pending: not one to reconcile
	var (
		leases []Lease
		pubs   []Run
	)
	if err := f.db.Read(ctx, func(q Querier) (err error) {
		if leases, err = OpenAcceptedLeases(ctx, q, f.alpha.ID, "", 10); err != nil {
			return err
		}
		pubs, err = PendingPublicationsOf(ctx, q, f.alpha.ID, "", 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(leases) != 2 || leases[0].ID != min(l1.ID, l2.ID) {
		t.Errorf("open accepted leases %+v", leases)
	}
	if len(pubs) != 1 || pubs[0].ID == run1.ID {
		t.Errorf("pending publications %+v", pubs)
	}
	f.tx(func(tx *Tx) error { return tx.ReleaseReservation(ctx, l1.ID, "x", epoch0) })
	if err := f.db.Read(ctx, func(q Querier) (err error) {
		leases, err = OpenAcceptedLeases(ctx, q, f.alpha.ID, "", 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].ID != l2.ID {
		t.Errorf("after releasing one %+v", leases)
	}
	var w Worker
	if err := f.db.Read(ctx, func(q Querier) (err error) { w, err = WorkerByID(ctx, q, f.alpha.ID); return err }); err != nil {
		t.Fatal(err)
	}
	if w.LiveLeases != 1 {
		t.Errorf("live leases = %d", w.LiveLeases)
	}
}
