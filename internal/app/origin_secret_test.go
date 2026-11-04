package app_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	originRef  = "origin/token"
	originRepo = "https://example.com/org/backend.git"
	sshRepo    = "git@example.com:org/backend.git"
)

func httpsProject(r *v1.CreateProjectRequest) { r.Repo = originRepo }

func (e *env) setOrigin(project, ref string) (v1.Project, error) {
	e.t.Helper()
	return e.app.Projects.Set(e.t.Context(), e.owner, project, v1.UpdateProjectRequest{OriginSecret: &ref})
}

// Every https Project starts with the origin secret set; a refused change
// leaves it as it was. The Projects of another URL form cannot hold one.
func TestOriginSecret_ValidationOfAProject(t *testing.T) {
	cases := []struct {
		name    string
		repo    string
		allowed bool // when the change is made
		ref     string
		want    domain.Code // "" for accepted
	}{
		{"allowed, https", originRepo, true, originRef, ""},
		{"clearing a set origin", originRepo, false, "", ""},
		{"ssh repo", sshRepo, true, originRef, domain.CodeInvalidRequest},
		{"http repo", "http://example.com/org/x.git", true, originRef, domain.CodeInvalidRequest},
		{"not a ref", originRepo, true, "Not A Ref!", domain.CodeInvalidRequest},
		{"not allowlisted", originRepo, false, "other/ref", domain.CodePolicyDenied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.project("backend", func(r *v1.CreateProjectRequest) { r.Repo = c.repo })
			before := ""
			if c.repo == originRepo {
				e.allow("backend", originRef)
				_, err := e.setOrigin("backend", originRef)
				must(t, err)
				before = originRef
				if !c.allowed {
					_, err := e.app.Projects.DenySecrets(t.Context(), e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{originRef}})
					must(t, err)
				}
			} else {
				e.allow("backend", originRef)
			}
			p, err := e.setOrigin("backend", c.ref)
			want := before
			if c.want == "" {
				must(t, err)
				want = c.ref
				if p.OriginSecret != want {
					t.Errorf("origin_secret = %q, want %q", p.OriginSecret, want)
				}
			} else {
				wantCode(t, err, c.want)
			}
			stored, err := e.app.Projects.Get(t.Context(), "backend")
			must(t, err)
			if stored.OriginSecret != want {
				t.Errorf("stored origin_secret = %q, want %q", stored.OriginSecret, want)
			}
		})
	}
}

func TestOriginSecret_ChangingAnotherFieldKeepsItAndClearingRemovesIt(t *testing.T) {
	e := newEnv(t)
	e.project("backend", httpsProject)
	e.allow("backend", originRef)
	_, err := e.setOrigin("backend", originRef)
	must(t, err)

	branch := "dev"
	p, err := e.app.Projects.Set(t.Context(), e.owner, "backend", v1.UpdateProjectRequest{DefaultBranch: &branch})
	must(t, err)
	if p.OriginSecret != originRef {
		t.Errorf("origin_secret = %q after an unrelated change", p.OriginSecret)
	}
	p, err = e.setOrigin("backend", "")
	must(t, err)
	if p.OriginSecret != "" {
		t.Errorf("origin_secret = %q after clearing", p.OriginSecret)
	}
}

type frozenOrigin struct {
	Secret string `json:"secret"`
	URL    string `json:"url"`
}

func (e *env) originOfRun(id string) *frozenOrigin {
	e.t.Helper()
	var spec struct {
		Origin *frozenOrigin `json:"origin"`
	}
	must(e.t, json.Unmarshal(e.storedRun(id).Spec, &spec))
	return spec.Origin
}

// A Run freezes the origin ref and URL: changing or clearing them in the
// Project later changes neither the spec nor its digest.
func TestOriginSecret_ARunFreezesTheOriginAndKeepsIt(t *testing.T) {
	e := newEnv(t)
	e.project("backend", httpsProject)
	e.allow("backend", originRef, "other/ref")
	_, err := e.setOrigin("backend", originRef)
	must(t, err)

	run := e.queuedRun("x")
	before := e.storedRun(run.ID)
	if got := e.originOfRun(run.ID); got == nil || *got != (frozenOrigin{originRef, originRepo}) {
		t.Fatalf("origin = %+v", got)
	}
	placement, err := runs.PlacementOf(before.Spec)
	must(t, err)
	if !equal(placement.Secrets, []string{originRef}) || !slices.Contains(placement.Capabilities, domain.OriginAskpassCapability) {
		t.Errorf("placement = %+v", placement)
	}

	_, err = e.setOrigin("backend", "other/ref")
	must(t, err)
	_, err = e.setOrigin("backend", "")
	must(t, err)
	after := e.storedRun(run.ID)
	if string(after.Spec) != string(before.Spec) || after.SpecDigest != before.SpecDigest {
		t.Errorf("the spec moved:\n%s\n%s", before.Spec, after.Spec)
	}
	if got := e.originOfRun(e.queuedRun("y").ID); got != nil {
		t.Errorf("a Run created after clearing froze %+v", got)
	}
}

