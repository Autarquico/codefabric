package app_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

type secretsAudit struct{ action, before, after string }

// secretsAudits are the audit entries of allowlist changes, oldest first.
func (e *env) secretsAudits() []secretsAudit {
	e.t.Helper()
	rows, err := e.db.Reader().QueryContext(e.t.Context(),
		`SELECT action, before, after FROM audit_entries WHERE action LIKE 'project.secrets.%' ORDER BY id`)
	must(e.t, err)
	defer func() { _ = rows.Close() }()
	var out []secretsAudit
	for rows.Next() {
		var a secretsAudit
		must(e.t, rows.Scan(&a.action, &a.before, &a.after))
		out = append(out, a)
	}
	must(e.t, rows.Err())
	return out
}

// holds makes the Worker report refs, each scoped to projects (none: any).
func (e *env) holds(w pki.Session, seq uint64, refs map[string][]string) {
	e.t.Helper()
	report := store.SecretsReport{Generation: 1, Epoch: "e1", Seq: seq}
	for ref, projects := range refs {
		report.Refs = append(report.Refs, v1.SecretMeta{Ref: ref, Backend: v1.SecretBackendFile, Projects: projects, UpdatedAt: e.clock.Now()})
	}
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
		_, err := tx.ReplaceWorkerSecrets(e.t.Context(), w.Principal.ID, report, e.clock.Now())
		return err
	}))
}

func TestProjectSecrets_AllowDenyListAndAudit(t *testing.T) {
	e := newEnv(t)
	p := e.project("backend")
	ctx := t.Context()
	events := e.count("events")

	got, err := e.app.Projects.AllowSecrets(ctx, e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"npm/token", "gh/token"}})
	must(t, err)
	if got.ProjectID != p.ID || !equal(got.Allowed, []string{"gh/token", "npm/token"}) {
		t.Fatalf("allow = %+v", got)
	}
	// Allowing what is allowed and denying what is not change nothing, and
	// nothing is audited for them.
	_, err = e.app.Projects.AllowSecrets(ctx, e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"gh/token"}})
	must(t, err)
	_, err = e.app.Projects.DenySecrets(ctx, e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"other"}})
	must(t, err)
	got, err = e.app.Projects.DenySecrets(ctx, e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"gh/token", "other"}})
	must(t, err)
	if !equal(got.Allowed, []string{"npm/token"}) {
		t.Fatalf("deny = %+v", got)
	}
	shown, err := e.app.Projects.Secrets(ctx, p.Short)
	must(t, err)
	if !equal(shown.Allowed, []string{"npm/token"}) {
		t.Errorf("secrets = %+v", shown)
	}

	want := []secretsAudit{
		{"project.secrets.allow", `[]`, `["gh/token","npm/token"]`},
		{"project.secrets.deny", `["gh/token","npm/token"]`, `["npm/token"]`},
	}
	if got := e.secretsAudits(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audit = %v, want %v", got, want)
	}
	// The scheduler and the grant read the allowlist when they act: there is
	// nobody to wake, so no event is written.
	if n := e.count("events"); n != events {
		t.Errorf("%d events written", n-events)
	}
}

func TestProjectSecrets_RequestIDIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	ctx := t.Context()
	req := v1.ProjectSecretsRequest{RequestID: "allow-1", Refs: []string{"gh/token"}}

	first, err := e.app.Projects.AllowSecrets(ctx, e.owner, "backend", req)
	must(t, err)
	_, err = e.app.Projects.DenySecrets(ctx, e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"gh/token"}})
	must(t, err)
	// The repeat answers what the first call did and does not allow again.
	again, err := e.app.Projects.AllowSecrets(ctx, e.owner, "backend", req)
	must(t, err)
	if fmt.Sprint(again) != fmt.Sprint(first) {
		t.Errorf("repeat = %+v, first = %+v", again, first)
	}
	if now, _ := e.app.Projects.Secrets(ctx, "backend"); len(now.Allowed) != 0 {
		t.Errorf("the repeat allowed again: %+v", now)
	}
	if n := len(e.secretsAudits()); n != 2 {
		t.Errorf("%d audit entries, want 2", n)
	}
	_, err = e.app.Projects.AllowSecrets(ctx, e.owner, "backend", v1.ProjectSecretsRequest{RequestID: "allow-1", Refs: []string{"other"}})
	wantCode(t, err, domain.CodeIdempotencyMismatch)
	// The same request_id names another operation on deny.
	_, err = e.app.Projects.DenySecrets(ctx, e.owner, "backend", req)
	must(t, err)
}

func TestProjectSecrets_Validation(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	ctx := t.Context()
	for name, refs := range map[string][]string{
		"no refs":       nil,
		"a glob":        {"gh/*"},
		"a leading dot": {".token"},
		"a space":       {"gh token"},
		"a repeat":      {"a", "a"},
		"too long":      {strings.Repeat("a", domain.MaxSecretRefLen+1)},
	} {
		_, err := e.app.Projects.AllowSecrets(ctx, e.owner, "backend", v1.ProjectSecretsRequest{Refs: refs})
		if err == nil {
			t.Errorf("%s was allowed", name)
			continue
		}
		wantCode(t, err, domain.CodeInvalidRequest)
	}
	_, err := e.app.Projects.AllowSecrets(ctx, e.owner, "nope", v1.ProjectSecretsRequest{Refs: []string{"a"}})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Projects.Secrets(ctx, "nope")
	wantCode(t, err, domain.CodeNotFound)

	full := make([]string, domain.MaxProjectSecrets)
	for i := range full {
		full[i] = fmt.Sprintf("ref-%03d", i)
	}
	_, err = e.app.Projects.AllowSecrets(ctx, e.owner, "backend", v1.ProjectSecretsRequest{Refs: full})
	must(t, err)
	_, err = e.app.Projects.AllowSecrets(ctx, e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"one-more"}})
	wantCode(t, err, domain.CodeInvalidRequest)
	if got, _ := e.app.Projects.Secrets(ctx, "backend"); len(got.Allowed) != domain.MaxProjectSecrets {
		t.Errorf("%d allowed after a refused allow", len(got.Allowed))
	}
}

