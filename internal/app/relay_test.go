package app_test

import (
	"crypto/rand"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// token asks for the token of the Run's Worker under the lease epoch of the reporter.
func (r *reporter) token() v1.RelayTokenResponse {
	r.e.t.Helper()
	tok, err := r.e.app.Runs.MintRelayToken(r.e.t.Context(), r.w, r.run.ID, v1.RelayTokenRequest{LeaseEpoch: r.lease.Epoch})
	must(r.e.t, err)
	return tok
}

func (r *reporter) projectID() string {
	r.e.t.Helper()
	task, err := r.e.app.Tasks.Get(r.e.t.Context(), r.run.TaskID, "")
	must(r.e.t, err)
	return task.ProjectID
}

func (e *env) authorize(w pki.Session, project string, write bool, token string) (runs.RelayGrant, error) {
	return e.app.Runs.AuthorizeRelay(e.t.Context(), w, project, write, token)
}

func TestRelayAuth_ReadIsForTheProjectOfTheLeaseAndWriteForTheSealedResultOnly(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	project := r.projectID()

	tok := r.token()
	if tok.Write || tok.Path != "/v1/relay/"+project+".git" || !tok.ExpiresAt.Equal(e.clock.Now().Add(runs.RelayTokenTTL)) {
		t.Errorf("token = %+v", tok)
	}
	g, err := e.authorize(r.w, project, false, tok.Token)
	if err != nil || g.Project.String() != project || g.Write {
		t.Fatalf("read = %+v (%v)", g, err)
	}
	// Writing needs a sealed result waiting to be published.
	_, err = e.authorize(r.w, project, true, tok.Token)
	wantCode(t, err, domain.CodePolicyDenied)

	r.exitedAndStopped(0)
	_, err = e.authorize(r.w, project, true, r.token().Token)
	wantCode(t, err, domain.CodePolicyDenied) // pending, but not sealed

	r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1)))
	tok = r.token()
	if !tok.Write {
		t.Error("a sealed, pending Run gets no write token")
	}
	g, err = e.authorize(r.w, project, true, tok.Token)
	run := e.storedRun(r.run.ID)
	if err != nil || !g.Write || g.ExpectedOID != sha1 || g.Branch != run.Branch || g.Worker.String() != string(run.WorkerID) {
		t.Fatalf("write = %+v (%v), branch %q", g, err, run.Branch)
	}

	// The token of before the sealing still reads, and no longer is a pass to write after publishing.
	e.pushes(r, sha1)
	r.mustSend(r.entry(v1.ReportPublished, publishedAs(sha1)))
	_, err = e.authorize(r.w, project, true, tok.Token)
	wantCode(t, err, domain.CodePolicyDenied)
	if _, err := e.authorize(r.w, project, false, tok.Token); err != nil {
		t.Errorf("read after publishing: %v", err)
	}
}

func TestScenario_GitRelayPublication_FetchNoAutorizado_Controller(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	other := e.joinWorker("beta") // no Runs at all
	project := r.projectID()
	tok := r.token()

	// A Worker with no Runs cannot get a token, and cannot use another's.
	_, err := e.app.Runs.MintRelayToken(t.Context(), other, r.run.ID, v1.RelayTokenRequest{LeaseEpoch: r.lease.Epoch})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.authorize(other, project, false, tok.Token)
	wantCode(t, err, domain.CodePolicyDenied)
	_, err = e.authorize(other, project, false, "")
	wantCode(t, err, domain.CodePolicyDenied)

	// A client is not a Worker.
	_, err = e.authorize(e.owner, project, false, tok.Token)
	wantCode(t, err, domain.CodeUnauthenticated)
}

