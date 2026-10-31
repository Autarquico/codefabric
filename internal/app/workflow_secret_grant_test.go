package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

// secretComponent is a component of one file whose content carries the marker of ref.
func (e *wfEnv) secretComponent(harness, name, ref string) domain.ManifestComponent {
	e.t.Helper()
	content := "token = \"{{cf-secret:" + ref + "}}\"\n"
	tree, err := treedigest.Files([]treedigest.Entry{treedigest.File{Path: name + ".toml", Content: []byte(content)}})
	must(e.t, err)
	return domain.ManifestComponent{
		Harness: harness, Name: name, Kind: "file", Ownership: domain.OwnershipManaged, TreeDigest: tree,
		Files: []domain.ManifestFile{{Path: name + ".toml", SHA256: e.upload(content), SecretRefs: []string{ref}}},
	}
}

// A secret only the configuration of the frozen Workflow uses is authorised
// like one the spec declares: by the allowlist and the scope of the Worker.
func TestSecretGrant_AuthorisesTheSecretsOfTheFrozenWorkflow(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	e.project("backend")
	e.mustCommit("review", 0, manifest(e.secretComponent("fake", "script", "team/token"), e.secretComponent("other", "script", "other/token")))
	w := e.joinWorker("alpha", withCapabilities(domain.HarnessModesMarker, domain.HarnessCapability("fake"),
		domain.HarnessModeCapability("fake", domain.ModeHeadless), domain.WorkflowConfigCapability,
		domain.HarnessConfigIsolationCapability("fake")))
	run := e.queuedRun("x", withHarness("fake", domain.ModeHeadless), withWorkflow("review@latest"))
	e.allow("backend", "team/token", "other/token")
	e.announces(w, map[string][]string{"team/token": nil, "other/token": nil})
	l := e.offer(run.ID, w)
	_, err := e.accept(w, l)
	must(t, err)
	r := &reporter{e: e.env, w: w, run: run, lease: l, epoch: e.epochOf(w)}

	out, err := r.grant(r.grantRequest("team/token"))
	must(t, err)
	if len(out.Granted) != 1 || out.Granted[0] != "team/token" {
		t.Fatalf("granted = %v", out.Granted)
	}
	// Only the components of the harness of the Run count, and the allowlist still decides.
	_, err = r.grant(r.grantRequest("other/token"))
	if got := denials(t, err); got != "other/token="+v1.SecretNotDeclared {
		t.Errorf("denials = %q", got)
	}
	_, err = e.app.Projects.DenySecrets(t.Context(), e.owner, "backend", v1.ProjectSecretsRequest{Refs: []string{"team/token"}})
	must(t, err)
	_, err = r.grant(r.grantRequest("team/token"))
	if got := denials(t, err); got != "team/token="+v1.SecretNotAllowlisted {
		t.Errorf("denials = %q", got)
	}
}