// A Run without an origin secret keeps the spec it always had.
func TestOriginSecret_ARunWithoutItHasNoOriginInItsSpec(t *testing.T) {
	e := newEnv(t)
	run := e.queuedRun("x")
	if spec := string(e.storedRun(run.ID).Spec); strings.Contains(spec, `"origin":`) {
		t.Errorf("spec = %s", spec)
	}
	placement, err := runs.PlacementOf(e.storedRun(run.ID).Spec)
	must(t, err)
	if slices.Contains(placement.Capabilities, domain.OriginAskpassCapability) || len(placement.Secrets) != 0 {
		t.Errorf("placement = %+v", placement)
	}
}

// Only a Worker with the capability and the secret is eligible, and explain
// names what the others lack.
func TestOriginSecret_OnlyAWorkerWithTheCapabilityAndTheSecretIsEligible(t *testing.T) {
	e := newEnv(t)
	e.project("backend", httpsProject)
	e.allow("backend", originRef)
	_, err := e.setOrigin("backend", originRef)
	must(t, err)

	old := e.joinWorker("old") // holds the ref, lacks the capability
	e.joinWorker("bare", withCapabilities(domain.OriginAskpassCapability))
	ready := e.joinWorker("ready", withCapabilities(domain.OriginAskpassCapability))
	e.announces(old, map[string][]string{originRef: nil})
	e.announces(ready, map[string][]string{originRef: nil})
	run := e.queuedRun("x")

	got, err := e.app.Scheduler.Explain(t.Context(), run.ID)
	must(t, err)
	failed := map[string]string{}
	for _, w := range got.Workers {
		for _, f := range w.Failed {
			failed[w.Name] += f.Filter + ": " + f.Message + "; "
		}
	}
	if !strings.Contains(failed["old"], scheduler.FilterCapabilities+": missing capabilities: "+domain.OriginAskpassCapability) {
		t.Errorf("old: %q", failed["old"])
	}
	if !strings.Contains(failed["bare"], scheduler.FilterSecrets) {
		t.Errorf("bare: %q", failed["bare"])
	}
	if failed["ready"] != "" {
		t.Errorf("ready: %q", failed["ready"])
	}
	if res := e.pass(); res.Assigned != 1 || e.workerOfRun(run.ID) != "ready" {
		t.Errorf("pass = %+v, Worker %q", res, e.workerOfRun(run.ID))
	}
	a := e.commands(ready)[0].Assign
	if a.OriginSecret != originRef || a.OriginURL != originRepo || a.Repo != originRepo {
		t.Errorf("assign origin = %q %q (repo %q)", a.OriginSecret, a.OriginURL, a.Repo)
	}
}

// Removing the ref from the allowlist revokes: the grant is denied from then on.
func TestOriginSecret_TheGrantFollowsTheAllowlist(t *testing.T) {
	e := newEnv(t)
	e.project("backend", httpsProject)
	e.allow("backend", originRef)
	_, err := e.setOrigin("backend", originRef)
	must(t, err)
	w := e.joinWorker("alpha", withCapabilities(domain.OriginAskpassCapability))
	e.announces(w, map[string][]string{originRef: nil})
	run := e.queuedRun("x")
	l := e.offer(run.ID, w)
	_, err = e.accept(w, l)
	must(t, err)
	r := &reporter{e: e, w: w, run: run, lease: l, epoch: e.epochOf(w)}

	out, err := r.grant(r.grantRequest(originRef))
	must(t, err)
	if len(out.Granted) != 1 || out.Granted[0] != originRef {
		t.Fatalf("granted = %v", out.Granted)
	}
	// A ref the Run did not freeze stays undeclared.
	_, err = r.grant(r.grantRequest("other/ref"))
	if got := denials(t, err); got != "other/ref="+v1.SecretNotDeclared {
		t.Errorf("denials = %q", got)
	}
	_, err = e.app.Projects.DenySecrets(t.Context(), e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{originRef}})
	must(t, err)
	_, err = r.grant(r.grantRequest(originRef))
	if got := denials(t, err); got != originRef+"="+v1.SecretNotAllowlisted {
		t.Errorf("denials = %q", got)
	}
}

// The grant is per ref, so a ref that is also a harness secret would hand the
// harness the origin credential: the Run is refused, however the ref got there.
func TestOriginSecret_ARunRefusesTheOriginRefAsAHarnessSecret(t *testing.T) {
	e := newEnv(t)
	e.project("backend", httpsProject)
	e.allow("backend", originRef)
	_, err := e.setOrigin("backend", originRef)
	must(t, err)

	_, err = e.createTask("backend", "x", usingSecret("ORIGIN_TOKEN", originRef))
	wantCode(t, err, domain.CodePolicyDenied)
	if !strings.Contains(err.Error(), "origin_secret "+originRef+" cannot also be a harness secret") {
		t.Errorf("error = %v", err)
	}
	if n := e.count("tasks"); n != 0 {
		t.Errorf("%d Tasks from a refused request", n)
	}
	e.mustTask("backend", "y", usingSecret("OTHER_TOKEN", "other/ref"))
}
