package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

func sqlStates(list string) []string {
	var states []string
	for _, quoted := range strings.Split(list, ",") {
		states = append(states, strings.Trim(strings.TrimSpace(quoted), "'"))
	}
	slices.Sort(states)
	return states
}

// The SQL lists are written out as text; they must stay the domain's.
func TestWorkerSQLListsMatchTheDomain(t *testing.T) {
	var active []string
	for _, s := range []domain.RunState{
		domain.RunQueued, domain.RunPreparing, domain.RunRunning, domain.RunWaitingInput, domain.RunCheckpointing,
		domain.RunSuspended, domain.RunCompleted, domain.RunFailed, domain.RunCancelled, domain.RunLost,
	} {
		if !s.IsTerminal() {
			active = append(active, string(s))
		}
	}
	slices.Sort(active)
	if got := sqlStates(activeRunStates); !slices.Equal(got, active) {
		t.Errorf("activeRunStates = %v, want the non-terminal states %v", got, active)
	}

	var live []string
	for _, s := range []domain.WorkerState{
		domain.WorkerJoining, domain.WorkerOnline, domain.WorkerDegraded, domain.WorkerDraining,
		domain.WorkerDrained, domain.WorkerOffline, domain.WorkerRemoved,
	} {
		if _, err := domain.NextWorkerState(domain.WorkerStatus{State: s}, domain.WorkerEventHeartbeatTimeout, domain.WorkerFacts{}); err == nil {
			live = append(live, string(s))
		}
	}
	slices.Sort(live)
	if got := sqlStates(liveWorkerStates); !slices.Equal(got, live) {
		t.Errorf("liveWorkerStates = %v, want the states that can go offline %v", got, live)
	}
}

type workerFixture struct {
	t   *testing.T
	db  *DB
	clk *clock.Fake
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	clk := clock.NewFake(epoch0)
	db, _ := openTest(t, Options{Clock: clk})
	return &workerFixture{t: t, db: db, clk: clk}
}