// Available names, for each allowed ref, the Workers that hold it for this
// Project: those that scope it to no Project or to this one.
func TestProjectSecrets_AvailableHonoursTheScopeOfTheWorker(t *testing.T) {
	e := newEnv(t)
	p := e.project("backend")
	other := e.project("frontend")
	ctx := t.Context()
	e.holds(e.joinWorker("alpha"), 1, map[string][]string{"gh/token": nil, "npm/token": nil})
	e.holds(e.joinWorker("beta"), 1, map[string][]string{"gh/token": {p.ID}, "npm/token": {other.ID}})
	e.holds(e.joinWorker("gamma"), 1, map[string][]string{"aws/key": nil})

	got, err := e.app.Projects.AllowSecrets(ctx, e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"gh/token", "npm/token", "pg/url"}})
	must(t, err)
	raw, _ := json.Marshal(got.Available)
	if want := `{"gh/token":["alpha","beta"],"npm/token":["alpha"],"pg/url":[]}`; string(raw) != want {
		t.Errorf("available = %s, want %s", raw, want)
	}
}

func TestTasks_SecretEnvIsValidatedAndFrozen(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	ctx := t.Context()

	bad := map[string][]v1.SecretEnv{
		"a reserved name":        {{Name: "PATH", Ref: "x"}},
		"a CF_ name":             {{Name: "CF_TOKEN", Ref: "x"}},
		"a GIT_ name":            {{Name: "GIT_ASKPASS", Ref: "x"}},
		"a TMUX name":            {{Name: "TMUX_PANE", Ref: "x"}},
		"a leading digit":        {{Name: "1TOKEN", Ref: "x"}},
		"a dash":                 {{Name: "MY-TOKEN", Ref: "x"}},
		"a repeated name":        {{Name: "A", Ref: "x"}, {Name: "A", Ref: "y"}},
		"a glob ref":             {{Name: "A", Ref: "gh/*"}},
		"an empty ref":           {{Name: "A", Ref: ""}},
		"more than MaxRunSecret": tooMany(domain.MaxRunSecrets + 1),
	}
	for name, env := range bad {
		_, err := e.createTask("backend", "p", func(r *v1.CreateTaskRequest) { r.Requirements.SecretEnv = env })
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		wantCode(t, err, domain.CodeInvalidRequest)
	}
	if n := e.count("tasks"); n != 0 {
		t.Fatalf("%d Tasks from refused requests", n)
	}

	created := e.mustTask("backend", "deploy", func(r *v1.CreateTaskRequest) {
		r.Requirements.SecretEnv = []v1.SecretEnv{{Name: "NPM_TOKEN", Ref: "npm/token"}, {Name: "GH_TOKEN", Ref: "gh/token"}}
	})
	run, err := e.app.Runs.Get(ctx, created.Run.ID)
	must(t, err)
	var spec struct {
		Schema  int             `json:"schema"`
		Secrets json.RawMessage `json:"secrets"`
	}
	must(t, json.Unmarshal(run.Spec, &spec))
	want := `{"env":[{"name":"GH_TOKEN","ref":"gh/token"},{"name":"NPM_TOKEN","ref":"npm/token"}],"refs":["gh/token","npm/token"]}`
	if spec.Schema != domain.RunSpecSchemaSecrets || string(spec.Secrets) != want {
		t.Errorf("spec = %s", run.Spec)
	}
	placement, err := runs.PlacementOf(run.Spec)
	if err != nil || !equal(placement.Secrets, []string{"gh/token", "npm/token"}) {
		t.Errorf("placement = %+v (%v)", placement, err)
	}
	if !strings.Contains(fmt.Sprint(created.Task.Requirements.SecretEnv), "GH_TOKEN") {
		t.Errorf("the Task does not show its secret_env: %+v", created.Task.Requirements)
	}
}

// A Run without secrets is frozen exactly as before secrets: schema 2, no
// secrets key, and the digest a Task with no secret_env always had.
func TestTasks_RunWithoutSecretsKeepsSchema2(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	plain := e.mustTask("backend", "fix the flaky test")
	empty := e.mustTask("backend", "fix the flaky test", func(r *v1.CreateTaskRequest) { r.Requirements.SecretEnv = []v1.SecretEnv{} })
	a, err := e.app.Runs.Get(t.Context(), plain.Run.ID)
	must(t, err)
	b, err := e.app.Runs.Get(t.Context(), empty.Run.ID)
	must(t, err)
	if string(a.Spec) != string(b.Spec) || a.SpecDigest != b.SpecDigest {
		t.Errorf("an empty secret_env changed the spec:\n%s\n%s", a.Spec, b.Spec)
	}
	if !strings.HasPrefix(string(a.Spec), `{"schema":2,`) || strings.Contains(string(a.Spec), "secrets") {
		t.Errorf("spec = %s", a.Spec)
	}
}

func tooMany(n int) []v1.SecretEnv {
	out := make([]v1.SecretEnv, n)
	for i := range out {
		out[i] = v1.SecretEnv{Name: fmt.Sprintf("V%d", i), Ref: "x"}
	}
	return out
}
