package app_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

const acceptBy = 60 * time.Second

func TestScenario_SchedulerLeasesReservations_AcceptIdempotente(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	l := e.offer(run.ID, w)

	first, err := e.accept(w, l)
	must(t, err)
	if first.Lease.State != domain.LeaseAccepted || first.Lease.AcceptedAt == nil || first.Lease.Epoch != 1 {
		t.Fatalf("lease after accept = %+v", first.Lease)
	}
	got := e.storedRun(run.ID)
	if got.State != domain.RunPreparing || got.WorkerID != w.Principal.ID || got.LeaseID != l.ID {
		t.Errorf("Run after accept = %+v", got)
	}
	if task, err := e.app.Tasks.Get(t.Context(), run.TaskID, ""); err != nil || task.State != domain.TaskRunning {
		t.Errorf("Task = %+v (%v), want running", task.State, err)
	}

	// The same accept again, later, answers byte for byte the same and writes nothing.
	events := len(e.events())
	e.clock.Advance(time.Hour)
	second, err := e.accept(w, l)
	must(t, err)
	if a, b := jsonOf(t, first), jsonOf(t, second); a != b {
		t.Errorf("second answer differs:\n%s\n%s", a, b)
	}
	if len(e.events()) != events || len(e.events(domain.EventLeaseAccepted)) != 1 {
		t.Errorf("repeating the accept wrote events: %d LeaseAccepted", len(e.events(domain.EventLeaseAccepted)))
	}

	// An expired lease answers CF_STALE_LEASE.
	other := e.queuedRun("y")
	expiring := e.offer(other.ID, w)
	e.clock.Advance(acceptBy)
	_, err = e.app.Leases.SweepExpired(t.Context())
	must(t, err)
	_, err = e.accept(w, expiring)
	wantCode(t, err, domain.CodeStaleLease)
}

func TestAccept_TheBoundaryOfAcceptByIsExclusive(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	l := e.offer(e.queuedRun("x").ID, w)

	e.clock.Advance(acceptBy - time.Nanosecond)
	if _, err := e.accept(w, l); err != nil {
		t.Fatalf("accept one nanosecond before accept_by: %v", err)
	}

	late := e.offer(e.queuedRun("y").ID, w)
	e.clock.Advance(acceptBy)
	events := len(e.events())
	_, err := e.accept(w, late)
	wantCode(t, err, domain.CodeStaleLease)
	if got := e.storedLease(late.ID).State; got != domain.LeasePending || len(e.events()) != events {
		t.Errorf("a late accept wrote: lease %s, %d new events", got, len(e.events())-events)
	}
	if e.storedRun(string(late.RunID)).State != domain.RunQueued {
		t.Error("a late accept moved the Run")
	}
}

func TestAccept_ChecksTheLeaseBeforeTheStateAndTheEpochBeforeBoth(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.joinWorker("alpha"), e.joinWorker("beta")
	l := e.offer(e.queuedRun("x").ID, alpha)

	_, err := e.accept(beta, l)
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Leases.Accept(t.Context(), alpha, "lease_01ARZ3NDEKTSV4RRFFQ69G5FAV", v1.AcceptLeaseRequest{Epoch: 1})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Leases.Accept(t.Context(), alpha, "run_"+string(l.ID)[len("lease_"):], v1.AcceptLeaseRequest{Epoch: 1})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Leases.Accept(t.Context(), alpha, "nonsense", v1.AcceptLeaseRequest{Epoch: 1})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Leases.Accept(t.Context(), e.owner, string(l.ID), v1.AcceptLeaseRequest{Epoch: 1})
	wantCode(t, err, domain.CodeNotFound) // a client is not a Worker
	_, err = e.app.Leases.Accept(t.Context(), alpha, string(l.ID), v1.AcceptLeaseRequest{})
	wantCode(t, err, domain.CodeInvalidRequest)

	_, err = e.app.Leases.Accept(t.Context(), alpha, string(l.ID), v1.AcceptLeaseRequest{Epoch: 2})
	wantCode(t, err, domain.CodeStaleLease)
	_, err = e.accept(alpha, l)
	must(t, err)
	// Once accepted, the wrong epoch is still stale: the answer is not for whoever asks.
	_, err = e.app.Leases.Accept(t.Context(), alpha, string(l.ID), v1.AcceptLeaseRequest{Epoch: 2})
	wantCode(t, err, domain.CodeStaleLease)
}

