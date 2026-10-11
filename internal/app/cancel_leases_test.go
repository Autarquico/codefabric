package app_test

import (
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Cancelling or completing a Task whose Run waits for its lease answer ends the
// lease, releases the reservation and withdraws the order, in one transaction.
func TestCancelAndComplete_WithAPendingLease(t *testing.T) {
	for name, act := range map[string]func(e *env, task string) error{
		"cancel": func(e *env, task string) error {
			_, err := e.app.Tasks.Cancel(e.t.Context(), e.owner, task, "", v1.ActionRequest{RequestID: "req-1"})
			return err
		},
		"complete": func(e *env, task string) error {
			_, err := e.app.Tasks.Complete(e.t.Context(), e.owner, task, "", v1.ActionRequest{RequestID: "req-1"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			w := e.joinWorker("alpha")
			run := e.queuedRun("x")
			l := e.offer(run.ID, w)

			must(t, act(e, run.TaskID))
			r := e.storedRun(run.ID)
			if r.State != domain.RunCancelled || r.WorkerID != "" || r.LeaseID != "" || r.LeaseEpoch != 1 {
				t.Errorf("Run = %+v: ended, with no Worker or lease and its epoch kept", r)
			}
			if got := e.storedLease(l.ID); got.State != domain.LeaseExpired || got.CloseCode != domain.LeaseCloseRunEnded {
				t.Errorf("lease = %+v", got)
			}
			if rs := e.reservation(l.ID); rs.ReleasedAt == nil || rs.ReleaseReason != "run_ended" {
				t.Errorf("reservation = %+v", rs)
			}
			if st := e.storedWorker(w); st.Reservations != 0 || st.Resources.Free.CPU != 16 {
				t.Errorf("Worker holds %d, free %+v", st.Reservations, st.Resources.Free)
			}
			if cmds := e.commands(w); len(cmds) != 0 {
				t.Errorf("the order of a cancelled Run is delivered: %+v", cmds)
			}
			// The Worker that still has the assignment is told it is stale.
			_, err := e.accept(w, l)
			wantCode(t, err, domain.CodeStaleLease)

			events := len(e.events())
			must(t, act(e, run.TaskID)) // the same request_id again
			if len(e.events()) != events || len(e.events(domain.EventLeaseExpired)) != 1 {
				t.Errorf("the repeated request wrote events")
			}
		})
	}
}

func TestCancel_WithAnAcceptedLeaseKeepsTheLeaseAndTheReservation(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	l := e.offer(run.ID, w)
	_, err := e.accept(w, l)
	must(t, err)

	_, err = e.app.Tasks.Cancel(t.Context(), e.owner, run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	r := e.storedRun(run.ID)
	if r.State != domain.RunCancelled || r.LeaseID != l.ID || r.WorkerID != w.Principal.ID {
		t.Errorf("Run = %+v: the references of an accepted lease stay", r)
	}
	if got := e.storedLease(l.ID); got.State != domain.LeaseAccepted {
		t.Errorf("lease = %s", got.State)
	}
	if rs := e.reservation(l.ID); rs.ReleasedAt != nil {
		t.Errorf("the reservation of a Run that may be starting was released")
	}
	got, err := e.app.Runs.Get(t.Context(), run.ID)
	must(t, err)
	if got.Lease == nil || got.Lease.State != domain.LeaseAccepted || got.Lease.Epoch != 1 {
		t.Errorf("Run view lease = %+v", got.Lease)
	}
}

func TestCancel_FailureRollsBackTheLeaseToo(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	l := e.offer(run.ID, w)
	e.refuseEvents(domain.EventRunCancelled)

	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, run.TaskID, "", v1.ActionRequest{})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
	if got := e.storedLease(l.ID); got.State != domain.LeasePending {
		t.Errorf("lease = %s", got.State)
	}
	if rs := e.reservation(l.ID); rs.ReleasedAt != nil {
		t.Error("reservation released")
	}
	if r := e.storedRun(run.ID); r.State != domain.RunQueued || r.LeaseID != l.ID {
		t.Errorf("Run = %+v", r)
	}
	if len(e.events(domain.EventLeaseExpired)) != 0 || len(e.commands(w)) != 1 {
		t.Error("the rollback left a trace")
	}
}

func TestCancel_RaceWithAccept(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	l := e.offer(run.ID, w)
	done := make(chan error, 1)
	go func() { _, err := e.accept(w, l); done <- err }()
	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	acceptErr := <-done

	r, got := e.storedRun(run.ID), e.storedLease(l.ID)
	switch got.State {
	case domain.LeaseAccepted: // accept won: the cancel then cancelled a preparing Run
		if acceptErr != nil || r.State != domain.RunCancelled || r.LeaseID != l.ID {
			t.Errorf("accept won: %v, Run %+v", acceptErr, r)
		}
	case domain.LeaseExpired:
		wantCode(t, acceptErr, domain.CodeStaleLease)
		if r.LeaseID != "" {
			t.Errorf("Run %+v", r)
		}
	default:
		t.Errorf("lease %s", got.State)
	}
}

func TestRemove_IsBlockedByOpenReservations(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	l := e.offer(run.ID, w)
	_, err := e.accept(w, l)
	must(t, err)
	_, err = e.app.Tasks.Cancel(t.Context(), e.owner, run.TaskID, "", v1.ActionRequest{})
	must(t, err) // the Run ended but its reservation stays until its stop is confirmed

	_, err = e.app.Workers.Remove(t.Context(), e.owner, "alpha", v1.ActionRequest{})
	wantCode(t, err, domain.CodeWorkerBusy)
	view, err := e.app.Workers.Get(t.Context(), "alpha")
	must(t, err)
	if view.Reservations != 1 {
		t.Errorf("Reservations = %d", view.Reservations)
	}
}

func TestHeartbeat_KeepsFreeNetOfReservations(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	e.offer(e.queuedRun("x").ID, w)
	e.beat(w)
	if got := e.storedWorker(w).Resources.Free; got.CPU != 14 || got.MemoryMB != 65536-4096 {
		t.Errorf("Free after a heartbeat = %+v", got)
	}
}
