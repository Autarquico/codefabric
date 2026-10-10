package app_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const commitA = "0123456789abcdef0123456789abcdef01234567"

// env is a real store and Authority with a fake clock, and the use cases on them.
type env struct {
	t         *testing.T
	db        *store.DB
	clock     *clock.Fake
	authority *pki.Authority
	app       *app.Services
	owner     pki.Session
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "controller.db"), store.Options{Clock: clk})
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ca, err := pki.NewCA(clk.Now())
	must(t, err)
	ids := domain.NewIDGenerator(clk.Now, rand.Reader)
	e := &env{
		t: t, db: db, clock: clk,
		authority: pki.NewAuthority(pki.AuthorityConfig{DB: db, CA: ca, Clock: clk, IDs: ids, CertTTL: domain.Defaults().IdentityCertTTL}),
		app:       app.New(db, clk, ids),
	}
	e.owner = e.newClient("owner")
	return e
}

func (e *env) newClient(name string) pki.Session {
	e.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(e.t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	must(e.t, err)
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	joined, err := e.authority.BootstrapOwner(e.t.Context(), csr, name)
	must(e.t, err)
	s, err := joined.Session()
	must(e.t, err)
	return s
}

func (e *env) project(name string, mutate ...func(*v1.CreateProjectRequest)) v1.Project {
	e.t.Helper()
	req := v1.CreateProjectRequest{Name: name, Repo: "git@example.com:org/" + name + ".git"}
	for _, m := range mutate {
		m(&req)
	}
	p, err := e.app.Projects.Create(e.t.Context(), e.owner, req)
	must(e.t, err)
	return p
}

func (e *env) createTask(project, prompt string, mutate ...func(*v1.CreateTaskRequest)) (v1.CreateTaskResponse, error) {
	e.t.Helper()
	req := v1.CreateTaskRequest{Project: project, Prompt: prompt, SourceCommit: commitA}
	for _, m := range mutate {
		m(&req)
	}
	return e.app.Tasks.Create(e.t.Context(), e.owner, req)
}

func (e *env) mustTask(project, prompt string, mutate ...func(*v1.CreateTaskRequest)) v1.CreateTaskResponse {
	e.t.Helper()
	out, err := e.createTask(project, prompt, mutate...)
	must(e.t, err)
	return out
}

func noStart(r *v1.CreateTaskRequest) { r.NoStart, r.SourceCommit = true, "" }

// events returns the committed events of a type, oldest first.
func (e *env) events(types ...domain.EventType) []domain.Event {
	e.t.Helper()
	all, err := e.db.Events(e.t.Context(), 0, 1000, store.EventFilter{Types: types})
	must(e.t, err)
	return all
}

func (e *env) count(table string) int {
	e.t.Helper()
	var n int
	must(e.t, e.db.Reader().QueryRowContext(e.t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n)) //nolint:gosec // test constant
	return n
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