func TestScenario_SchedulerLeasesReservations_SinCapacidad_Reject(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	l := e.offer(run.ID, w)
	if got := e.storedWorker(w); got.Reservations != 1 || got.Resources.Free.CPU != 14 {
		t.Fatalf("Worker holds %d reservations, free %+v", got.Reservations, got.Resources.Free)
	}

	e.clock.Advance(5 * time.Second)
	first, err := e.reject(w, l, domain.CodeWorkerCapacity)
	must(t, err)
	if first.Lease.State != domain.LeaseRejected || first.Lease.CloseCode != string(domain.CodeWorkerCapacity) ||
		first.Lease.ClosedAt == nil || first.Lease.CloseMessage != "no room" {
		t.Fatalf("lease after reject = %+v", first.Lease)
	}
	r := e.storedRun(run.ID)
	if r.State != domain.RunQueued || r.WorkerID != "" || r.LeaseID != "" || r.LeaseEpoch != 1 {
		t.Errorf("Run after reject = %+v: queued with no Worker or lease, and the epoch kept", r)
	}
	if got := e.storedWorker(w); got.Reservations != 0 || got.Resources.Free.CPU != 16 {
		t.Errorf("the reservation was not released: %d, free %+v", got.Reservations, got.Resources.Free)
	}
	if cmds := e.commands(w); len(cmds) != 0 {
		t.Errorf("the rejected assignment is still delivered: %+v", cmds)
	}
	if rejected := e.events(domain.EventLeaseRejected); len(rejected) != 1 || rejected[0].RunID != domain.ID(run.ID) || rejected[0].Actor.Kind != domain.ActorWorker {
		t.Errorf("LeaseRejected = %+v", rejected)
	}

	// Repeating it answers the same closure, keeps the first time and writes nothing.
	events := len(e.events())
	e.clock.Advance(time.Second)
	second, err := e.reject(w, l, domain.CodeWorkerCapacity)
	must(t, err)
	if jsonOf(t, first) != jsonOf(t, second) || len(e.events()) != events {
		t.Errorf("the repeated reject changed something:\n%s\n%s", jsonOf(t, first), jsonOf(t, second))
	}
}

func TestReject_StatesAndInputs(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")

	accepted := e.offer(e.queuedRun("a").ID, w)
	_, err := e.accept(w, accepted)
	must(t, err)
	_, err = e.reject(w, accepted, domain.CodeWorkerCapacity)
	wantCode(t, err, domain.CodeInvalidTransition)

	expired := e.offer(e.queuedRun("b").ID, w)
	e.clock.Advance(acceptBy)
	_, err = e.app.Leases.SweepExpired(t.Context())
	must(t, err)
	_, err = e.reject(w, expired, domain.CodeWorkerCapacity)
	wantCode(t, err, domain.CodeStaleLease)

	late := e.offer(e.queuedRun("c").ID, w)
	e.clock.Advance(acceptBy)
	_, err = e.reject(w, late, domain.CodeWorkerCapacity) // not swept yet, but past accept_by
	wantCode(t, err, domain.CodeStaleLease)

	pending := e.offer(e.queuedRun("d").ID, w)
	for name, code := range map[string]domain.Code{"not a reject code": domain.CodeStaleLease, "empty": ""} {
		if _, err := e.reject(w, pending, code); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			wantCode(t, err, domain.CodeInvalidRequest)
		}
	}
	_, err = e.app.Leases.Reject(t.Context(), w, string(pending.ID),
		v1.RejectLeaseRequest{Epoch: 1, Code: domain.CodeWorkerCapacity, Message: strings.Repeat("x", 513)})
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = e.app.Leases.Reject(t.Context(), w, string(pending.ID),
		v1.RejectLeaseRequest{Epoch: 2, Code: domain.CodeWorkerCapacity})
	wantCode(t, err, domain.CodeStaleLease)
	_, err = e.reject(w, pending, domain.CodeHarnessUnavailable)
	must(t, err)
	// A repeat with the wrong epoch is stale even though the lease is rejected.
	_, err = e.app.Leases.Reject(t.Context(), w, string(pending.ID),
		v1.RejectLeaseRequest{Epoch: 2, Code: domain.CodeHarnessUnavailable})
	wantCode(t, err, domain.CodeStaleLease)
}

