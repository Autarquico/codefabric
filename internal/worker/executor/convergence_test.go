//go:build linux || darwin

package executor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// errLost is an answer that never arrived: what it answered was applied.
var errLost = errors.New("the answer was lost")

// liveRelay is the relay of the convergence test: it holds what the fake git pushed.
type liveRelay struct {
	mu   sync.Mutex
	refs map[string]string // "<project>/<ref or branch>" -> commit
}

func (r *liveRelay) set(project, ref, commit string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs[project+"/"+ref] = commit
}

func (r *liveRelay) get(project, ref string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oid, ok := r.refs[project+"/"+ref]
	return oid, ok, nil
}

func (r *liveRelay) BranchTip(_ context.Context, project domain.ID, branch string) (string, bool, error) {
	return r.get(string(project), branch)
}

func (r *liveRelay) RefTip(_ context.Context, project domain.ID, ref string) (string, bool, error) {
	return r.get(string(project), ref)
}

// appController is the real Controller (its use cases on a real store) as the
// executor and the sender see it, with answers a test can lose.
type appController struct {
	mu   sync.Mutex
	app  *app.Services
	w    pki.Session
	lose map[string]bool // the next answer of the operation is lost
	// between, if set, runs once between an answer of reconcile and its use.
	between func()
}

func (c *appController) losing(op string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	lost := c.lose[op]
	delete(c.lose, op)
	return lost
}

func (c *appController) Assignments(ctx context.Context, after uint64, _ time.Duration, limit int) (v1.AssignmentsResponse, error) {
	return c.app.Leases.Assignments(ctx, c.w, leases.AssignmentsQuery{AfterSeq: after, Limit: limit})
}

func (c *appController) AcceptLease(ctx context.Context, lease string, epoch uint64) (v1.Lease, error) {
	out, err := c.app.Leases.Accept(ctx, c.w, lease, v1.AcceptLeaseRequest{Epoch: epoch})
	if err == nil && c.losing("accept") {
		return v1.Lease{}, errLost
	}
	return out.Lease, err
}

func (c *appController) RejectLease(ctx context.Context, lease string, req v1.RejectLeaseRequest) (v1.Lease, error) {
	out, err := c.app.Leases.Reject(ctx, c.w, lease, req)
	return out.Lease, err
}

func (c *appController) AckCommands(ctx context.Context, upTo uint64) (v1.AckCommandsResponse, error) {
	return c.app.Leases.Ack(ctx, c.w, v1.AckCommandsRequest{UpToSeq: upTo})
}

func (c *appController) RelayToken(ctx context.Context, run string, epoch uint64) (v1.RelayTokenResponse, error) {
	return c.app.Runs.MintRelayToken(ctx, c.w, run, v1.RelayTokenRequest{LeaseEpoch: epoch})
}

func (c *appController) RelayURL(path string) string { return "https://controller.test" + path }

func (c *appController) OrphanToken(ctx context.Context, run string, epoch uint64, commit string) (v1.RelayTokenResponse, error) {
	return c.app.Runs.MintOrphanToken(ctx, c.w, run, v1.OrphanTokenRequest{LeaseEpoch: epoch, ResultCommit: commit})
}

func (c *appController) GrantSecrets(ctx context.Context, run string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
	return c.app.Runs.GrantSecrets(ctx, c.w, run, req)
}

func (c *appController) Reconcile(ctx context.Context, req v1.ReconcileRequest) (v1.ReconcileResponse, error) {
	out, err := c.app.Reconciliation.Reconcile(ctx, c.w, req)
	if err == nil && c.losing("reconcile") {
		return v1.ReconcileResponse{}, errLost
	}
	c.mu.Lock()
	between := c.between
	c.between = nil
	c.mu.Unlock()
	if between != nil {
		between()
	}
	return out, err
}

func (c *appController) ReportRun(ctx context.Context, run string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
	out, err := c.app.Runs.Report(ctx, c.w, run, req)
	if err == nil && c.losing("report") {
		return v1.RunReportResponse{}, errLost
	}
	return out, err
}

