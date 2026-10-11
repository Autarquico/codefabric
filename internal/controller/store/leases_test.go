package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// leaseFixture has two online Workers and a Task with queued Runs.
type leaseFixture struct {
	*workerFixture
	alpha, beta Worker
}

func newLeaseFixture(t *testing.T) *leaseFixture {
	t.Helper()
	f := &leaseFixture{workerFixture: newWorkerFixture(t)}
	f.alpha = f.worker(1, "alpha", domain.WorkerOnline)
	f.beta = f.worker(2, "beta", domain.WorkerOnline)
	ctx := t.Context()
	f.tx(func(tx *Tx) error {
		p := Project{ID: "project_1", Short: 1, Name: "p", Repo: "r", DefaultBranch: "main", CreatedAt: epoch0}
		if err := tx.InsertProject(ctx, p); err != nil {
			return err
		}
		return tx.InsertTask(ctx, Task{ID: "task_1", Short: 1, ProjectID: p.ID, Name: "t", Prompt: "p", State: domain.TaskQueued, CreatedBy: domain.CreatorHuman, CreatedAt: epoch0})
	})
	return f
}

func (f *leaseFixture) tx(fn func(*Tx) error) {
	f.t.Helper()
	if err := f.db.WithTx(f.t.Context(), fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *leaseFixture) queuedRun(n int) Run {
	f.t.Helper()
	run := Run{
		ID: domain.ID(fmt.Sprintf("run_%c", 'a'+n)), Short: uint64(n), //nolint:gosec // a small test counter
		TaskID: "task_1", Attempt: n, Lineage: domain.LineageInitial,
		Spec: []byte(`{}`), SpecDigest: "d", SourceCommit: "c", State: domain.RunQueued, Publication: domain.PublicationNone,
		Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, CreatedAt: epoch0.Add(time.Duration(n) * time.Second),
	}
	f.tx(func(tx *Tx) error { return tx.InsertRun(f.t.Context(), run) })
	return run
}

func (f *leaseFixture) run(id domain.ID) Run {
	f.t.Helper()
	var r Run
	if err := f.db.Read(f.t.Context(), func(q Querier) (err error) {
		r, err = RunByID(f.t.Context(), q, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// assign gives the Run to the Worker under a pending lease and an open reservation.
func (f *leaseFixture) assign(run Run, w Worker, amount v1.Capacity) Lease {
	f.t.Helper()
	ctx := f.t.Context()
	epoch := run.LeaseEpoch + 1
	l := Lease{
		ID: domain.ID(fmt.Sprintf("lease_%s%d", run.ID, epoch)), RunID: run.ID, WorkerID: w.ID, Epoch: epoch,
		State: domain.LeasePending, CreatedAt: epoch0, AcceptBy: epoch0.Add(domain.Defaults().LeaseAcceptBy),
	}
	f.tx(func(tx *Tx) error {
		if err := tx.InsertLease(ctx, l); err != nil {
			return err
		}
		if err := tx.AssignRun(ctx, run, l.ID, w.ID, epoch); err != nil {
			return err
		}
		return tx.InsertReservation(ctx, Reservation{LeaseID: l.ID, RunID: run.ID, WorkerID: w.ID, Amount: amount, CreatedAt: epoch0})
	})
	return l
}

func TestLeases_ARunHasOneLiveLeaseAndEachEpochIsIssuedOnce(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run := f.queuedRun(1)
	first := f.assign(run, f.alpha, v1.Capacity{CPU: 2})

	second := Lease{ID: "lease_second", RunID: run.ID, WorkerID: f.beta.ID, Epoch: 2, State: domain.LeasePending, CreatedAt: epoch0, AcceptBy: epoch0.Add(time.Minute)}
	if err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.InsertLease(ctx, second) }); err == nil {
		t.Error("a second live lease of the Run was stored")
	}

	f.tx(func(tx *Tx) error {
		return tx.CloseLease(ctx, first.ID, domain.LeaseExpired, domain.LeaseCloseAcceptTimeout, "", epoch0.Add(time.Minute))
	})
	reused := second
	reused.ID, reused.Epoch = "lease_reused", 1
	if err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.InsertLease(ctx, reused) }); err == nil {
		t.Error("epoch 1 of the Run was issued twice")
	}
	if err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.InsertLease(ctx, second) }); err != nil {
		t.Errorf("a new lease once the first closed: %v", err)
	}
}