func TestScenario_SchedulerLeasesReservations_Expiracion_Sweep(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	l := e.offer(run.ID, w)

	// A nanosecond before accept_by the lease is alive; at accept_by it expires.
	e.clock.Advance(acceptBy - time.Nanosecond)
	if n, err := e.app.Leases.SweepExpired(t.Context()); err != nil || n != 0 {
		t.Fatalf("sweep before accept_by expired %d (%v)", n, err)
	}
	if e.storedLease(l.ID).State != domain.LeasePending {
		t.Fatal("the lease is not pending before accept_by")
	}

	e.clock.Advance(time.Nanosecond)
	if n, err := e.app.Leases.SweepExpired(t.Context()); err != nil || n != 1 {
		t.Fatalf("sweep at accept_by expired %d (%v)", n, err)
	}
	got := e.storedLease(l.ID)
	if got.State != domain.LeaseExpired || got.CloseCode != domain.LeaseCloseAcceptTimeout || got.ClosedAt == nil {
		t.Errorf("lease = %+v", got)
	}
	r := e.storedRun(run.ID)
	if r.State != domain.RunQueued || r.WorkerID != "" || r.LeaseID != "" || r.LeaseEpoch != 1 {
		t.Errorf("Run = %+v: queued, with no Worker and its epoch kept", r)
	}
	if st := e.storedWorker(w); st.Reservations != 0 || st.Resources.Free.CPU != 16 {
		t.Errorf("the reservation was not released: %d, free %+v", st.Reservations, st.Resources.Free)
	}
	if cmds := e.commands(w); len(cmds) != 0 {
		t.Errorf("an expired assignment is still delivered: %+v", cmds)
	}
	if expired := e.events(domain.EventLeaseExpired); len(expired) != 1 || expired[0].Data["close_code"] != domain.LeaseCloseAcceptTimeout || expired[0].Actor.Kind != domain.ActorSystem {
		t.Errorf("LeaseExpired = %+v", expired)
	}

	// Sweeping again changes nothing.
	events := len(e.events())
	if n, err := e.app.Leases.SweepExpired(t.Context()); err != nil || n != 0 || len(e.events()) != events {
		t.Errorf("a second sweep expired %d (%v) and wrote %d events", n, err, len(e.events())-events)
	}
}

// Everything a lease creates or settles is one transaction.
func TestCreate_FailureOfTheLastStepLeavesNothingAndSkipsNoSeq(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	before := len(e.events())
	latest := e.db.Notifier().Latest()
	e.refuseEvents(domain.EventRunAssigned)

	_, err := e.tryOffer(run.ID, w)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
	for _, table := range []string{"leases", "reservations", "worker_commands"} {
		if n := e.count(table); n != 0 {
			t.Errorf("%s has %d rows after the rollback", table, n)
		}
	}
	r := e.storedRun(run.ID)
	if r.LeaseID != "" || r.WorkerID != "" || r.LeaseEpoch != 0 || r.State != domain.RunQueued {
		t.Errorf("Run = %+v", r)
	}
	if got := e.storedWorker(w).LastCommandSeq; got != 0 {
		t.Errorf("the failed transaction used the Seq %d", got)
	}
	if len(e.events()) != before || e.db.Notifier().Latest() != latest {
		t.Error("the rollback wrote or announced an event")
	}
}

func TestAccept_FailureOfTheLastStepLeavesTheLeasePending(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	l := e.offer(run.ID, w)
	taskBefore, err := e.app.Tasks.Get(t.Context(), run.TaskID, "")
	must(t, err)
	e.refuseEvents(domain.EventLeaseAccepted)

	if _, err := e.accept(w, l); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
	if got := e.storedLease(l.ID); got.State != domain.LeasePending || got.AcceptedAt != nil {
		t.Errorf("lease = %+v", got)
	}
	if got := e.storedRun(run.ID); got.State != domain.RunQueued {
		t.Errorf("Run = %s", got.State)
	}
	if task, _ := e.app.Tasks.Get(t.Context(), run.TaskID, ""); task.State != taskBefore.State {
		t.Errorf("Task = %s", task.State)
	}
	if cmds := e.commands(w); len(cmds) != 1 {
		t.Errorf("the assignment must still be delivered: %+v", cmds)
	}
}