// world is a Controller and a Worker in one process, on one fake clock.
type world struct {
	t      *testing.T
	f      *fixture
	g      *fakeGit
	ctrl   *appController
	relay  *liveRelay
	owner  pki.Session
	db     *store.DB
	sender *journal.Sender
	rng    *mrand.Rand
	epochs int
	beat   string // the daemon epoch the heartbeats carry

	pushFails bool
	lostState int
	rounds    int
	lateFrom  int // the step from which a cancel or a loss may come
	log       []string
}

func csrPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	must(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func newWorld(t *testing.T, seed uint64) *world {
	t.Helper()
	w := &world{t: t, relay: &liveRelay{refs: map[string]string{}}, ctrl: &appController{lose: map[string]bool{}}, rng: mrand.New(mrand.NewPCG(seed, seed))} //nolint:gosec // a fixed seed is the point: a failure replays
	w.g = &fakeGit{head: headOID}
	w.g.push = func(c pushCall) error {
		if w.pushFails {
			return &gitops.PushError{Class: gitops.PushRefused, Message: "refused"}
		}
		w.relay.set(c.Project, c.Branch, c.Commit)
		return nil
	}
	w.f = newFixture(t, func(o *Options) {
		o.Git, o.Controller = w.g, w.ctrl
		o.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
	})
	clk := w.f.clock
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "controller.db"), store.Options{Clock: clk})
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	w.db = db
	ca, err := pki.NewCA(clk.Now())
	must(t, err)
	ids := domain.NewIDGenerator(clk.Now, rand.Reader)
	authority := pki.NewAuthority(pki.AuthorityConfig{DB: db, CA: ca, Clock: clk, IDs: ids, CertTTL: domain.Defaults().IdentityCertTTL, EnrollWorker: workers.Enroll})
	w.ctrl.app = app.New(db, clk, ids, app.Config{Workers: workers.Config{StartedAt: clk.Now()}, RunLostTimeout: 30 * time.Minute}, runs.WithRelay(w.relay))
	owner, err := authority.BootstrapOwner(t.Context(), csrPEM(t), "owner")
	must(t, err)
	w.owner, err = owner.Session()
	must(t, err)
	invite, _, err := authority.CreateToken(t.Context(), w.owner, domain.KindWorker, time.Hour)
	must(t, err)
	joined, err := authority.Join(t.Context(), invite.Token, csrPEM(t), "alpha", &v1.WorkerSettings{})
	must(t, err)
	w.ctrl.w, err = joined.Session()
	must(t, err)
	w.restart()
	w.heartbeat()
	_, err = w.ctrl.app.Projects.Create(t.Context(), w.owner, v1.CreateProjectRequest{Name: "backend", Repo: "/srv/r"})
	must(t, err)
	return w
}

// restart is a new daemon: a new executor and journal epoch, its recovery, and
// a reconciliation still to make. With loseState its state.db is gone: the
// markers and worktrees of its Runs, and its tmux sessions, stay.
func (w *world) restart(loseState ...bool) {
	if len(loseState) > 0 && loseState[0] {
		_ = w.f.db.Close()
		path := w.f.paths.WorkerStateFile()
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				w.t.Fatal(err)
			}
		}
		db, err := state.Open(w.t.Context(), path)
		must(w.t, err)
		w.t.Cleanup(func() { _ = db.Close() })
		w.f.db, w.f.opts.DB = db, db
		w.lostState++
	}
	w.epochs++
	w.beat = ulid.Make().String()
	w.f.restart(w.beat)
	w.f.e.reconciledGen.Store(0)
	w.f.e.heartbeatTaken.Store(false) // until its first heartbeat is taken
	w.f.e.spawn = func(state.Key) {}
	w.sender = &journal.Sender{
		DB: w.f.db, Reporter: w.ctrl, Journal: w.f.j, Clock: w.f.clock, Log: slog.New(slog.DiscardHandler),
		OnView: w.f.e.ConfirmView, OnFence: func(state.Key) { w.f.e.RequestReconcile() },
	}
	must(w.t, w.f.e.recoverRuns(w.t.Context()))
}

