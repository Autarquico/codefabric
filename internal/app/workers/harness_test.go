package workers_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const (
	controllerVersion = "v0.3.1"
	offlineTTL        = 45 * time.Second
)

// env is a real store and Authority with a fake clock, and the Worker use cases on them.
type env struct {
	t         *testing.T
	db        *store.DB
	clock     *clock.Fake
	ca        *pki.CA
	ids       *domain.IDGenerator
	authority *pki.Authority
	svc       *workers.Service
	owner     pki.Session
	seeded    int
}

func newEnv(t *testing.T, version string) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "controller.db"), store.Options{Clock: clk})
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ca, err := pki.NewCA(clk.Now())
	must(t, err)
	e := &env{
		t: t, db: db, clock: clk, ca: ca, ids: domain.NewIDGenerator(clk.Now, rand.Reader),
		svc: workers.New(db, clk, workers.Config{ControllerVersion: version, OfflineTTL: offlineTTL}),
	}
	e.authority = e.newAuthority(workers.Enroll)
	owner, err := e.authority.BootstrapOwner(t.Context(), newCSR(t), "owner")
	must(t, err)
	e.owner, err = owner.Session()
	must(t, err)
	return e
}

func (e *env) newAuthority(enroll pki.EnrollWorker) *pki.Authority {
	return pki.NewAuthority(pki.AuthorityConfig{
		DB: e.db, CA: e.ca, Clock: e.clock, IDs: e.ids, CertTTL: domain.Defaults().IdentityCertTTL,
		EnrollWorker: enroll,
	})
}

func newCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	must(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// token issues a single-use worker invitation.
func (e *env) token() string {
	e.t.Helper()
	invite, _, err := e.authority.CreateToken(e.t.Context(), e.owner, domain.KindWorker, time.Hour)
	must(e.t, err)
	return invite.Token
}

// join enrols a Worker through the Authority, as POST /v1/join does, and
// returns the session of its new identity.
func (e *env) join(name string, settings ...v1.WorkerSettings) pki.Session {
	e.t.Helper()
	s, err := e.tryJoin(e.token(), name, settings...)
	must(e.t, err)
	return s
}

func (e *env) tryJoin(token, name string, settings ...v1.WorkerSettings) (pki.Session, error) {
	e.t.Helper()
	worker := v1.WorkerSettings{}
	if len(settings) > 0 {
		worker = settings[0]
	}
	joined, err := e.authority.Join(e.t.Context(), token, newCSR(e.t), name, &worker)
	if err != nil {
		return pki.Session{}, err
	}
	s, err := joined.Session()
	must(e.t, err)
	return s, nil
}

// request is a valid heartbeat of a daemon instance with the given epoch.
func request(epoch string, mutate ...func(*v1.HeartbeatRequest)) v1.HeartbeatRequest {
	req := v1.HeartbeatRequest{
		Epoch: epoch, DaemonVersion: controllerVersion, Hostname: "box.lan", OS: "linux", Arch: "amd64",
		Total:  v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 500000},
		Health: v1.Health{Status: v1.HealthOK},
	}
	for _, m := range mutate {
		m(&req)
	}
	return req
}

func withVersion(v string) func(*v1.HeartbeatRequest) {
	return func(r *v1.HeartbeatRequest) { r.DaemonVersion = v }
}

func newEpoch() string { return ulid.Make().String() }

// beat sends a heartbeat and returns the state the Controller holds the Worker in.
func (e *env) beat(s pki.Session, req v1.HeartbeatRequest) domain.WorkerState {
	e.t.Helper()
	out, err := e.svc.Heartbeat(e.t.Context(), s, req)
	must(e.t, err)
	return out.State
}

func (e *env) sweep() {
	e.t.Helper()
	must(e.t, e.svc.Sweep(e.t.Context()))
}

func (e *env) worker(ref string) v1.Worker {
	e.t.Helper()
	w, err := e.svc.Get(e.t.Context(), ref)
	must(e.t, err)
	return w
}

func (e *env) state(s pki.Session) domain.WorkerState {
	e.t.Helper()
	return e.worker(string(s.Principal.ID)).State
}

// events returns the committed events of the types, oldest first.
func (e *env) events(types ...domain.EventType) []domain.Event {
	e.t.Helper()
	all, err := e.db.Events(e.t.Context(), 0, 1000, store.EventFilter{Types: types})
	must(e.t, err)
	return all
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	must(e.t, e.db.Reader().QueryRowContext(e.t.Context(), query, args...).Scan(&n))
	return n
}

// seedRun inserts a Run assigned to the Worker, in the given state.
func (e *env) seedRun(worker string, state domain.RunState) domain.ID {
	e.t.Helper()
	ctx := e.t.Context()
	e.seeded++
	n := e.seeded
	short := uint64(n) //nolint:gosec // a small counter
	id := domain.ID(fmt.Sprintf("run_%d", n))
	must(e.t, e.db.WithTx(ctx, func(tx *store.Tx) error {
		if n == 1 {
			p := store.Project{ID: "project_1", Short: 1, Name: "p", Repo: "r", DefaultBranch: "main", CreatedAt: e.clock.Now()}
			if err := tx.InsertProject(ctx, p); err != nil {
				return err
			}
			task := store.Task{
				ID: "task_1", Short: 1, ProjectID: p.ID, Name: "t", Prompt: "x", State: domain.TaskRunning,
				CreatedBy: domain.CreatorHuman, CreatedAt: e.clock.Now(),
			}
			if err := tx.InsertTask(ctx, task); err != nil {
				return err
			}
		}
		run := store.Run{
			ID: id, Short: short, TaskID: "task_1", Attempt: n, Lineage: domain.LineageInitial, Spec: []byte(`{}`),
			SpecDigest: "d", SourceCommit: "c", State: state, Publication: domain.PublicationNone,
			Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, WorkerID: domain.ID(worker), CreatedAt: e.clock.Now(),
		}
		if state.IsTerminal() {
			finished := e.clock.Now()
			run.FinishedAt = &finished
		}
		return tx.InsertRun(ctx, run)
	}))
	return id
}

// finishRun ends a seeded Run.
func (e *env) finishRun(id domain.ID) {
	e.t.Helper()
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(e.t.Context(), `UPDATE runs SET state = 'cancelled', finished_at = ? WHERE id = ?`, e.clock.Now().UnixNano(), string(id))
		return err
	}))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code domain.Code) {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != code {
		t.Fatalf("got %v, want code %s", err, code)
	}
}