func TestReject_FailureOfTheLastStepLeavesTheLeasePending(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	l := e.offer(e.queuedRun("x").ID, w)
	e.refuseEvents(domain.EventLeaseRejected)

	if _, err := e.reject(w, l, domain.CodeWorkerCapacity); err == nil {
		t.Fatal("rejected")
	}
	if got := e.storedLease(l.ID); got.State != domain.LeasePending {
		t.Errorf("lease = %s", got.State)
	}
	if st := e.storedWorker(w); st.Reservations != 1 {
		t.Errorf("reservations = %d", st.Reservations)
	}
	if cmds := e.commands(w); len(cmds) != 1 {
		t.Errorf("commands = %+v", cmds)
	}
}

// Whatever order an accept, a reject and an expiry meet in, exactly one wins and
// the Run, the lease and the reservation agree.
func TestRaces_AcceptRejectAndExpiryExcludeEachOther(t *testing.T) {
	const rounds = 8
	for round := range rounds {
		e := newEnv(t)
		w := e.joinWorker("alpha")
		run := e.queuedRun("x")
		l := e.offer(run.ID, w)
		e.clock.Advance(acceptBy - time.Millisecond) // the expiry only wins if the clock passes accept_by

		var wg sync.WaitGroup
		results := make([]error, 3)
		wg.Add(3)
		go func() { defer wg.Done(); _, results[0] = e.accept(w, l) }()
		go func() { defer wg.Done(); _, results[1] = e.reject(w, l, domain.CodeWorkerCapacity) }()
		go func() {
			defer wg.Done()
			if round%2 == 0 {
				e.clock.Advance(2 * time.Millisecond)
			}
			_, results[2] = e.app.Leases.SweepExpired(t.Context())
		}()
		wg.Wait()

		got := e.storedLease(l.ID)
		r := e.storedRun(run.ID)
		st := e.storedWorker(w)
		switch got.State {
		case domain.LeaseAccepted:
			if r.State != domain.RunPreparing || r.LeaseID != l.ID || st.Reservations != 1 {
				t.Errorf("round %d accepted: Run %+v, %d reservations", round, r, st.Reservations)
			}
		case domain.LeaseRejected, domain.LeaseExpired:
			if r.State != domain.RunQueued || r.LeaseID != "" || r.WorkerID != "" || st.Reservations != 0 {
				t.Errorf("round %d %s: Run %+v, %d reservations", round, got.State, r, st.Reservations)
			}
		default:
			t.Errorf("round %d: the lease is %s", round, got.State)
		}
		if closed := len(e.events(domain.EventLeaseAccepted, domain.EventLeaseRejected, domain.EventLeaseExpired)); closed != 1 {
			t.Errorf("round %d: %d events say how the lease ended", round, closed)
		}
		if results[0] != nil && got.State == domain.LeaseAccepted {
			t.Errorf("round %d: the accept lost with the lease accepted: %v", round, results[0])
		}
	}
}