func (w *world) heartbeat() {
	_, err := w.ctrl.app.Workers.Heartbeat(w.t.Context(), w.ctrl.w, v1.HeartbeatRequest{
		Epoch: w.beat, Hostname: "box", OS: "linux", Arch: "amd64", Capabilities: []string{"harness:command", domain.RepoConfigCapability},
		Total: v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 1000}, Health: v1.Health{Status: v1.HealthOK},
	})
	must(w.t, err)
	w.f.e.HeartbeatTaken()
}

func (w *world) longPoll(lose bool) {
	ctx := w.t.Context()
	cursor, err := state.Cursor(ctx, w.f.db.Reader())
	must(w.t, err)
	page, err := w.ctrl.Assignments(ctx, cursor, 0, 0)
	must(w.t, err)
	if lose || len(page.Commands) == 0 {
		return
	}
	_, err = w.f.e.persist(ctx, page.Commands)
	must(w.t, err)
	if cursor, err = state.Cursor(ctx, w.f.db.Reader()); err == nil && cursor > 0 {
		_, _ = w.ctrl.AckCommands(ctx, cursor)
	}
}

func (w *world) reconcile(lose bool) {
	w.rounds++
	if w.rng != nil && w.rng.IntN(4) == 0 {
		// The orders of the queue are consumed between an answer and its use.
		w.ctrl.mu.Lock()
		w.ctrl.between = func() { w.longPoll(false) }
		w.ctrl.mu.Unlock()
	}
	if lose {
		w.ctrl.mu.Lock()
		w.ctrl.lose["reconcile"] = true
		w.ctrl.mu.Unlock()
	}
	gen := w.f.e.reconcileGen.Load()
	if err := w.f.e.reconcile(w.t.Context(), gen); err == nil && w.f.e.reconcileGen.Load() == gen {
		w.f.e.reconciledGen.Store(gen)
	}
	w.ctrl.mu.Lock()
	delete(w.ctrl.lose, "reconcile")
	w.ctrl.mu.Unlock()
}

func (w *world) flush(lose bool) {
	if lose {
		w.ctrl.mu.Lock()
		w.ctrl.lose["report"] = true
		w.ctrl.mu.Unlock()
	}
	_, _ = w.sender.Flush(w.t.Context())
	w.ctrl.mu.Lock()
	delete(w.ctrl.lose, "report")
	w.ctrl.mu.Unlock()
}

func (w *world) stepAll() {
	live, err := state.LiveRuns(w.t.Context(), w.f.db.Reader())
	must(w.t, err)
	for _, r := range live {
		w.f.e.step(w.t.Context(), r.Key)
	}
}

// harnessExit ends the harness of a Run whose session is alive: it leaves the
// markers of a wrapper that ran it and exited, and its session ends.
func (w *world) harnessExit(r state.Run) {
	name := cfruntime.NameFor(r.RunID, r.LeaseEpoch)
	if st, _ := w.f.rt.Status(w.t.Context(), w.f.rt.HandleFor(name)); !st.Alive {
		return
	}
	wr := newWrapperProcess(w.t, "exit 0")
	<-wr.ended
	must(w.t, os.MkdirAll(w.f.runDir(r.Key), 0o700))
	w.f.writeWrapper(r.Key, wr.w)
	w.f.writeStarted(r.Key, wr.w, w.f.clock.Now())
	w.f.writeExit(r.Key, execwrap.Exit{ExitCode: intp(0), ExitedAt: w.f.clock.Now(), Cause: execwrap.CauseExit})
	w.f.rt.setAlive(name, false)
}

func (w *world) publish(fails bool) {
	w.pushFails = fails
	w.f.passBackoff()
	for i := 0; i < 8; i++ {
		if !w.f.turn() {
			break
		}
	}
	w.pushFails = false
}

func (w *world) runs() []store.Run {
	var out []store.Run
	must(w.t, w.db.Read(w.t.Context(), func(q store.Querier) (err error) {
		out, err = store.Runs(w.t.Context(), q, store.RunFilter{})
		return err
	}))
	return out
}

