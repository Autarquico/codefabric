package projects_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/environments"
	"github.com/Autarquico/codefabric/internal/app/projects"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

type fixture struct {
	t        *testing.T
	owner    pki.Session
	projects *projects.Service
	envs     *environments.Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "controller.db"), store.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ca, err := pki.NewCA(clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	ids := domain.NewIDGenerator(clk.Now, rand.Reader)
	authority := pki.NewAuthority(pki.AuthorityConfig{DB: db, CA: ca, Clock: clk, IDs: ids, CertTTL: domain.Defaults().IdentityCertTTL})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := authority.BootstrapOwner(t.Context(), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), "owner")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := joined.Session()
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, owner: owner, projects: projects.New(db, clk, ids), envs: environments.New(db, clk)}
}

func (f *fixture) project(req v1.CreateProjectRequest) {
	f.t.Helper()
	req.Name, req.Repo = "backend", "https://example.com/acme/backend.git"
	if _, err := f.projects.Create(f.t.Context(), f.owner, req); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) explain(file string, blobs map[string]string) v1.ProjectExplanation {
	f.t.Helper()
	req := v1.ExplainProjectRequest{Commit: commit, BlobIDs: blobs}
	if file != "" {
		req.FilePresent, req.File = true, []byte(file)
	}
	out, err := f.projects.Explain(f.t.Context(), "backend", req)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func field(t *testing.T, e v1.ProjectExplanation, name string) v1.ExplainedField {
	t.Helper()
	for _, f := range e.Fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field %q in %+v", name, e.Fields)
	return v1.ExplainedField{}
}

func TestExplain_OriginsGlobalAndProject(t *testing.T) {
	f := newFixture(t)
	f.project(v1.CreateProjectRequest{DefaultBranch: "trunk", Defaults: v1.ProjectDefaults{Resources: &v1.Resources{CPU: 2, MemoryMB: 2048}}})
	e := f.explain("", nil)

	if got := field(t, e, "resources.cpu"); got.Value != 2 || got.Origin != domain.OriginProject {
		t.Errorf("cpu = %+v, want 2 from project", got)
	}
	if got := field(t, e, "source_ref"); got.Value != "trunk" || got.Origin != domain.OriginProject {
		t.Errorf("source_ref = %+v, want trunk from project", got)
	}
	if got := field(t, e, "mode"); got.Origin != domain.OriginGlobal {
		t.Errorf("mode origin = %q, want global", got.Origin)
	}
	if e.FilePresent || e.Invalid != nil || e.EffectiveDigest == "" {
		t.Errorf("absent file: %+v", e)
	}
}

func TestExplain_RepoLowersAndIsClampedAboveTheCeiling(t *testing.T) {
	f := newFixture(t)
	f.project(v1.CreateProjectRequest{Defaults: v1.ProjectDefaults{Resources: &v1.Resources{CPU: 2, MemoryMB: 2048}}})
	e := f.explain("defaults:\n  resources:\n    cpu: 1\n    memory_mb: 999999\n", nil)

	if got := field(t, e, "resources.cpu"); got.Value != 1 || got.Origin != domain.OriginRepo {
		t.Errorf("cpu = %+v, want 1 from repo", got)
	}
	if got := field(t, e, "resources.memory_mb"); got.Value != 2048 || got.Origin != domain.OriginClamped {
		t.Errorf("memory = %+v, want 2048 clamped", got)
	}
}

func TestExplain_DenyIgnoresTheFileEvenIfInvalid(t *testing.T) {
	f := newFixture(t)
	f.project(v1.CreateProjectRequest{RepoConfig: domain.RepoConfigDeny})
	e := f.explain("not: [valid", nil)

	if !e.FileIgnored || e.Invalid != nil {
		t.Fatalf("deny: ignored=%v invalid=%v, want ignored and no error", e.FileIgnored, e.Invalid)
	}
	if got := field(t, e, "resources.cpu"); got.Origin == domain.OriginRepo {
		t.Errorf("cpu came from the repo under deny: %+v", got)
	}
}

// The Worker fails the Run on an effective configuration over 16 KiB, so
// explain says the same of the same file (the parser takes up to 64 KiB).
func TestExplain_AnEffectiveConfigOverTheLimitIsInvalid(t *testing.T) {
	f := newFixture(t)
	f.project(v1.CreateProjectRequest{})
	e := f.explain("evaluation:\n  description: "+strings.Repeat("x", 20<<10)+"\n", nil)

	if e.Invalid == nil || e.Invalid.Code != domain.CodeRepoConfigInvalid || len(e.Fields) != 0 {
		t.Fatalf("invalid = %+v, fields %d, want CF_REPO_CONFIG_INVALID and no fields", e.Invalid, len(e.Fields))
	}
}

func TestExplain_InvalidKeyIsReportedWithItsCode(t *testing.T) {
	f := newFixture(t)
	f.project(v1.CreateProjectRequest{})
	e := f.explain("defaults:\n  resources:\n    gpu: 1\n", nil)

	if e.Invalid == nil || e.Invalid.Code != domain.CodeRepoConfigInvalid || e.Invalid.Details["key"] != "defaults.resources.gpu" {
		t.Fatalf("invalid = %+v, want CF_REPO_CONFIG_INVALID on defaults.resources.gpu", e.Invalid)
	}
	if len(e.Fields) != 0 {
		t.Errorf("an invalid file shows no fields, got %d", len(e.Fields))
	}
}

func TestExplain_EnvironmentOfTheProjectAndDigestInputs(t *testing.T) {
	f := newFixture(t)
	if _, err := f.envs.Apply(t.Context(), f.owner, environments.ApplyRequest{Name: "node", Definition: domain.EnvironmentDef{Provider: "host", Prepare: "npm ci"}}); err != nil {
		t.Fatal(err)
	}
	f.project(v1.CreateProjectRequest{DefaultEnvironment: "node"})
	file := "environment:\n  prepare: pnpm i\n  digest_inputs: [package.json]\n"

	first := f.explain(file, nil)
	if got := field(t, first, "environment.prepare"); got.Value != "pnpm i" || got.Origin != domain.OriginRepo {
		t.Errorf("prepare = %+v, want pnpm i from repo", got)
	}
	if got := field(t, first, "environment.provider"); got.Origin != domain.OriginProject {
		t.Errorf("provider origin = %q, want project", got.Origin)
	}
	if len(first.NeedBlobs) != 1 || first.NeedBlobs[0] != "package.json" || first.EffectiveDigest != "" {
		t.Fatalf("first answer = need %v digest %q, want the blob of package.json asked", first.NeedBlobs, first.EffectiveDigest)
	}

	a := f.explain(file, map[string]string{"package.json": "aaaa"})
	b := f.explain(file, map[string]string{"package.json": "bbbb"})
	if a.EffectiveDigest == "" || a.EffectiveDigest == b.EffectiveDigest {
		t.Errorf("digests %q and %q: a different input blob must give a different digest", a.EffectiveDigest, b.EffectiveDigest)
	}
}

func TestExplain_RefusesAShortCommit(t *testing.T) {
	f := newFixture(t)
	f.project(v1.CreateProjectRequest{})
	if _, err := f.projects.Explain(t.Context(), "backend", v1.ExplainProjectRequest{Commit: "main"}); err == nil {
		t.Error("a branch name was accepted as the commit")
	}
}