func TestRelayAuth_EveryLinkOfTheChainIsCheckedOnEveryRequest(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	project := r.projectID()
	tok := r.token().Token

	// Another Project.
	e.project("other")
	otherProject, err := e.app.Projects.Get(t.Context(), "other")
	must(t, err)
	_, err = e.authorize(r.w, otherProject.ID, false, tok)
	wantCode(t, err, domain.CodePolicyDenied)
	_, err = e.authorize(r.w, "task_01ARZ3NDEKTSV4RRFFQ69G5FAV", false, tok)
	wantCode(t, err, domain.CodePolicyDenied)

	// A token that was tampered with, truncated, or made for nothing.
	flipped := tok[:len(tok)-2] + "AA"
	for name, bad := range map[string]string{"tampered": flipped, "no signature": strings.Split(tok, ".")[0], "garbage": "x.y", "empty": ""} {
		if _, err := e.authorize(r.w, project, false, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A token from before a restart of the Controller: another key.
	restarted := app.New(e.db, e.clock, domain.NewIDGenerator(e.clock.Now, rand.Reader), workers.Config{}, runs.WithRelay(e.relay))
	_, err = restarted.Runs.AuthorizeRelay(t.Context(), r.w, project, false, tok)
	wantCode(t, err, domain.CodePolicyDenied)

	// The lease epoch moved on.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET lease_epoch = 2 WHERE id = ?`, r.run.ID)
		return err
	}))
	_, err = e.authorize(r.w, project, false, tok)
	wantCode(t, err, domain.CodePolicyDenied)
	_, err = e.app.Runs.MintRelayToken(t.Context(), r.w, r.run.ID, v1.RelayTokenRequest{LeaseEpoch: r.lease.Epoch})
	wantCode(t, err, domain.CodeStaleLease)
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE runs SET lease_epoch = 1 WHERE id = ?`, r.run.ID)
		return err
	}))
	if _, err := e.authorize(r.w, project, false, tok); err != nil {
		t.Fatalf("restored: %v", err)
	}

	// The Task was archived.
	_, err = e.app.Tasks.Cancel(t.Context(), e.owner, r.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	_, err = e.app.Tasks.Archive(t.Context(), e.owner, r.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	_, err = e.authorize(r.w, project, false, tok)
	wantCode(t, err, domain.CodePolicyDenied)
	_, err = e.app.Runs.MintRelayToken(t.Context(), r.w, r.run.ID, v1.RelayTokenRequest{LeaseEpoch: r.lease.Epoch})
	wantCode(t, err, domain.CodeNotFound)
}

func TestRelayAuth_ATokenExpires(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	project := r.projectID()
	tok := r.token().Token
	e.clock.Advance(runs.RelayTokenTTL - time.Second)
	if _, err := e.authorize(r.w, project, false, tok); err != nil {
		t.Fatalf("before it expires: %v", err)
	}
	e.clock.Advance(2 * time.Second)
	_, err := e.authorize(r.w, project, false, tok)
	wantCode(t, err, domain.CodePolicyDenied)
}

func TestRelayAuth_ATokenIsForTheWorkerItWasIssuedTo(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	beta := e.joinWorker("beta")
	// Beta also has a Run in the same Project.
	second := e.acceptedRun(beta, "y")
	if second.projectID() != r.projectID() {
		t.Skip("the Runs are in different Projects")
	}
	stolen := r.token().Token
	_, err := e.authorize(beta, r.projectID(), false, stolen)
	wantCode(t, err, domain.CodePolicyDenied)
	if _, err := e.authorize(beta, r.projectID(), false, second.token().Token); err != nil {
		t.Errorf("its own token: %v", err)
	}
}

func TestMintRelayToken_ChecksTheShapeOfTheRequest(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	_, err := e.app.Runs.MintRelayToken(t.Context(), r.w, "nonsense", v1.RelayTokenRequest{LeaseEpoch: 1})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Runs.MintRelayToken(t.Context(), r.w, r.run.ID, v1.RelayTokenRequest{LeaseEpoch: 7})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Runs.MintRelayToken(t.Context(), e.owner, r.run.ID, v1.RelayTokenRequest{LeaseEpoch: 1})
	wantCode(t, err, domain.CodeUnauthenticated)
}