// step does one thing, chosen at random. A cancel and a loss come at most once,
// and only late enough for the Run to have got somewhere, when the seed says so.
func (w *world) step(i int, cancelled, lost *bool, task string) {
	ctx := w.t.Context()
	action := w.rng.IntN(13)
	if action == 7 || action == 8 {
		// Mostly while the Run runs, which is where they interfere; seldom before.
		running := false
		for _, r := range w.runs() {
			running = running || r.Runtime == domain.RuntimeActive && !r.State.IsTerminal()
		}
		if i < w.lateFrom || (!running && w.rng.IntN(10) != 0) {
			action = 2
		}
	}
	if action == 11 && w.rng.IntN(4) != 0 {
		action = 1
	}
	if action == 3 && w.rng.IntN(3) != 0 {
		action = 5 // a harness lives a while: the reports of its start get there first
	}
	switch action {
	case 0:
		w.longPoll(w.rng.IntN(5) == 0)
	case 1:
		if w.rng.IntN(5) == 0 {
			w.ctrl.mu.Lock()
			w.ctrl.lose["accept"] = true // the accept is taken, and its answer lost
			w.ctrl.mu.Unlock()
		}
		w.f.e.decideOnce(ctx)
		w.ctrl.mu.Lock()
		delete(w.ctrl.lose, "accept")
		w.ctrl.mu.Unlock()
	case 2:
		w.stepAll()
	case 3:
		live, err := state.LiveRuns(ctx, w.f.db.Reader())
		must(w.t, err)
		if len(live) > 0 {
			w.harnessExit(live[w.rng.IntN(len(live))])
		}
	case 4:
		w.publish(w.rng.IntN(3) == 0)
	case 5:
		w.flush(w.rng.IntN(4) == 0)
	case 6:
		w.reconcile(w.rng.IntN(5) == 0)
	case 7:
		if !*cancelled {
			*cancelled = true
			_, _ = w.ctrl.app.Tasks.Cancel(ctx, w.owner, task, "", v1.ActionRequest{})
		}
	case 8:
		if !*lost {
			*lost = true
			w.f.clock.Advance(31 * time.Minute)
			_, err := w.ctrl.app.Reconciliation.SweepLost(ctx)
			must(w.t, err)
		}
	case 9:
		w.restart(w.rng.IntN(4) == 0)
	case 10:
		_, err := w.ctrl.app.Scheduler.Pass(ctx)
		must(w.t, err)
	case 12:
		// A human asks for another try of a publication that failed (cf run publish).
		for _, r := range w.runs() {
			if r.Publication == domain.PublicationFailed {
				_, _ = w.ctrl.app.Runs.Publish(ctx, w.owner, string(r.ID), v1.ActionRequest{})
			}
		}
	case 11:
		w.f.clock.Advance(domain.Defaults().LeaseAcceptBy + time.Second)
		_, err := w.ctrl.app.Leases.SweepExpired(ctx)
		must(w.t, err)
	}
	w.log = append(w.log, fmt.Sprint(action))
	if action != 8 {
		w.heartbeat()
	}
}

// drain lets everything finish: no answer is lost, no push fails, and every
// harness still running ends.
func (w *world) drain() (rounds int) {
	ctx := w.t.Context()
	start := w.rounds
	for range 60 {
		w.heartbeat()
		w.longPoll(false)
		if !w.f.e.Reconciled() {
			w.reconcile(false)
		}
		w.f.e.decideOnce(ctx)
		w.stepAll()
		live, err := state.LiveRuns(ctx, w.f.db.Reader())
		must(w.t, err)
		for _, r := range live {
			if r.Phase == state.PhaseRunning || r.Phase == state.PhaseStopping {
				w.harnessExit(r)
			}
		}
		w.f.clock.Advance(noWrapperLimit + time.Second)
		w.stepAll()
		w.publish(false)
		w.flush(false)
		_, err = w.ctrl.app.Scheduler.Pass(ctx)
		must(w.t, err)
		if w.quiet() {
			w.reconcile(false) // one more: what reconciles settles
			w.publish(false)
			w.flush(false)
			if w.quiet() {
				return w.rounds - start
			}
		}
	}
	w.dump()
	w.t.Fatalf("no convergence after the drain; actions %v", w.log)
	return 0
}