// worker inserts a worker identity and its Worker.
func (f *workerFixture) worker(short uint64, name string, state domain.WorkerState) Worker {
	f.t.Helper()
	ctx := f.t.Context()
	id := domain.ID(fmt.Sprintf("worker_%d%s", short, strings.ToUpper(name)))
	w := Worker{
		ID: id, Short: short, Name: name, IdentityID: id, State: state, CreatedAt: f.clk.Now(),
		Settings: v1.WorkerSettings{MaxConcurrentRuns: 4, Availability: domain.AvailabilityAlways},
	}
	if state == domain.WorkerOffline {
		w.BeforeOffline = domain.WorkerOnline
	}
	err := f.db.WithTx(ctx, func(tx *Tx) error {
		err := tx.InsertIdentity(ctx, Identity{
			ID: id, Kind: domain.KindWorker, Name: name, CertSerial: "serial-" + string(id), NotAfter: epoch0.Add(time.Hour), CreatedAt: epoch0,
		})
		if err != nil {
			return err
		}
		return tx.InsertWorker(ctx, w)
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

func (f *workerFixture) read(id domain.ID) Worker {
	f.t.Helper()
	var w Worker
	err := f.db.Read(f.t.Context(), func(q Querier) (err error) {
		w, err = WorkerByID(f.t.Context(), q, id)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

func TestWorkers_RoundTripWithTheLastHeartbeat(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := t.Context()
	w := f.worker(1, "alpha", domain.WorkerJoining)

	if got := f.read(w.ID); got.Name != "alpha" || got.State != domain.WorkerJoining || got.Resources != (v1.WorkerResources{}) ||
		got.Report.Capabilities == nil || got.LastHeartbeatAt != nil {
		t.Fatalf("before any heartbeat: %+v", got)
	}

	total := v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 512000}
	resources := v1.WorkerResources{Total: total, Allocatable: v1.Capacity{CPU: 8, MemoryMB: 65536, DiskMB: 512000}, Free: v1.Capacity{CPU: 8}}
	report := WorkerReport{Epoch: "E1", DaemonVersion: "v0.3.0", Hostname: "alpha.lan", OS: "linux", Arch: "amd64", Capabilities: []string{"docker"}}
	beat := func(report WorkerReport, health v1.Health) {
		t.Helper()
		if err := f.db.WithTx(ctx, func(tx *Tx) error {
			return tx.RecordHeartbeat(ctx, w.ID, report, resources, health, f.clk.Now())
		}); err != nil {
			t.Fatal(err)
		}
	}
	beat(report, v1.Health{Status: v1.HealthOK})
	report.Epoch = "E2"
	beat(report, v1.Health{Status: v1.HealthDegraded, Message: "disk"}) // replaces the first: only the last is kept

	got := f.read(w.ID)
	if got.Report.Epoch != "E2" || got.Report.Hostname != "alpha.lan" || !slices.Equal(got.Report.Capabilities, []string{"docker"}) ||
		got.Resources != resources || got.Health != (v1.Health{Status: v1.HealthDegraded, Message: "disk"}) ||
		got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(epoch0) {
		t.Errorf("after heartbeats: %+v", got)
	}
	if n := count(t, f.db, "worker_heartbeats"); n != 1 {
		t.Errorf("%d heartbeat rows, want only the latest", n)
	}
	byIdentity := Worker{}
	err := f.db.Read(ctx, func(q Querier) (err error) {
		byIdentity, err = WorkerByIdentity(ctx, q, w.IdentityID)
		return err
	})
	if err != nil || byIdentity.ID != w.ID {
		t.Errorf("by identity: %+v, %v", byIdentity, err)
	}
}

func TestSetWorkerStatus_IsACompareAndSet(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := t.Context()
	w := f.worker(1, "alpha", domain.WorkerOnline)
	online := domain.WorkerStatus{State: domain.WorkerOnline}
	offline := domain.WorkerStatus{State: domain.WorkerOffline, BeforeOffline: domain.WorkerOnline}

	move := func(from, to domain.WorkerStatus) bool {
		t.Helper()
		var moved bool
		if err := f.db.WithTx(ctx, func(tx *Tx) (err error) {
			moved, err = tx.SetWorkerStatus(ctx, w.ID, from, to, f.clk.Now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return moved
	}
	if !move(online, offline) {
		t.Fatal("online -> offline did not move")
	}
	if got := f.read(w.ID); got.Status() != offline {
		t.Errorf("status = %+v", got.Status())
	}
	if move(online, offline) {
		t.Error("the same change moved twice")
	}
	if move(domain.WorkerStatus{State: domain.WorkerOffline, BeforeOffline: domain.WorkerDrained}, online) {
		t.Error("a status with another BeforeOffline matched")
	}
	if !move(offline, online) || f.read(w.ID).Status() != online {
		t.Error("offline -> online did not move back, clearing BeforeOffline")
	}
	if got := f.read(w.ID); got.RemovedAt != nil {
		t.Error("removed_at set without removing")
	}
	if !move(online, domain.WorkerStatus{State: domain.WorkerRemoved}) {
		t.Fatal("remove did not move")
	}
	if got := f.read(w.ID); got.RemovedAt == nil || !got.RemovedAt.Equal(epoch0) {
		t.Errorf("removed_at = %v", got.RemovedAt)
	}
}

func TestSilentWorkers_CountFromTheLastSignOfLifeOrTheCreation(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := t.Context()
	never := f.worker(1, "never", domain.WorkerJoining) // created at epoch0, no heartbeat
	beating := f.worker(2, "beating", domain.WorkerOnline)
	f.worker(3, "gone", domain.WorkerOffline)
	removed := f.worker(4, "removed", domain.WorkerOnline)
	err := f.db.WithTx(ctx, func(tx *Tx) error {
		if err := tx.RecordHeartbeat(ctx, beating.ID, WorkerReport{Epoch: "E"}, v1.WorkerResources{}, v1.Health{}, epoch0.Add(40*time.Second)); err != nil {
			return err
		}
		_, err := tx.SetWorkerStatus(ctx, removed.ID, removed.Status(), domain.WorkerStatus{State: domain.WorkerRemoved}, epoch0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	silent := func(cutoff time.Time) []string {
		t.Helper()
		var names []string
		err := f.db.Read(ctx, func(q Querier) error {
			ws, err := SilentWorkers(ctx, q, cutoff)
			for _, w := range ws {
				names = append(names, w.Name)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	if got := silent(epoch0); len(got) != 0 {
		t.Errorf("at the creation instant itself: %v; silence is strictly older than the cutoff", got)
	}
	if got := silent(epoch0.Add(time.Nanosecond)); !slices.Equal(got, []string{never.Name}) {
		t.Errorf("just after the creation: %v, want only the Worker that never heartbeated (offline, removed and fresh ones are not live-and-silent)", got)
	}
	if got := silent(epoch0.Add(41 * time.Second)); !slices.Equal(got, []string{"never", "beating"}) {
		t.Errorf("later: %v", got)
	}
}

func TestWorkers_ActiveRunsCountOnlyTheNonTerminalRunsOfTheWorker(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := t.Context()
	w := f.worker(1, "alpha", domain.WorkerOnline)
	other := f.worker(2, "beta", domain.WorkerOnline)

	err := f.db.WithTx(ctx, func(tx *Tx) error {
		p := Project{ID: "project_1", Short: 1, Name: "p", Repo: "r", DefaultBranch: "main", CreatedAt: epoch0}
		if err := tx.InsertProject(ctx, p); err != nil {
			return err
		}
		task := Task{ID: "task_1", Short: 1, ProjectID: p.ID, Name: "t", Prompt: "p", State: domain.TaskRunning, CreatedBy: domain.CreatorHuman, CreatedAt: epoch0}
		if err := tx.InsertTask(ctx, task); err != nil {
			return err
		}
		for i, state := range []domain.RunState{
			domain.RunQueued, domain.RunPreparing, domain.RunRunning, domain.RunWaitingInput, domain.RunCheckpointing,
			domain.RunSuspended, domain.RunCompleted, domain.RunFailed, domain.RunCancelled, domain.RunLost,
		} {
			run := Run{
				ID: domain.ID("run_" + string(state)), Short: uint64(i + 1), TaskID: task.ID, Attempt: i + 1,
				Lineage: domain.LineageInitial, Spec: []byte(`{}`), SpecDigest: "d", SourceCommit: "c", State: state,
				Publication: domain.PublicationNone, Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone,
				WorkerID: w.ID, CreatedAt: epoch0,
			}
			if err := tx.InsertRun(ctx, run); err != nil {
				return err
			}
		}
		unassigned := Run{
			ID: "run_unassigned", Short: 11, TaskID: task.ID, Attempt: 11, Lineage: domain.LineageInitial, Spec: []byte(`{}`),
			SpecDigest: "d", SourceCommit: "c", State: domain.RunQueued, Publication: domain.PublicationNone,
			Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, CreatedAt: epoch0,
		}
		return tx.InsertRun(ctx, unassigned)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read(w.ID).ActiveRuns; got != 5 {
		t.Errorf("ActiveRuns = %d, want the 5 non-terminal Runs", got)
	}
	if got := f.read(other.ID).ActiveRuns; got != 0 {
		t.Errorf("another Worker has %d", got)
	}
}

func TestWorkers_NamesAreFreedByRemoval(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := t.Context()
	w := f.worker(1, "alpha", domain.WorkerOnline)

	taken := func() bool {
		var taken bool
		if err := f.db.Read(ctx, func(q Querier) (err error) {
			taken, err = WorkerNameTaken(ctx, q, "alpha")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return taken
	}
	if !taken() {
		t.Fatal("a live Worker's name is free")
	}
	if err := f.db.WithTx(ctx, func(tx *Tx) error {
		dup := w
		dup.ID, dup.IdentityID, dup.Short = "worker_X", "worker_X", 9
		if err := tx.InsertIdentity(ctx, Identity{ID: dup.ID, Kind: domain.KindWorker, Name: "x", CertSerial: "x", NotAfter: epoch0, CreatedAt: epoch0}); err != nil {
			return err
		}
		if err := tx.InsertWorker(ctx, dup); err == nil {
			t.Error("two live Workers took the same name")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	err := f.db.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.SetWorkerStatus(ctx, w.ID, w.Status(), domain.WorkerStatus{State: domain.WorkerRemoved}, epoch0)
		return err
	})
	if err != nil || taken() {
		t.Fatalf("a removed Worker still holds its name (%v)", err)
	}
	f.worker(2, "alpha", domain.WorkerJoining) // the name can be used again

	var candidates []domain.Candidate
	if err := f.db.Read(ctx, func(q Querier) (err error) {
		candidates, err = WorkerCandidates(ctx, q)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id, err := domain.Resolve("alpha", domain.KindWorker, "", candidates)
	if err != nil || id == w.ID {
		t.Errorf("the name resolves to %s (%v), want the live Worker, not the removed one", id, err)
	}
	if id, err := domain.Resolve("worker-1", domain.KindWorker, "", candidates); err != nil || id != w.ID {
		t.Errorf("the removed Worker's short ID resolves to %s (%v)", id, err)
	}
}

// A Controller of the first Phase 1 milestone upgrades to the Workers schema
// keeping its Projects, Tasks and Runs, after leaving a backup of its database.
func TestOpen_UpgradesAPhase1DatabaseWithoutWorkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	seedUntil(t, path, 101)

	db, err := Open(t.Context(), path, Options{Clock: clock.NewFake(epoch0)})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := quickCheckFile(t.Context(), path+".pre-101"); err != nil {
		t.Errorf("no valid backup of the 0101 database: %v", err)
	}
	for table, want := range map[string]int{"projects": 1, "tasks": 1, "runs": 1, "identities": 1, "workers": 0, "worker_heartbeats": 0} {
		if got := count(t, db, table); got != want {
			t.Errorf("%s has %d rows after the upgrade, want %d", table, got, want)
		}
	}
	// The Run that predates Workers can now be assigned to one.
	ctx := t.Context()
	err = db.WithTx(ctx, func(tx *Tx) error {
		if err := tx.InsertIdentity(ctx, Identity{ID: "worker_1", Kind: domain.KindWorker, Name: "w", CertSerial: "w", NotAfter: epoch0, CreatedAt: epoch0}); err != nil {
			return err
		}
		if err := tx.InsertWorker(ctx, Worker{ID: "worker_1", Short: 1, Name: "w", IdentityID: "worker_1", State: domain.WorkerOnline, CreatedAt: epoch0}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE runs SET worker_id = 'worker_1'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := workerActiveRuns(t, db, "worker_1"); got != 1 {
		t.Errorf("ActiveRuns = %d, want the one queued Run that was assigned", got)
	}
}

func workerActiveRuns(t *testing.T, db *DB, id domain.ID) int {
	t.Helper()
	var w Worker
	if err := db.Read(t.Context(), func(q Querier) (err error) {
		w, err = WorkerByID(t.Context(), q, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return w.ActiveRuns
}

// seedUntil builds a database as it was at schema version last, with a row in
// each table that the later migrations must not disturb.
func seedUntil(t *testing.T, path string, last int64) {
	t.Helper()
	old := fstest.MapFS{}
	entries, err := fs.ReadDir(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		version, _, _ := strings.Cut(e.Name(), "_")
		if n, convErr := strconv.ParseInt(version, 10, 64); convErr != nil || n > last {
			continue
		}
		raw, err := fs.ReadFile(embeddedMigrations, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		old[e.Name()] = &fstest.MapFile{Data: raw}
	}
	sqlDB, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v, err := provider.GetDBVersion(t.Context()); err != nil || v != last {
		t.Fatalf("seeded version %d (%v), want %d", v, err, last)
	}
	for _, stmt := range []string{
		`INSERT INTO identities (id, kind, name, cert_serial, not_after, created_at) VALUES ('client_1', 'client', 'owner', 'ab', 9, 1)`,
		`INSERT INTO projects (id, short_n, name, repo, default_branch, defaults, created_at) VALUES ('project_1', 1, 'p', 'r', 'main', '{}', 1)`,
		`INSERT INTO tasks (id, short_n, project_id, name, prompt, autostart, requirements, created_by, state, created_at)
		 VALUES ('task_1', 1, 'project_1', 't', 'x', 1, '{}', 'human', 'queued', 1)`,
		`INSERT INTO runs (id, short_n, task_id, attempt, lineage_kind, spec, spec_digest, source_commit, state, publication,
		     evaluation, runtime, created_at) VALUES ('run_1', 1, 'task_1', 1, 'initial', '{}', 'd', 'c', 'queued', 'none', 'none', 'none', 1)`,
	} {
		if _, err := sqlDB.ExecContext(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
}
