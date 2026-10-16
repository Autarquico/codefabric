package app_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// grantAudit is one secret.grant audit entry.
type grantAudit struct {
	run, actor, ref, result string
	leaseEpoch              uint64
}

func (e *env) grantAudits() []grantAudit {
	e.t.Helper()
	rows, err := e.db.Reader().QueryContext(e.t.Context(),
		`SELECT resource_id, actor_id, after, result FROM audit_entries WHERE action = 'secret.grant' ORDER BY id`)
	must(e.t, err)
	defer func() { _ = rows.Close() }()
	var out []grantAudit
	for rows.Next() {
		var (
			a     grantAudit
			after string
		)
		must(e.t, rows.Scan(&a.run, &a.actor, &after, &a.result))
		var body struct {
			Ref        string `json:"ref"`
			LeaseEpoch uint64 `json:"lease_epoch"`
		}
		must(e.t, json.Unmarshal([]byte(after), &body))
		a.ref, a.leaseEpoch = body.Ref, body.LeaseEpoch
		out = append(out, a)
	}
	must(e.t, rows.Err())
	return out
}

func (e *env) secretGrants(run string) (grants []store.SecretGrant) {
	e.t.Helper()
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		grants, err = store.SecretGrantsOf(e.t.Context(), q, domain.ID(run))
		return err
	}))
	return grants
}

// grantResults is "ref=result" of each audit entry, in order.
func grantResults(audits []grantAudit) string {
	parts := make([]string, len(audits))
	for i, a := range audits {
		parts[i] = a.ref + "=" + a.result
	}
	return strings.Join(parts, " ")
}

// announces makes the daemon of the Worker report refs (scoped to projects;
// none: any) as generation 1 of its current epoch.
func (e *env) announces(w pki.Session, refs map[string][]string) {
	e.t.Helper()
	report := store.SecretsReport{Generation: 1, Epoch: e.epochOf(w)}
	for ref, projects := range refs {
		report.Refs = append(report.Refs, v1.SecretMeta{Ref: ref, Backend: v1.SecretBackendFile, Projects: projects, UpdatedAt: e.clock.Now()})
	}
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
		fence, _, err := store.WorkerSecretsFence(e.t.Context(), tx, w.Principal.ID)
		if err != nil {
			return err
		}
		report.Seq = fence.Seq + 1
		_, err = tx.ReplaceWorkerSecrets(e.t.Context(), w.Principal.ID, report, e.clock.Now())
		return err
	}))
}

func withSecretEnv(env ...v1.SecretEnv) func(*v1.CreateTaskRequest) {
	return func(r *v1.CreateTaskRequest) { r.Requirements.SecretEnv = env }
}

// secretsRun is a Run of Project backend that uses gh/token and npm/token,
// both allowed, offered to the Worker (which holds both) and accepted.
func (e *env) secretsRun(w pki.Session) *reporter {
	e.t.Helper()
	run := e.queuedRun("deploy", withSecretEnv(v1.SecretEnv{Name: "GH_TOKEN", Ref: "gh/token"}, v1.SecretEnv{Name: "NPM_TOKEN", Ref: "npm/token"}))
	_, err := e.app.Projects.AllowSecrets(e.t.Context(), e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"gh/token", "npm/token"}})
	must(e.t, err)
	e.announces(w, map[string][]string{"gh/token": nil, "npm/token": nil})
	l := e.offer(run.ID, w)
	_, err = e.accept(w, l)
	must(e.t, err)
	return &reporter{e: e, w: w, run: run, lease: l, epoch: e.epochOf(w)}
}

func (r *reporter) grantRequest(refs ...string) v1.GrantSecretsRequest {
	return v1.GrantSecretsRequest{LeaseID: string(r.lease.ID), LeaseEpoch: r.lease.Epoch, WorkerEpoch: r.e.epochOf(r.w), Refs: refs}
}

func (r *reporter) grant(req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
	return r.e.app.Runs.GrantSecrets(r.e.t.Context(), r.w, r.run.ID, req)
}

// denials is details.denied of a denial, as "ref=reason".
func denials(t *testing.T, err error) string {
	t.Helper()
	var coded *domain.Error
	if !errors.As(err, &coded) {
		t.Fatalf("err = %v, not a coded error", err)
	}
	denied, ok := coded.Details["denied"].([]v1.SecretDenial)
	if !ok {
		t.Fatalf("details = %#v", coded.Details)
	}
	parts := make([]string, len(denied))
	for i, d := range denied {
		parts[i] = d.Ref + "=" + d.Reason
	}
	return strings.Join(parts, " ")
}