// quiet says nothing is left to do: no Run live here, no report on its way, no
// publication trying, and every Run of the Controller over with its reservation released.
func (w *world) quiet() bool {
	ctx := w.t.Context()
	live, err := state.LiveRuns(ctx, w.f.db.Reader())
	must(w.t, err)
	pending, err := state.PendingJournal(ctx, w.f.db.Reader(), 10)
	must(w.t, err)
	unsettled, err := state.UnsettledPublications(ctx, w.f.db.Reader())
	must(w.t, err)
	if len(live) > 0 || len(pending) > 0 || len(unsettled) > 0 {
		return false
	}
	for _, r := range w.runs() {
		if !r.State.IsTerminal() {
			return false
		}
		if r.LeaseID != "" && r.LeaseState == domain.LeaseAccepted {
			var open bool
			must(w.t, w.db.Read(ctx, func(q store.Querier) (err error) {
				open, err = store.OpenReservation(ctx, q, r.LeaseID)
				return err
			}))
			if open {
				return false
			}
		}
	}
	return true
}

func TestConvergence_RandomInterleavingsOfAWorkerAndTheController(t *testing.T) {
	for seed := convergenceFrom(); seed < convergenceFrom()+convergenceSeeds(); seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			w := newWorld(t, seed)
			created, err := w.ctrl.app.Tasks.Create(t.Context(), w.owner, v1.CreateTaskRequest{
				Project: "backend", Prompt: "echo hi", ResolvedRef: "main", SourceCommit: "0123456789abcdef0123456789abcdef01234567",
				Requirements: v1.Requirements{Harness: "command"},
			})
			must(t, err)
			_, err = w.ctrl.app.Scheduler.Pass(t.Context())
			must(t, err)
			var cancelled, lost bool
			w.lateFrom = w.rng.IntN(20)
			for i := range 100 {
				w.step(i, &cancelled, &lost, created.Task.ID)
			}
			rounds := w.drain()
			for _, r := range w.runs() {
				t.Logf("run %s epoch %d: %s runtime %s publication %s end %q (state lost %d)", r.ID, r.LeaseEpoch, r.State, r.Runtime, r.Publication, r.EndReason, w.lostState)
			}
			if rounds > 6 {
				t.Errorf("%d rounds of reconciliation to drain: actions %v", rounds, w.log)
			}
			w.check(t)
		})
	}
}

// check is the invariants once everything settled.
func (w *world) check(t *testing.T) {
	ctx := t.Context()
	for _, r := range w.runs() {
		if r.State == domain.RunQueued {
			continue
		}
		counts := map[domain.EventType]int{}
		evs, err := w.db.Events(ctx, 0, 10000, store.EventFilter{})
		must(t, err)
		for _, ev := range evs {
			if ev.RunID == r.ID {
				counts[ev.Type]++
			}
		}
		for _, typ := range []domain.EventType{domain.EventRunStopped, domain.EventRunPublished, domain.EventRunOrphanPreserved} {
			if counts[typ] > 1 {
				t.Errorf("%s: %d %s: actions %v", r.ID, counts[typ], typ, w.log)
			}
		}
		orphans := map[uint64]int{}
		for _, ev := range evs {
			if ev.RunID == r.ID && ev.Type == domain.EventRunOrphaned {
				epoch, _ := ev.Data["lease_epoch"].(float64)
				orphans[uint64(epoch)]++
			}
		}
		for epoch, n := range orphans {
			if n > 1 {
				t.Errorf("%s: %d RunOrphaned under epoch %d: actions %v", r.ID, n, epoch, w.log)
			}
		}
		if r.Runtime == domain.RuntimeActive {
			t.Errorf("%s: runtime still active: actions %v", r.ID, w.log)
		}
		if r.Publication == domain.PublicationPending && r.ResultCommit == "" {
			t.Errorf("%s: a pending publication with nothing sealed: actions %v", r.ID, w.log)
		}
		// What the Worker holds of it is coherent with the Controller.
		local, err := state.PublicationOf(ctx, w.f.db.Reader(), state.Key{RunID: string(r.ID), LeaseEpoch: r.LeaseEpoch})
		if err == nil {
			switch r.Publication {
			case domain.PublicationPublished:
				if local.State != state.PubPublished {
					t.Errorf("%s: published, and %s on the Worker: actions %v", r.ID, local.State, w.log)
				}
			case domain.PublicationFailed:
				if local.State != state.PubFailed && local.State != state.PubPublished {
					t.Errorf("%s: failed, and %s on the Worker: actions %v", r.ID, local.State, w.log)
				}
			case domain.PublicationPending:
				t.Errorf("%s: pending on the Controller while the Worker is %s: actions %v", r.ID, local.State, w.log)
			}
			if r.Publication == domain.PublicationPublished {
				if tip, ok, _ := w.relay.get(string(mustTaskProject(t, w, r.TaskID)), r.Branch); !ok || tip != r.ResultCommit {
					t.Errorf("%s: published, and the relay has %q: actions %v", r.ID, tip, w.log)
				}
			}
		}
	}
}