func TestAssignments_AreOnlyTheCallersAndAreDeliveredUntilAcknowledged(t *testing.T) {
	e := newEnv(t)
	alpha, beta := e.joinWorker("alpha"), e.joinWorker("beta")
	first, second := e.offer(e.queuedRun("a").ID, alpha), e.offer(e.queuedRun("b").ID, alpha)
	e.offer(e.queuedRun("c").ID, beta)

	cmds := e.commands(alpha)
	if len(cmds) != 2 || cmds[0].Seq != 1 || cmds[1].Seq != 2 || cmds[0].Type != domain.CommandAssign {
		t.Fatalf("alpha's commands = %+v", cmds)
	}
	a := cmds[0].Assign
	if a.LeaseID != string(first.ID) || a.RunID != string(first.RunID) || a.Epoch != 1 || a.SourceCommit != commitA ||
		a.Resources != (v1.Capacity{CPU: 2, MemoryMB: 4096}) || !a.AcceptBy.Equal(first.AcceptBy) || !strings.Contains(string(a.Spec), `"prompt":"a"`) ||
		a.RunShort != "run-1" || a.TaskID == "" || a.ProjectID == "" {
		t.Errorf("assign = %+v", a)
	}
	if got := e.commands(beta); len(got) != 1 || got[0].Seq != 1 {
		t.Errorf("beta's commands = %+v", got)
	}

	// Reading acknowledges nothing: the same orders again, and from a Seq on.
	again, err := e.app.Leases.Assignments(t.Context(), alpha, leases.AssignmentsQuery{AfterSeq: 1})
	must(t, err)
	if len(again.Commands) != 1 || again.Commands[0].Seq != 2 || again.LastSeq != 2 {
		t.Errorf("after seq 1: %+v", again)
	}

	// Accepting a lease confirms its own assign and no other.
	_, err = e.accept(alpha, second)
	must(t, err)
	if got := e.commands(alpha); len(got) != 1 || got[0].Seq != 1 {
		t.Errorf("after accepting the second: %+v", got)
	}

	// The cumulative ACK is idempotent; the first assign is gone for good.
	for range 2 {
		ack, err := e.app.Leases.Ack(t.Context(), alpha, v1.AckCommandsRequest{UpToSeq: 1})
		must(t, err)
		if ack.AckedThrough != 1 {
			t.Errorf("ack = %+v", ack)
		}
	}
	if got := e.commands(alpha); len(got) != 0 {
		t.Errorf("after the ACK: %+v", got)
	}
	// The ACK of beta covers beta's orders, not alpha's.
	_, err = e.app.Leases.Ack(t.Context(), beta, v1.AckCommandsRequest{UpToSeq: 1})
	must(t, err)
	if st := e.storedWorker(alpha); st.LastCommandSeq != 2 {
		t.Errorf("alpha's last seq = %d", st.LastCommandSeq)
	}
	_, err = e.app.Leases.Ack(t.Context(), alpha, v1.AckCommandsRequest{UpToSeq: 3})
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = e.app.Leases.Ack(t.Context(), e.owner, v1.AckCommandsRequest{UpToSeq: 0})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Leases.Assignments(t.Context(), e.owner, leases.AssignmentsQuery{})
	wantCode(t, err, domain.CodeNotFound)
}

func TestAssignments_ACancelledOrderLeavesAHoleThatTheAckCrosses(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	e.offer(e.queuedRun("a").ID, w)
	e.offer(e.queuedRun("b").ID, w)
	e.clock.Advance(acceptBy)
	_, err := e.app.Leases.SweepExpired(t.Context())
	must(t, err)
	if cmds := e.commands(w); len(cmds) != 0 {
		t.Fatalf("both leases expired, commands = %+v", cmds)
	}

	third := e.offer(e.queuedRun("c").ID, w)
	cmds := e.commands(w)
	if len(cmds) != 1 || cmds[0].Seq != 3 || cmds[0].Assign.LeaseID != string(third.ID) {
		t.Fatalf("commands = %+v, want the third order after the two cancelled ones", cmds)
	}
	ack, err := e.app.Leases.Ack(t.Context(), w, v1.AckCommandsRequest{UpToSeq: 3})
	must(t, err)
	if ack.AckedThrough != 3 || len(e.commands(w)) != 0 {
		t.Errorf("ack = %+v", ack)
	}
}

func TestAssignments_PagesNeverSkipAnOrderAndBoundTheirBytes(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	for i := range 5 {
		e.offer(e.queuedRun(strings.Repeat("p", i+1)).ID, w)
	}

	var seen []uint64
	after := uint64(0)
	for {
		page, err := e.app.Leases.Assignments(t.Context(), w, leases.AssignmentsQuery{AfterSeq: after, Limit: 2})
		must(t, err)
		if len(page.Commands) == 0 {
			break
		}
		if len(page.Commands) > 2 || page.LastSeq != page.Commands[len(page.Commands)-1].Seq {
			t.Fatalf("page = %+v", page)
		}
		for _, c := range page.Commands {
			seen = append(seen, c.Seq)
		}
		after = page.LastSeq
	}
	if len(seen) != 5 {
		t.Fatalf("pages delivered %v", seen)
	}
	for i, s := range seen {
		if s != uint64(i+1) {
			t.Fatalf("pages delivered %v: an order was skipped", seen)
		}
	}

	// The default limit is 16 and the maximum 64.
	if page, err := e.app.Leases.Assignments(t.Context(), w, leases.AssignmentsQuery{Limit: 1000}); err != nil || len(page.Commands) != 5 {
		t.Errorf("limit 1000: %d commands (%v)", len(page.Commands), err)
	}

	// Past the byte bound the page stops, but its first order is always sent.
	big := strings.Repeat("x", 300<<10)
	e.offer(e.queuedRun("big").ID, w)
	e.offer(e.queuedRun("bigger").ID, w)
	e.bloat(6, big)
	e.bloat(7, big)
	page, err := e.app.Leases.Assignments(t.Context(), w, leases.AssignmentsQuery{AfterSeq: 5})
	must(t, err)
	if len(page.Commands) != 1 || page.Commands[0].Seq != 6 || page.LastSeq != 6 {
		t.Fatalf("first big page = %d commands, last %d", len(page.Commands), page.LastSeq)
	}
	page, err = e.app.Leases.Assignments(t.Context(), w, leases.AssignmentsQuery{AfterSeq: page.LastSeq})
	must(t, err)
	if len(page.Commands) != 1 || page.Commands[0].Seq != 7 {
		t.Fatalf("second big page = %+v", page.Commands)
	}
}