func TestSecretGrant_GrantsWhatTheRunDeclaresAndAuditsEachRef(t *testing.T) {
	e := newEnv(t)
	r := e.secretsRun(e.joinWorker("alpha"))

	out, err := r.grant(r.grantRequest("npm/token", "gh/token"))
	must(t, err)
	if !slices.Equal(out.Granted, []string{"gh/token", "npm/token"}) {
		t.Fatalf("granted = %v", out.Granted)
	}
	got := e.grantAudits()
	if want := "gh/token=granted npm/token=granted"; grantResults(got) != want {
		t.Fatalf("audit = %s, want %s", grantResults(got), want)
	}
	for _, a := range got {
		if a.run != r.run.ID || a.actor != string(r.w.Principal.ID) || a.leaseEpoch != r.lease.Epoch {
			t.Errorf("audit = %+v", a)
		}
	}
}

// Every way of not being allowed to resolve a ref is denied, the denial and
// its audit entry are committed before the error is returned, and the error
// says which refs and why.
func TestSecretGrant_AuthorizationTable(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(e *env, r *reporter) (*reporter, v1.GrantSecretsRequest)
		code    domain.Code
		reason  string
	}{
		{"the Run is terminal", func(_ *env, r *reporter) (*reporter, v1.GrantSecretsRequest) {
			r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(0)), r.entry(v1.ReportStopped))
			return r, r.grantRequest("gh/token")
		}, domain.CodeStaleLease, v1.GrantRunNotPreparable},
		{"the lease is pending", func(e *env, _ *reporter) (*reporter, v1.GrantSecretsRequest) {
			run := e.queuedRun("other", withSecretEnv(v1.SecretEnv{Name: "GH_TOKEN", Ref: "gh/token"}))
			w := e.sessionOf("alpha")
			p := &reporter{e: e, w: w, run: run, lease: e.offer(run.ID, w)}
			return p, p.grantRequest("gh/token")
		}, domain.CodeStaleLease, v1.GrantStaleLease},
		{"the lease expired", func(e *env, _ *reporter) (*reporter, v1.GrantSecretsRequest) {
			run := e.queuedRun("other", withSecretEnv(v1.SecretEnv{Name: "GH_TOKEN", Ref: "gh/token"}))
			w := e.sessionOf("alpha")
			p := &reporter{e: e, w: w, run: run, lease: e.offer(run.ID, w)}
			e.clock.Advance(acceptBy)
			_, err := e.app.Leases.SweepExpired(e.t.Context())
			must(e.t, err)
			return p, p.grantRequest("gh/token")
		}, domain.CodeStaleLease, v1.GrantStaleLease},
		{"the lease epoch is stale", func(e *env, _ *reporter) (*reporter, v1.GrantSecretsRequest) {
			run := e.queuedRun("other", withSecretEnv(v1.SecretEnv{Name: "GH_TOKEN", Ref: "gh/token"}))
			w := e.sessionOf("alpha")
			old := &reporter{e: e, w: w, run: run, lease: e.offer(run.ID, w)}
			e.clock.Advance(acceptBy)
			_, err := e.app.Leases.SweepExpired(e.t.Context())
			must(e.t, err)
			_, err = e.accept(w, e.offer(run.ID, w)) // epoch 2 is the live one
			must(e.t, err)
			return old, old.grantRequest("gh/token")
		}, domain.CodeStaleLease, v1.GrantStaleLease},
		{"the lease is another Worker's", func(e *env, r *reporter) (*reporter, v1.GrantSecretsRequest) {
			beta := e.joinWorker("beta")
			e.announces(beta, map[string][]string{"gh/token": nil})
			req := r.grantRequest("gh/token")
			req.WorkerEpoch = e.epochOf(beta)
			return &reporter{e: e, w: beta, run: r.run, lease: r.lease}, req
		}, domain.CodeStaleLease, v1.GrantNotLeaseHolder},
		{"the identity is revoked", func(e *env, r *reporter) (*reporter, v1.GrantSecretsRequest) {
			must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
				return tx.Revoke(e.t.Context(), r.w.Principal.ID, domain.KindWorker, e.clock.Now())
			}))
			return r, r.grantRequest("gh/token")
		}, domain.CodeUnauthenticated, v1.GrantUnauthenticated},
		{"a later heartbeat replaced the daemon", func(e *env, r *reporter) (*reporter, v1.GrantSecretsRequest) {
			req := r.grantRequest("gh/token")
			e.epochs[r.w.Principal.ID] = ulid.Make().String()
			e.beat(r.w)
			return r, req
		}, domain.CodeStaleLease, v1.GrantDaemonRetired},
		{"a later generation fenced the daemon", func(e *env, r *reporter) (*reporter, v1.GrantSecretsRequest) {
			must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
				_, err := tx.ReplaceWorkerSecrets(e.t.Context(), r.w.Principal.ID,
					store.SecretsReport{Generation: 2, Epoch: "01J99999999999999999999999", Seq: 1}, e.clock.Now())
				return err
			}))
			return r, r.grantRequest("gh/token")
		}, domain.CodeStaleLease, v1.GrantDaemonRetired},
		{"the ref is not declared", func(e *env, r *reporter) (*reporter, v1.GrantSecretsRequest) {
			_, err := e.app.Projects.AllowSecrets(e.t.Context(), e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"pg/url"}})
			must(e.t, err)
			e.announces(r.w, map[string][]string{"gh/token": nil, "npm/token": nil, "pg/url": nil})
			return r, r.grantRequest("pg/url")
		}, domain.CodePolicyDenied, v1.SecretNotDeclared},
		{"the ref left the allowlist", func(e *env, r *reporter) (*reporter, v1.GrantSecretsRequest) {
			_, err := e.app.Projects.DenySecrets(e.t.Context(), e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"gh/token"}})
			must(e.t, err)
			return r, r.grantRequest("gh/token")
		}, domain.CodePolicyDenied, v1.SecretNotAllowlisted},
		{"the Worker does not announce the ref", func(e *env, r *reporter) (*reporter, v1.GrantSecretsRequest) {
			e.announces(r.w, map[string][]string{"npm/token": nil})
			return r, r.grantRequest("gh/token")
		}, domain.CodePolicyDenied, v1.SecretNotAnnounced},
		{"the Worker holds the ref for other Projects", func(e *env, r *reporter) (*reporter, v1.GrantSecretsRequest) {
			e.announces(r.w, map[string][]string{"gh/token": {"project_01J00000000000000000000000"}, "npm/token": nil})
			return r, r.grantRequest("gh/token")
		}, domain.CodePolicyDenied, v1.SecretOutOfScope},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			r := e.secretsRun(e.joinWorker("alpha"))
			caller, req := c.arrange(e, r)
			before := len(e.grantAudits())

			out, err := caller.grant(req)
			wantCode(t, err, c.code)
			if len(out.Granted) != 0 {
				t.Errorf("granted %v with a denial", out.Granted)
			}
			if got, want := denials(t, err), req.Refs[0]+"="+c.reason; got != want {
				t.Errorf("details.denied = %s, want %s", got, want)
			}
			// The trail was committed: it is read back after the error, from another connection.
			audits := e.grantAudits()[before:]
			if want := req.Refs[0] + "=denied:" + c.reason; grantResults(audits) != want {
				t.Errorf("audit = %q, want %q", grantResults(audits), want)
			}
			if len(audits) == 1 && (audits[0].run != caller.run.ID || audits[0].actor != string(caller.w.Principal.ID)) {
				t.Errorf("audit = %+v", audits[0])
			}
		})
	}
}