func mustTaskProject(t *testing.T, w *world, task domain.ID) domain.ID {
	t.Helper()
	var p domain.ID
	must(t, w.db.Read(t.Context(), func(q store.Querier) error {
		tk, err := store.TaskByID(t.Context(), q, task)
		p = tk.ProjectID
		return err
	}))
	return p
}

// dump says where things stand, for a drain that did not converge.
func (w *world) dump() {
	ctx := w.t.Context()
	for _, r := range w.runs() {
		var open bool
		if r.LeaseID != "" {
			_ = w.db.Read(ctx, func(q store.Querier) (err error) { open, err = store.OpenReservation(ctx, q, r.LeaseID); return err })
		}
		w.t.Logf("controller: %s e%d %s runtime %s pub %s result %q harness_ran %v lease %s reservation open %v", r.ID, r.LeaseEpoch, r.State, r.Runtime, r.Publication, r.ResultCommit, r.HarnessRan, r.LeaseState, open)
	}
	all, _ := state.AllRuns(ctx, w.f.db.Reader())
	for _, r := range all {
		w.t.Logf("worker: %s e%d phase %s fenced %q orphaned %v adopted %v started %v", r.RunID, r.LeaseEpoch, r.Phase, r.FencedCode, r.OrphanedAt != nil, r.Adopted, r.StartedAt != nil)
	}
	w.logJournal(ctx)
	pubs, _ := state.UnsettledPublications(ctx, w.f.db.Reader())
	for _, p := range pubs {
		w.t.Logf("publication: %s e%d %s attempts %d next %v err %q", p.RunID, p.LeaseEpoch, p.State, p.Attempts, p.NextAttemptAt, p.LastError)
	}
	stops, _ := state.PendingStopOrders(ctx, w.f.db.Reader())
	for _, o := range stops {
		w.t.Logf("stop: %s e%d %s", o.RunID, o.LeaseEpoch, o.Reason)
	}
}

// convergenceSeeds is how many seeds the test runs: 30, or CF_CONVERGENCE_SEEDS
// for a longer search; CF_CONVERGENCE_FROM is the first.
func convergenceSeeds() uint64 { return envUint("CF_CONVERGENCE_SEEDS", 30) }

func convergenceFrom() uint64 { return envUint("CF_CONVERGENCE_FROM", 1) }

func envUint(name string, def uint64) uint64 {
	var n uint64
	if _, err := fmt.Sscan(os.Getenv(name), &n); err != nil || n == 0 {
		return def
	}
	return n
}

func (w *world) logJournal(ctx context.Context) {
	q, err := w.f.db.Reader().QueryContext(ctx, `SELECT seq, run_id, lease_epoch, kind, payload FROM journal ORDER BY seq`)
	if err != nil {
		return
	}
	defer func() { _ = q.Close() }()
	for q.Next() {
		var (
			seq            uint64
			run, kind, raw string
			epoch          uint64
		)
		_ = q.Scan(&seq, &run, &epoch, &kind, &raw)
		w.t.Logf("journal: %d %s e%d %s %s", seq, run, epoch, kind, raw)
	}
	if err := q.Err(); err != nil {
		w.t.Logf("journal: %v", err)
	}
}
