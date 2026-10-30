package app_test

import (
	"database/sql"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// joinWorker enrols a Worker and sends its first heartbeat, so that it is
// online with 16 CPUs and 64 GB to contribute. mutate adjusts the heartbeat.
func (e *env) joinWorker(name string, mutate ...func(*v1.HeartbeatRequest)) pki.Session {
	e.t.Helper()
	return e.joinWorkerWith(name, v1.WorkerSettings{}, mutate...)
}

// joinWorkerWith is joinWorker for a Worker that contributes what settings say.
func (e *env) joinWorkerWith(name string, settings v1.WorkerSettings, mutate ...func(*v1.HeartbeatRequest)) pki.Session {
	e.t.Helper()
	invite, _, err := e.authority.CreateToken(e.t.Context(), e.owner, domain.KindWorker, time.Hour)
	must(e.t, err)
	joined, err := e.authority.Join(e.t.Context(), invite.Token, e.csr(), name, &settings)
	must(e.t, err)
	w, err := joined.Session()
	must(e.t, err)
	if e.sessions == nil {
		e.sessions = map[string]pki.Session{}
	}
	e.sessions[name] = w
	e.beat(w, mutate...)
	return w
}

// sessionOf is the session of a Worker joined by joinWorker.
func (e *env) sessionOf(name string) pki.Session { return e.sessions[name] }

// beat sends a heartbeat for the Worker, with the last epoch it used.
func (e *env) beat(w pki.Session, mutate ...func(*v1.HeartbeatRequest)) {
	e.t.Helper()
	req := v1.HeartbeatRequest{
		Epoch: e.epochOf(w), DaemonVersion: "", Hostname: "box.lan", OS: "linux", Arch: "amd64",
		Total: v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 500000}, Health: v1.Health{Status: v1.HealthOK},
		// New Runs are schema 4 unless their Project denies the repo's configuration.
		Capabilities: []string{domain.RepoConfigCapability},
	}
	for _, m := range mutate {
		m(&req)
	}
	_, err := e.app.Workers.Heartbeat(e.t.Context(), w, req)
	must(e.t, err)
}

// epochOf is the epoch of the Worker's daemon: one ULID per Worker.
func (e *env) epochOf(w pki.Session) string {
	if e.epochs == nil {
		e.epochs = map[domain.ID]string{}
	}
	if _, ok := e.epochs[w.Principal.ID]; !ok {
		e.epochs[w.Principal.ID] = ulid.Make().String()
	}
	return e.epochs[w.Principal.ID]
}

func withCapabilities(c ...string) func(*v1.HeartbeatRequest) {
	return func(r *v1.HeartbeatRequest) { r.Capabilities = append(slices.Clone(c), domain.RepoConfigCapability) }
}

// offer gives the queued Run to the Worker under a new lease, as the scheduler
// does, reserving 2 CPUs and 4 GB.
func (e *env) offer(run string, w pki.Session) store.Lease {
	e.t.Helper()
	l, err := e.tryOffer(run, w)
	must(e.t, err)
	return l
}

func (e *env) tryOffer(run string, w pki.Session) (store.Lease, error) {
	e.t.Helper()
	ctx := e.t.Context()
	return mutation.Do(ctx, e.db, e.clock, e.owner, "test.offer", "", run,
		func(tx *store.Tx, act mutation.Act) (store.Lease, error) {
			r, err := store.RunByID(ctx, tx, domain.ID(run))
			if err != nil {
				return store.Lease{}, err
			}
			task, err := store.TaskByID(ctx, tx, r.TaskID)
			if err != nil {
				return store.Lease{}, err
			}
			worker, err := store.WorkerByID(ctx, tx, w.Principal.ID)
			if err != nil {
				return store.Lease{}, err
			}
			return e.app.Leases.CreateIn(ctx, tx, act, r, task, worker, v1.Capacity{CPU: 2, MemoryMB: 4096})
		})
}

// queuedRun creates a project (once) and a Task with its first Run, queued.
func (e *env) queuedRun(prompt string, mutate ...func(*v1.CreateTaskRequest)) v1.Run {
	e.t.Helper()
	if _, err := e.app.Projects.Get(e.t.Context(), "backend"); err != nil {
		e.project("backend")
	}
	return *e.mustTask("backend", prompt, mutate...).Run
}