func TestScenario_SecretsLocal_Allowlist(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("deploy", withSecretEnv(v1.SecretEnv{Name: "PG_URL", Ref: "pg/url"}))
	e.announces(w, map[string][]string{"pg/url": nil})
	l := e.offer(run.ID, w)
	_, err := e.accept(w, l)
	must(t, err)
	r := &reporter{e: e, w: w, run: run, lease: l}

	// Given a ref outside the allowlist, when the Run tries to resolve it:
	_, err = r.grant(r.grantRequest("pg/url"))

	// CF_POLICY_DENIED, and an audit entry.
	wantCode(t, err, domain.CodePolicyDenied)
	if got := denials(t, err); got != "pg/url="+v1.SecretNotAllowlisted {
		t.Errorf("details.denied = %s", got)
	}
	if got := grantResults(e.grantAudits()); got != "pg/url=denied:"+v1.SecretNotAllowlisted {
		t.Errorf("audit = %s", got)
	}
	grants := e.secretGrants(run.ID)
	if len(grants) != 1 || grants[0].Granted() {
		t.Errorf("grants = %+v", grants)
	}
}

// A grant is all or nothing: one ref denied denies the batch, and each ref
// has its own result, in the answer and in the audit.
func TestSecretGrant_AMixedBatchIsDeniedWhole(t *testing.T) {
	e := newEnv(t)
	r := e.secretsRun(e.joinWorker("alpha"))
	_, err := e.app.Projects.DenySecrets(t.Context(), e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"npm/token"}})
	must(t, err)

	out, err := r.grant(r.grantRequest("gh/token", "npm/token", "pg/url"))
	wantCode(t, err, domain.CodePolicyDenied)
	if len(out.Granted) != 0 {
		t.Errorf("granted = %v", out.Granted)
	}
	if got, want := denials(t, err), "npm/token=not_allowlisted pg/url=not_declared"; got != want {
		t.Errorf("details.denied = %s, want %s", got, want)
	}
	if got, want := grantResults(e.grantAudits()), "gh/token=denied npm/token=denied:not_allowlisted pg/url=denied:not_declared"; got != want {
		t.Errorf("audit = %s, want %s", got, want)
	}
}