func TestRunLease_AssignClearAndAdvanceAreCompareAndSets(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run := f.queuedRun(1)
	l := f.assign(run, f.alpha, v1.Capacity{})

	got := f.run(run.ID)
	if got.LeaseID != l.ID || got.LeaseEpoch != 1 || got.WorkerID != f.alpha.ID || got.LeaseState != domain.LeasePending || got.QueuedReason != "" {
		t.Fatalf("assigned Run = %+v", got)
	}

	// A Run that has its lease cannot be assigned again from a stale read.
	err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.AssignRun(ctx, run, "lease_x", f.beta.ID, 2) })
	if !errors.Is(err, ErrNoRowChanged) {
		t.Errorf("assigning a Run that has a lease: %v", err)
	}
	// The epoch of another lease matches neither a clear, an advance nor a finish.
	stale := got
	stale.LeaseEpoch = 7
	for name, step := range map[string]func(*Tx) error{
		"clear":   func(tx *Tx) error { return tx.ClearRunLease(ctx, stale) },
		"advance": func(tx *Tx) error { return tx.AdvanceRun(ctx, stale, domain.RunPreparing) },
		"finish":  func(tx *Tx) error { return tx.FinishRun(ctx, stale, domain.RunCancelled, "", epoch0) },
	} {
		if err := f.db.WithTx(ctx, step); !errors.Is(err, ErrNoRowChanged) {
			t.Errorf("%s with a stale epoch: %v", name, err)
		}
	}

	f.tx(func(tx *Tx) error { return tx.ClearRunLease(ctx, got) })
	cleared := f.run(run.ID)
	if cleared.WorkerID != "" || cleared.LeaseID != "" || cleared.LeaseEpoch != 1 {
		t.Errorf("cleared Run = %+v, want no Worker or lease and the epoch kept", cleared)
	}
}

