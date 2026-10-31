package app_test

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/domain"
)

const claudeTokenRef = "claude/oauth-token" //nolint:gosec // a ref, not a credential

func claudeWorker(e *env, name string) {
	e.t.Helper()
	e.joinWorker(name, withCapabilities(domain.HarnessModesMarker, domain.HarnessCapability("claude-code"),
		domain.HarnessModeCapability("claude-code", domain.ModeHeadless)))
}

// A Run of a harness that declares a required secret freezes it with its
// variable, as schema 3 (or 4), and placement reads the ref from the spec.
func TestHarnessRequiredSecrets_ARunFreezesTheRef(t *testing.T) {
	e := newEnv(t)
	e.project("backend", func(r *v1.CreateProjectRequest) { r.RepoConfig = domain.RepoConfigDeny })
	e.allow("backend", claudeTokenRef)

	created := e.mustTask("backend", "x", withHarness("claude-code", domain.ModeHeadless), usingSecret("GH_TOKEN", "gh/token"))
	run, err := e.app.Runs.Get(t.Context(), created.Run.ID)
	must(t, err)
	var spec struct {
		Schema  int             `json:"schema"`
		Secrets json.RawMessage `json:"secrets"`
	}
	must(t, json.Unmarshal(run.Spec, &spec))
	want := `{"env":[{"name":"CLAUDE_CODE_OAUTH_TOKEN","ref":"claude/oauth-token"},{"name":"GH_TOKEN","ref":"gh/token"}],"refs":["claude/oauth-token","gh/token"]}`
	if spec.Schema != domain.RunSpecSchemaSecrets || string(spec.Secrets) != want {
		t.Errorf("spec = %s", run.Spec)
	}
	placement, err := runs.PlacementOf(run.Spec)
	if err != nil || !equal(placement.Secrets, []string{claudeTokenRef, "gh/token"}) {
		t.Errorf("placement = %+v (%v)", placement, err)
	}

	// Naming the variable itself is no duplicate.
	again := e.mustTask("backend", "y", withHarness("claude-code", domain.ModeHeadless), usingSecret("CLAUDE_CODE_OAUTH_TOKEN", claudeTokenRef))
	if r, _ := e.app.Runs.Get(t.Context(), again.Run.ID); !strings.Contains(string(r.Spec), `"refs":["claude/oauth-token"]`) {
		t.Errorf("spec = %s", r.Spec)
	}
	// Another harness is untouched.
	plain := e.mustTask("backend", "z", withHarness("command", domain.ModeHeadless))
	if r, _ := e.app.Runs.Get(t.Context(), plain.Run.ID); strings.Contains(string(r.Spec), "secrets") {
		t.Errorf("spec = %s", r.Spec)
	}
}

func TestHarnessRequiredSecrets_AVariableOfAnotherRefConflicts(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	e.allow("backend", claudeTokenRef)
	_, err := e.createTask("backend", "x", withHarness("claude-code", domain.ModeHeadless), usingSecret("CLAUDE_CODE_OAUTH_TOKEN", "other"))
	wantCode(t, err, domain.CodeInvalidRequest)
	if n := e.count("tasks"); n != 0 {
		t.Errorf("%d Tasks from a refused request", n)
	}
}

// Only the Worker that holds the token is eligible.
func TestHarnessRequiredSecrets_AWorkerWithoutTheTokenIsNotEligible(t *testing.T) {
	e := newEnv(t)
	claudeWorker(e, "alpha") // it would win a tie
	claudeWorker(e, "beta")
	e.holds(e.sessionOf("beta"), 1, map[string][]string{claudeTokenRef: nil})
	e.project("backend")
	e.allow("backend", claudeTokenRef)
	run := e.queuedRun("x", withHarness("claude-code", domain.ModeHeadless))

	want := map[string]string{"alpha": `CF_SECRET_UNAVAILABLE {"missing":["claude/oauth-token"]}`, "beta": ""}
	if got := e.secretsVerdicts(run.ID); !maps.Equal(got, want) {
		t.Errorf("explain = %v, want %v", got, want)
	}
	if res := e.pass(); res.Assigned != 1 || e.workerOfRun(run.ID) != "beta" {
		t.Errorf("pass = %+v, Worker %q", res, e.workerOfRun(run.ID))
	}
}

// A Project that does not allow the token cannot create the Run, and the
// message says what to allow.
func TestHarnessRequiredSecrets_AProjectWithoutTheAllowlistRefusesTheRun(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	_, err := e.createTask("backend", "x", withHarness("claude-code", domain.ModeHeadless))
	wantCode(t, err, domain.CodePolicyDenied)
	if !strings.Contains(err.Error(), "cf project secrets allow <project> "+claudeTokenRef) {
		t.Errorf("error = %v", err)
	}
	if n := e.count("tasks"); n != 0 {
		t.Errorf("%d Tasks from a refused request", n)
	}
	e.allow("backend", claudeTokenRef)
	e.mustTask("backend", "x", withHarness("claude-code", domain.ModeHeadless))
}