func (e *env) storedRun(id string) store.Run {
	e.t.Helper()
	var r store.Run
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		r, err = store.RunByID(e.t.Context(), q, domain.ID(id))
		return err
	}))
	return r
}

func (e *env) storedLease(id domain.ID) store.Lease {
	e.t.Helper()
	var l store.Lease
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		l, err = store.LeaseByID(e.t.Context(), q, id)
		return err
	}))
	return l
}

func (e *env) storedWorker(w pki.Session) store.Worker {
	e.t.Helper()
	var out store.Worker
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		out, err = store.WorkerByID(e.t.Context(), q, w.Principal.ID)
		return err
	}))
	return out
}

func (e *env) accept(w pki.Session, l store.Lease) (v1.LeaseResponse, error) {
	return e.app.Leases.Accept(e.t.Context(), w, string(l.ID), v1.AcceptLeaseRequest{Epoch: l.Epoch})
}

func (e *env) reject(w pki.Session, l store.Lease, code domain.Code) (v1.LeaseResponse, error) {
	return e.app.Leases.Reject(e.t.Context(), w, string(l.ID), v1.RejectLeaseRequest{Epoch: l.Epoch, Code: code, Message: "no room"})
}

// refuseEvents makes the store refuse any event of the type, to fail a
// transaction at its last step.
func (e *env) refuseEvents(eventType domain.EventType) {
	e.t.Helper()
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(e.t.Context(),
			`CREATE TRIGGER refuse_`+string(eventType)+` BEFORE INSERT ON events WHEN NEW.type = '`+string(eventType)+`'
			 BEGIN SELECT RAISE(ABORT, 'refused'); END`)
		return err
	}))
}

// commands is the orders the Worker would be sent from the start.
func (e *env) commands(w pki.Session) []v1.WorkerCommand {
	e.t.Helper()
	out, err := e.app.Leases.Assignments(e.t.Context(), w, leases.AssignmentsQuery{})
	must(e.t, err)
	return out.Commands
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	must(t, err)
	return string(raw)
}

// pass runs one pass of the scheduler.
func (e *env) pass() scheduler.Result {
	e.t.Helper()
	res, err := e.app.Scheduler.Pass(e.t.Context())
	must(e.t, err)
	return res
}

// workerOfRun is the name of the Worker a Run is assigned to, "" if none.
func (e *env) workerOfRun(run string) string {
	e.t.Helper()
	id := e.storedRun(run).WorkerID
	if id == "" {
		return ""
	}
	var w store.Worker
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		w, err = store.WorkerByID(e.t.Context(), q, id)
		return err
	}))
	return w.Name
}

// reservation reads the reservation of a lease.
func (e *env) reservation(lease domain.ID) store.Reservation {
	e.t.Helper()
	var (
		r              store.Reservation
		l, run, worker string
		created        int64
		released       sql.NullInt64
	)
	must(e.t, e.db.Reader().QueryRowContext(e.t.Context(),
		`SELECT lease_id, run_id, worker_id, cpu, memory_mb, disk_mb, created_at, released_at, COALESCE(release_reason, '')
		 FROM reservations WHERE lease_id = ?`, string(lease)).
		Scan(&l, &run, &worker, &r.Amount.CPU, &r.Amount.MemoryMB, &r.Amount.DiskMB, &created, &released, &r.ReleaseReason))
	if released.Valid {
		at := time.Unix(0, released.Int64).UTC()
		r.ReleasedAt = &at
	}
	r.LeaseID, r.RunID, r.WorkerID = domain.ID(l), domain.ID(run), domain.ID(worker)
	return r
}

// refuseEventsOfRun refuses the events of the type that are about one Run.
func (e *env) refuseEventsOfRun(eventType domain.EventType, run string) {
	e.t.Helper()
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(e.t.Context(),
			`CREATE TRIGGER refuse_run BEFORE INSERT ON events WHEN NEW.type = '`+string(eventType)+`' AND NEW.run_id = '`+run+`'
			 BEGIN SELECT RAISE(ABORT, 'refused'); END`)
		return err
	}))
}