func TestSetQueuedReasons_OnlyWritesWhatChangesAndOnlyOnRunsStillWaiting(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	waiting, assigned := f.queuedRun(1), f.queuedRun(2)
	f.assign(assigned, f.alpha, v1.Capacity{})

	f.tx(func(tx *Tx) error {
		return tx.SetQueuedReasons(ctx, []Run{waiting, assigned}, string(domain.CodeNoEligibleWorker))
	})
	if got := f.run(waiting.ID).QueuedReason; got != string(domain.CodeNoEligibleWorker) {
		t.Errorf("waiting Run: %q", got)
	}
	if got := f.run(assigned.ID).QueuedReason; got != "" {
		t.Errorf("a Run with a lease got the reason %q", got)
	}
	var changes int
	f.tx(func(tx *Tx) error {
		if err := tx.SetQueuedReasons(ctx, []Run{waiting}, string(domain.CodeNoEligibleWorker)); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT changes()`).Scan(&changes)
	})
	if changes != 0 {
		t.Errorf("repeating the same reason changed %d rows", changes)
	}
}

func TestQueuedRuns_ServeHigherPriorityFirstThenOldest(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	old, newer, urgent, leased := f.queuedRun(1), f.queuedRun(2), f.queuedRun(3), f.queuedRun(4)
	f.assign(leased, f.alpha, v1.Capacity{})
	f.tx(func(tx *Tx) error {
		// Priority belongs to the Task: give the urgent Run a Task of its own.
		if err := tx.InsertTask(ctx, Task{
			ID: "task_2", Short: 2, ProjectID: "project_1", Name: "u", Prompt: "p", Priority: 5,
			State: domain.TaskQueued, CreatedBy: domain.CreatorHuman, CreatedAt: epoch0,
		}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE runs SET task_id = 'task_2', attempt = 1 WHERE id = ?`, string(urgent.ID))
		return err
	})

	var got []domain.ID
	if err := f.db.Read(ctx, func(q Querier) error {
		runs, err := QueuedRuns(ctx, q)
		for _, r := range runs {
			got = append(got, r.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := []domain.ID{urgent.ID, old.ID, newer.ID}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("queue = %v, want %v (a Run with a lease is not in it)", got, want)
	}
}

func TestReservations_ReleaseOnceAndFreeIsWhatTheOpenOnesLeave(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	f.tx(func(tx *Tx) error {
		return tx.RecordHeartbeat(ctx, f.alpha.ID, WorkerReport{Epoch: "e"},
			v1.WorkerResources{Allocatable: v1.Capacity{CPU: 8, MemoryMB: 16384, DiskMB: 1000}}, v1.Health{Status: v1.HealthOK}, epoch0)
	})
	l1 := f.assign(f.queuedRun(1), f.alpha, v1.Capacity{CPU: 2, MemoryMB: 4096})
	f.assign(f.queuedRun(2), f.alpha, v1.Capacity{CPU: 3, MemoryMB: 1024, DiskMB: 100})

	w := f.read(f.alpha.ID)
	if w.Reservations != 2 || w.Reserved != (v1.Capacity{CPU: 5, MemoryMB: 5120, DiskMB: 100}) {
		t.Fatalf("reserved %+v in %d reservations", w.Reserved, w.Reservations)
	}
	if want := (v1.Capacity{CPU: 3, MemoryMB: 11264, DiskMB: 900}); w.Resources.Free != want {
		t.Errorf("Free = %+v, want %+v", w.Resources.Free, want)
	}

	f.tx(func(tx *Tx) error { return tx.ReleaseReservation(ctx, l1.ID, "lease_expired", epoch0) })
	if err := f.db.WithTx(ctx, func(tx *Tx) error { return tx.ReleaseReservation(ctx, l1.ID, "lease_expired", epoch0) }); !errors.Is(err, ErrNoRowChanged) {
		t.Errorf("releasing twice: %v", err)
	}
	w = f.read(f.alpha.ID)
	if w.Reservations != 1 || w.Resources.Free.CPU != 5 {
		t.Errorf("after the release: %d reservations, free %+v", w.Reservations, w.Resources.Free)
	}
	if got := f.read(f.beta.ID); got.Reservations != 0 || got.Reserved != (v1.Capacity{}) {
		t.Errorf("another Worker holds %+v", got.Reserved)
	}
}

func TestReservations_FreeNeverGoesBelowZero(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	f.assign(f.queuedRun(1), f.alpha, v1.Capacity{CPU: 6})
	f.tx(func(tx *Tx) error {
		// The Worker now contributes less than it has reserved.
		return tx.RecordHeartbeat(ctx, f.alpha.ID, WorkerReport{Epoch: "e"},
			v1.WorkerResources{Allocatable: v1.Capacity{CPU: 4}}, v1.Health{Status: v1.HealthOK}, epoch0)
	})
	if got := f.read(f.alpha.ID).Resources.Free.CPU; got != 0 {
		t.Errorf("Free CPU = %d", got)
	}
}

func TestCommands_SeqIsPerWorkerMonotonicAndRollbackGivesItBack(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	// The foreign keys need a Run and a lease for each assign.
	for i := 1; i <= 3; i++ {
		f.assign(f.queuedRun(i), f.alpha, v1.Capacity{})
	}
	f.assign(f.queuedRun(4), f.beta, v1.Capacity{})

	seqs := map[string][]uint64{}
	for _, c := range []struct {
		name string
		w    Worker
		run  domain.ID
	}{{"alpha", f.alpha, "run_b"}, {"alpha", f.alpha, "run_c"}, {"beta", f.beta, "run_e"}} {
		f.tx(func(tx *Tx) error {
			l := "lease_" + string(c.run) + "1"
			cmd, err := tx.EnqueueCommand(ctx, WorkerCommand{
				WorkerID: c.w.ID, Type: domain.CommandAssign, RunID: c.run, LeaseID: domain.ID(l), Epoch: 1,
				Payload: []byte(`{}`), CreatedAt: epoch0,
			})
			seqs[c.name] = append(seqs[c.name], cmd.Seq)
			return err
		})
	}
	if len(seqs["alpha"]) != 2 || seqs["alpha"][0] != 1 || seqs["alpha"][1] != 2 || seqs["beta"][0] != 1 {
		t.Errorf("seqs = %v, want 1,2 for alpha and 1 for beta", seqs)
	}

	boom := errors.New("boom")
	err := f.db.WithTx(ctx, func(tx *Tx) error {
		c, err := tx.EnqueueCommand(ctx, WorkerCommand{
			WorkerID: f.alpha.ID, Type: domain.CommandAssign, RunID: "run_d", LeaseID: "lease_run_d1", Epoch: 1,
			Payload: []byte(`{}`), CreatedAt: epoch0,
		})
		if err != nil || c.Seq != 3 {
			t.Errorf("seq %d (%v)", c.Seq, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if got := f.read(f.alpha.ID).LastCommandSeq; got != 2 {
		t.Errorf("LastCommandSeq = %d after the rollback", got)
	}
}

func TestCommands_AreDeliveredUntilAcknowledgedOrCancelled(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	var leases []Lease
	for i := 1; i <= 4; i++ {
		l := f.assign(f.queuedRun(i), f.alpha, v1.Capacity{})
		leases = append(leases, l)
		f.tx(func(tx *Tx) error {
			_, err := tx.EnqueueCommand(ctx, WorkerCommand{
				WorkerID: f.alpha.ID, Type: domain.CommandAssign, RunID: l.RunID, LeaseID: l.ID,
				Epoch: l.Epoch, Payload: []byte(`{"n":1}`), CreatedAt: epoch0,
			})
			return err
		})
	}
	pending := func(after uint64) []uint64 {
		var out []uint64
		if err := f.db.Read(ctx, func(q Querier) error {
			cmds, err := UndeliveredCommands(ctx, q, f.alpha.ID, after, 10)
			for _, c := range cmds {
				out = append(out, c.Seq)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	equal := func(got []uint64, want ...uint64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
	equal(pending(0), 1, 2, 3, 4)
	equal(pending(0), 1, 2, 3, 4) // reading acknowledges nothing
	equal(pending(2), 3, 4)

	f.tx(func(tx *Tx) error { return tx.CancelAssign(ctx, leases[1].ID, epoch0) })
	f.tx(func(tx *Tx) error { return tx.AckAssign(ctx, leases[1].ID, epoch0) }) // both marks coexist
	equal(pending(0), 1, 3, 4)

	ack := func(upTo uint64, at time.Time) (through uint64) {
		f.tx(func(tx *Tx) (err error) {
			through, err = tx.AckCommandsThrough(ctx, f.alpha.ID, upTo, at)
			return err
		})
		return through
	}
	if got := ack(3, epoch0.Add(time.Second)); got != 3 {
		t.Errorf("ack through 3 answered %d", got)
	}
	if got := ack(3, epoch0.Add(time.Hour)); got != 3 { // idempotent
		t.Errorf("repeated ack answered %d", got)
	}
	if got := ack(1, epoch0.Add(time.Hour)); got != 3 { // a lower one later does not lower the watermark
		t.Errorf("a lower ack answered %d", got)
	}
	equal(pending(0), 4)

	cmd, err := commandOf(f, leases[0].ID)
	if err != nil || cmd.AckedAt == nil || !cmd.AckedAt.Equal(epoch0.Add(time.Second)) {
		t.Errorf("command of lease 1 = %+v (%v): a repeated ACK must keep the first time", cmd, err)
	}
	cmd, err = commandOf(f, leases[1].ID)
	if err != nil || cmd.AckedAt == nil || cmd.CancelledAt == nil {
		t.Errorf("command of lease 2 = %+v (%v): acknowledged and cancelled both", cmd, err)
	}
}

// commandOf is the assign command of a lease.
func commandOf(f *leaseFixture, lease domain.ID) (WorkerCommand, error) {
	return scanCommand(f.db.Reader().QueryRowContext(f.t.Context(),
		`SELECT `+commandColumns+` FROM worker_commands WHERE lease_id = ? AND type = 'assign'`, string(lease)))
}

func TestCommands_AckAndCancelOfALeaseWithoutAssignAreErrors(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	l := f.assign(f.queuedRun(1), f.alpha, v1.Capacity{})
	for name, step := range map[string]func(*Tx) error{
		"ack":    func(tx *Tx) error { return tx.AckAssign(ctx, l.ID, epoch0) },
		"cancel": func(tx *Tx) error { return tx.CancelAssign(ctx, l.ID, epoch0) },
	} {
		if err := f.db.WithTx(ctx, step); !errors.Is(err, ErrNoRowChanged) {
			t.Errorf("%s of a lease that has no assign: %v", name, err)
		}
	}
}

func TestMissesSince_CountRejectionsAndTimeoutsAndKeepTheLatestPerRunAndWorker(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run, other := f.queuedRun(1), f.queuedRun(2)
	rejected := f.assign(run, f.alpha, v1.Capacity{})
	f.tx(func(tx *Tx) error {
		if err := tx.CloseLease(ctx, rejected.ID, domain.LeaseRejected, string(domain.CodeWorkerCapacity), "full", epoch0.Add(5*time.Second)); err != nil {
			return err
		}
		return tx.ReleaseReservation(ctx, rejected.ID, "lease_rejected", epoch0)
	})
	f.tx(func(tx *Tx) error { return tx.ClearRunLease(ctx, f.run(run.ID)) })
	timedOut := f.assign(other, f.beta, v1.Capacity{})
	f.tx(func(tx *Tx) error {
		if err := tx.CloseLease(ctx, timedOut.ID, domain.LeaseExpired, domain.LeaseCloseAcceptTimeout, "", epoch0.Add(7*time.Second)); err != nil {
			return err
		}
		return tx.ReleaseReservation(ctx, timedOut.ID, "lease_expired", epoch0)
	})
	f.tx(func(tx *Tx) error { return tx.ClearRunLease(ctx, f.run(other.ID)) })
	// A lease that expired because its Run ended is no Worker's fault.
	ended := f.queuedRun(3)
	endedLease := f.assign(ended, f.alpha, v1.Capacity{})
	f.tx(func(tx *Tx) error {
		if err := tx.CloseLease(ctx, endedLease.ID, domain.LeaseExpired, domain.LeaseCloseRunEnded, "", epoch0.Add(8*time.Second)); err != nil {
			return err
		}
		return tx.ReleaseReservation(ctx, endedLease.ID, "run_ended", epoch0)
	})

	var got []Miss
	read := func(since time.Time) {
		got = nil
		if err := f.db.Read(ctx, func(q Querier) (err error) {
			got, err = MissesSince(ctx, q, since)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	read(epoch0)
	if len(got) != 2 {
		t.Fatalf("misses = %+v, want the rejection and the timeout", got)
	}
	byRun := map[domain.ID]Miss{}
	for _, m := range got {
		byRun[m.RunID] = m
	}
	if m := byRun[run.ID]; m.WorkerID != f.alpha.ID || !m.At.Equal(epoch0.Add(5*time.Second)) || m.Code != domain.CodeWorkerCapacity {
		t.Errorf("rejection = %+v", m)
	}
	if m := byRun[other.ID]; m.WorkerID != f.beta.ID || !m.At.Equal(epoch0.Add(7*time.Second)) || m.Code != domain.CodeNoEligibleWorker {
		t.Errorf("timeout = %+v", m)
	}
	read(epoch0.Add(7 * time.Second))
	if len(got) != 0 {
		t.Errorf("a miss at the cut-off counts: %+v", got)
	}
}

func TestProjectWorkers_AreThoseThatAcceptedARunOfTheProject(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	accepted, pending := f.queuedRun(1), f.queuedRun(2)
	l := f.assign(accepted, f.alpha, v1.Capacity{})
	f.assign(pending, f.beta, v1.Capacity{})
	f.tx(func(tx *Tx) error { return tx.AcceptLease(ctx, l.ID, epoch0) })

	var got map[domain.ID]bool
	if err := f.db.Read(ctx, func(q Querier) (err error) {
		got, err = ProjectWorkers(ctx, q, "project_1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[f.alpha.ID] {
		t.Errorf("ProjectWorkers = %v, want only alpha", got)
	}
}

func TestLeaseInvariant_ARunsLeaseIsALiveLeaseOfTheSameEpochAndWorker(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	run := f.queuedRun(1)
	f.queuedRun(2)
	l := f.assign(run, f.alpha, v1.Capacity{})
	f.tx(func(tx *Tx) error {
		if err := tx.CloseLease(ctx, l.ID, domain.LeaseExpired, domain.LeaseCloseAcceptTimeout, "", epoch0); err != nil {
			return err
		}
		return tx.ReleaseReservation(ctx, l.ID, "lease_expired", epoch0)
	})
	f.tx(func(tx *Tx) error { return tx.ClearRunLease(ctx, f.run(run.ID)) })
	f.assign(f.run(run.ID), f.beta, v1.Capacity{})

	var broken int
	f.tx(func(tx *Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM runs r WHERE r.lease_id IS NOT NULL AND NOT EXISTS (
			     SELECT 1 FROM leases l WHERE l.id = r.lease_id AND l.state IN ('pending', 'accepted')
			     AND l.epoch = r.lease_epoch AND l.worker_id = r.worker_id AND l.run_id = r.id)`).Scan(&broken)
	})
	if broken != 0 {
		t.Errorf("%d Runs point at a lease that is not theirs", broken)
	}
}

func TestOpen_UpgradesAPhase1DatabaseWithoutLeases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	seedUntil(t, path, 102)

	db, err := Open(t.Context(), path, Options{Clock: clock.NewFake(epoch0)})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for table, want := range map[string]int{"runs": 1, "leases": 0, "reservations": 0, "worker_commands": 0} {
		if got := count(t, db, table); got != want {
			t.Errorf("%s has %d rows after the upgrade, want %d", table, got, want)
		}
	}
	var queued []Run
	if err := db.Read(t.Context(), func(q Querier) (err error) {
		queued, err = QueuedRuns(t.Context(), q)
		return err
	}); err != nil || len(queued) != 1 || queued[0].LeaseEpoch != 0 {
		t.Errorf("queue after the upgrade = %+v (%v)", queued, err)
	}
}

// The query of the recent misses reads the leases closed lately through an
// index, not the whole table.
func TestMissesSince_UsesTheIndexOnClosedAt(t *testing.T) {
	f := newLeaseFixture(t)
	rows, err := f.db.Reader().QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+missesQuery,
		epoch0.UnixNano(), string(domain.LeaseRejected), string(domain.LeaseExpired), domain.LeaseCloseAcceptTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "; ")
	if strings.Contains(joined, "SCAN leases") || !strings.Contains(joined, "leases_closed_at") {
		t.Errorf("plan = %s, want a search on leases_closed_at", joined)
	}
}