func TestLongPoll(t *testing.T) {
	t.Run("an order that is already there is returned at once", func(t *testing.T) {
		e := newEnv(t)
		w := e.joinWorker("alpha")
		e.offer(e.queuedRun("a").ID, w)
		page, err := e.app.Leases.Assignments(t.Context(), w, leases.AssignmentsQuery{Wait: leases.MaxWait})
		if err != nil || len(page.Commands) != 1 {
			t.Fatalf("page = %+v (%v)", page, err)
		}
	})

	t.Run("it waits for a commit that brings an order and ignores the ones that do not", func(t *testing.T) {
		e := newEnv(t)
		alpha, beta := e.joinWorker("alpha"), e.joinWorker("beta")
		result := e.poll(alpha, 0, leases.MaxWait)
		e.blockUntilPolling()

		e.offer(e.queuedRun("a").ID, beta) // an event, but not for alpha
		select {
		case page := <-result:
			t.Fatalf("a commit for another Worker ended the poll: %+v", page)
		case <-time.After(50 * time.Millisecond):
		}

		e.offer(e.queuedRun("b").ID, alpha)
		page := e.receive(result)
		if page.err != nil || len(page.Commands) != 1 || page.LastSeq != 1 {
			t.Fatalf("page = %+v", page)
		}
	})

	t.Run("it never misses a commit that lands as it starts to wait", func(t *testing.T) {
		e := newEnv(t)
		w := e.joinWorker("alpha")
		for round := range 12 {
			result := e.poll(w, uint64(round), leases.MaxWait)
			if round%2 == 0 {
				e.blockUntilPolling()
			}
			e.offer(e.queuedRun("p").ID, w)
			page := e.receive(result)
			if page.err != nil || len(page.Commands) != 1 || page.Commands[0].Seq != uint64(round+1) {
				t.Fatalf("round %d: page = %+v", round, page)
			}
		}
	})

	t.Run("the deadline is one for the whole request and ends it empty", func(t *testing.T) {
		e := newEnv(t)
		w := e.joinWorker("alpha")
		other := e.joinWorker("beta")
		result := e.poll(w, 7, leases.MaxWait)
		e.blockUntilPolling()

		// Wake-ups that bring nothing for the Worker do not extend the wait.
		e.clock.Advance(leases.MaxWait - time.Second)
		e.offer(e.queuedRun("a").ID, other)
		e.clock.Advance(time.Second)
		page := e.receive(result)
		if page.err != nil || len(page.Commands) != 0 || page.LastSeq != 7 || page.Commands == nil {
			t.Fatalf("page = %+v (%v), want an empty list and last_seq 7", page, page.err)
		}
		e.noTimersLeft()
	})

	t.Run("a wait above the maximum is cut to it", func(t *testing.T) {
		e := newEnv(t)
		w := e.joinWorker("alpha")
		result := e.poll(w, 0, time.Hour)
		e.blockUntilPolling()
		e.clock.Advance(leases.MaxWait)
		if page := e.receive(result); page.err != nil || len(page.Commands) != 0 {
			t.Fatalf("page = %+v", page)
		}
	})

	t.Run("wait zero answers at once", func(t *testing.T) {
		e := newEnv(t)
		w := e.joinWorker("alpha")
		page, err := e.app.Leases.Assignments(t.Context(), w, leases.AssignmentsQuery{})
		if err != nil || len(page.Commands) != 0 || page.Commands == nil || page.LastSeq != 0 {
			t.Fatalf("page = %+v (%v)", page, err)
		}
		e.noTimersLeft()
	})

	t.Run("a cancelled request returns the context error and leaves nothing behind", func(t *testing.T) {
		e := newEnv(t)
		w := e.joinWorker("alpha")
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := e.app.Leases.Assignments(ctx, w, leases.AssignmentsQuery{Wait: leases.MaxWait})
			done <- err
		}()
		e.blockUntilPolling()
		cancel()
		select {
		case err := <-done:
			if err == nil || ctx.Err() == nil {
				t.Fatalf("err = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the poll ignored its context")
		}
		e.noTimersLeft()
	})
}