// A retry after a lost answer is decided again, from the state of its own
// instant: a grant given earlier is not replayed, and a deny between the two
// denies the retry without taking back the first.
func TestSecretGrant_ARetryIsAuthorisedAgain(t *testing.T) {
	e := newEnv(t)
	r := e.secretsRun(e.joinWorker("alpha"))
	req := r.grantRequest("gh/token")

	_, err := r.grant(req) // its answer is lost
	must(t, err)
	_, err = r.grant(req)
	must(t, err)
	if got := grantResults(e.grantAudits()); got != "gh/token=granted gh/token=granted" {
		t.Fatalf("audit = %s: each attempt is decided and audited", got)
	}

	_, err = e.app.Projects.DenySecrets(t.Context(), e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"gh/token"}})
	must(t, err)
	_, err = r.grant(req)
	wantCode(t, err, domain.CodePolicyDenied)

	grants := e.secretGrants(r.run.ID)
	var kept []bool
	for _, g := range grants {
		kept = append(kept, g.Granted())
	}
	if fmt.Sprint(kept) != "[true true false]" {
		t.Errorf("grants = %v: the deny took back a grant already given", kept)
	}
}

// What names nothing a grant can be kept against is an error with no trail.
func TestSecretGrant_ARequestThatNamesNothingIsRefusedWithoutAudit(t *testing.T) {
	e := newEnv(t)
	r := e.secretsRun(e.joinWorker("alpha"))
	bad := map[string]struct {
		run  string
		req  v1.GrantSecretsRequest
		code domain.Code
	}{
		"no refs":         {r.run.ID, r.grantRequest(), domain.CodeInvalidRequest},
		"a glob":          {r.run.ID, r.grantRequest("gh/*"), domain.CodeInvalidRequest},
		"a repeated ref":  {r.run.ID, r.grantRequest("gh/token", "gh/token"), domain.CodeInvalidRequest},
		"no worker epoch": {r.run.ID, func() v1.GrantSecretsRequest { q := r.grantRequest("gh/token"); q.WorkerEpoch = ""; return q }(), domain.CodeInvalidRequest},
		"epoch 0":         {r.run.ID, func() v1.GrantSecretsRequest { q := r.grantRequest("gh/token"); q.LeaseEpoch = 0; return q }(), domain.CodeInvalidRequest},
		"not a run":       {"task_01J00000000000000000000000", r.grantRequest("gh/token"), domain.CodeNotFound},
		"another lease ID": {r.run.ID, func() v1.GrantSecretsRequest {
			q := r.grantRequest("gh/token")
			q.LeaseID = "lease_01J00000000000000000000000"
			return q
		}(), domain.CodeNotFound},
		"an unknown epoch": {r.run.ID, func() v1.GrantSecretsRequest { q := r.grantRequest("gh/token"); q.LeaseEpoch = 9; return q }(), domain.CodeNotFound},
	}
	for name, c := range bad {
		_, err := e.app.Runs.GrantSecrets(t.Context(), r.w, c.run, c.req)
		if err == nil {
			t.Errorf("%s was granted", name)
			continue
		}
		wantCode(t, err, c.code)
	}
	if _, err := e.app.Runs.GrantSecrets(t.Context(), e.owner, r.run.ID, r.grantRequest("gh/token")); err == nil {
		t.Error("a client was granted")
	}
	if n := len(e.grantAudits()); n != 0 {
		t.Errorf("%d audit entries for requests that name nothing", n)
	}
}