type polled struct {
	v1.AssignmentsResponse
	err error
}

// poll starts an Assignments request in the background.
func (e *env) poll(w pki.Session, after uint64, wait time.Duration) <-chan polled {
	e.t.Helper()
	out := make(chan polled, 1)
	go func() {
		page, err := e.app.Leases.Assignments(e.t.Context(), w, leases.AssignmentsQuery{AfterSeq: after, Wait: wait})
		out <- polled{page, err}
	}()
	return out
}

// blockUntilPolling returns once a request is waiting on its deadline.
func (e *env) blockUntilPolling() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(e.t.Context(), 10*time.Second)
	defer cancel()
	must(e.t, e.clock.BlockUntil(ctx, 1))
}

func (e *env) receive(ch <-chan polled) polled {
	e.t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(10 * time.Second):
		e.t.Fatal("the poll did not answer")
		return polled{}
	}
}

// noTimersLeft checks that no request leaves its deadline running.
func (e *env) noTimersLeft() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(e.t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := e.clock.BlockUntil(ctx, 1); err == nil {
		e.t.Error("a timer is still running")
	}
}

// bloat makes the payload of the order with the Seq big, to test the byte bound.
func (e *env) bloat(seq uint64, pad string) {
	e.t.Helper()
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(e.t.Context(), `UPDATE worker_commands SET payload = json_set(payload, '$.spec', json_object('pad', ?)) WHERE seq = ?`, pad, seq)
		return err
	}))
}

// The answer is the watermark of what the Worker has acknowledged, not an echo of the request.
func TestAck_AnswersTheHighestAcknowledgedSeq(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	for i := range 5 {
		e.offer(e.queuedRun(strings.Repeat("p", i+1)).ID, w)
	}
	for _, c := range []struct{ upTo, want uint64 }{{5, 5}, {3, 5}, {5, 5}, {0, 5}} {
		got, err := e.app.Leases.Ack(t.Context(), w, v1.AckCommandsRequest{UpToSeq: c.upTo})
		must(t, err)
		if got.AckedThrough != c.want {
			t.Errorf("ack %d answered %d, want %d", c.upTo, got.AckedThrough, c.want)
		}
	}
}

// Revoking the Worker while its poll waits ends the poll with CF_UNAUTHENTICATED and no orders.
func TestLongPoll_IsAuthorisedAgainAtEveryLook(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	result := e.poll(w, 0, leases.MaxWait)
	e.blockUntilPolling()

	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		return tx.Revoke(t.Context(), w.Principal.ID, domain.KindWorker, e.clock.Now())
	}))
	e.clock.Advance(leases.MaxWait) // the deadline: its last look is authorised too
	got := e.receive(result)
	wantCode(t, got.err, domain.CodeUnauthenticated)
	if len(got.Commands) != 0 {
		t.Errorf("orders were delivered to a revoked Worker: %+v", got.Commands)
	}

	// And a poll that finds an order ready does not deliver it either.
	e2 := newEnv(t)
	w2 := e2.joinWorker("alpha")
	e2.offer(e2.queuedRun("x").ID, w2)
	must(t, e2.db.WithTx(t.Context(), func(tx *store.Tx) error {
		return tx.Revoke(t.Context(), w2.Principal.ID, domain.KindWorker, e2.clock.Now())
	}))
	page, err := e2.app.Leases.Assignments(t.Context(), w2, leases.AssignmentsQuery{})
	wantCode(t, err, domain.CodeUnauthenticated)
	if len(page.Commands) != 0 {
		t.Errorf("page = %+v", page)
	}
}
