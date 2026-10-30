package repoconfig_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/repoconfig"
)

func mustParse(t *testing.T, yaml string) repoconfig.File {
	t.Helper()
	f, err := repoconfig.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func invalidKey(t *testing.T, yaml string) string {
	t.Helper()
	_, err := repoconfig.Parse([]byte(yaml))
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeRepoConfigInvalid {
		t.Fatalf("want CF_REPO_CONFIG_INVALID, got %v", err)
	}
	key, _ := de.Details["key"].(string)
	return key
}

func TestParse_RejectsKeysARepoMayNotSet(t *testing.T) {
	cases := map[string]struct{ yaml, key string }{
		"top level":          {"secrets: {a: b}\n", "secrets"},
		"defaults other":     {"defaults:\n  mode: headless\n", "defaults.mode"},
		"resources other":    {"defaults:\n  resources:\n    gpu: 1\n", "defaults.resources.gpu"},
		"provider":           {"environment:\n  provider: host\n", "environment.provider"},
		"capabilities":       {"environment:\n  requires_capabilities: [x]\n", "environment.requires_capabilities"},
		"service field":      {"environment:\n  services:\n    - {name: db, start: s, user: root}\n", "environment.services.user"},
		"merge key":          {"environment:\n  <<: {prepare: x}\n", "environment.<<"},
		"repo_config itself": {"repo_config: allow\n", "repo_config"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := invalidKey(t, c.yaml); got != c.key {
				t.Fatalf("key = %q, want %q", got, c.key)
			}
		})
	}
}

func TestParse_RejectsHostileDocuments(t *testing.T) {
	cases := map[string]string{
		"anchor":           "evaluation:\n  a: &x [1]\n",
		"alias":            "evaluation:\n  a: &x [1]\n  b: *x\n",
		"two documents":    "evaluation: {a: 1}\n---\nevaluation: {b: 2}\n",
		"duplicate key":    "environment: {prepare: a}\nenvironment: {prepare: b}\n",
		"too large":        "evaluation: {a: " + strings.Repeat("x", repoconfig.MaxFileBytes) + "}\n",
		"not a mapping":    "- a\n- b\n",
		"broken yaml":      "evaluation: [\n",
		"zero cpu":         "defaults: {resources: {cpu: 0}}\n",
		"negative mem":     "defaults: {resources: {memory_mb: -1}}\n",
		"cpu not number":   "defaults: {resources: {cpu: many}}\n",
		"bad digest path":  "environment:\n  digest_inputs: ['../etc/passwd']\n",
		"service no start": "environment:\n  services:\n    - {name: db}\n",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) { invalidKey(t, y) })
	}
}

func TestParse_ErrorKeepsTheOffendingKeyBounded(t *testing.T) {
	long := strings.Repeat("k", 5000)
	_, err := repoconfig.Parse([]byte(long + ": 1\n"))
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("want domain error, got %v", err)
	}
	if len(de.Message) > 400 {
		t.Fatalf("message is %d bytes, not bounded", len(de.Message))
	}
}

func TestParse_EmptyFileSaysNothing(t *testing.T) {
	for _, y := range []string{"", "# only a comment\n", "---\n"} {
		f := mustParse(t, y)
		if f.Environment != nil || f.Resources != nil || f.Evaluation != nil {
			t.Fatalf("%q: want empty file, got %+v", y, f)
		}
	}
}

func TestParse_AcceptsWhatARepoMaySet(t *testing.T) {
	f := mustParse(t, `
evaluation:
  test: {command: make test}
defaults:
  resources: {cpu: 2, memory_mb: 1024}
environment:
  prepare: make deps
  digest_inputs: [go.sum, package-lock.json]
  services:
    - {name: db, start: ./db.sh, stop: ./db-stop.sh, ready: ./db-ready.sh}
`)
	if *f.Resources.CPU != 2 || *f.Resources.MemoryMB != 1024 {
		t.Fatalf("resources = %+v", f.Resources)
	}
	if *f.Environment.Prepare != "make deps" || len(*f.Environment.Services) != 1 || len(*f.Environment.DigestInputs) != 2 {
		t.Fatalf("environment = %+v", f.Environment)
	}
	if f.Evaluation["test"] == nil {
		t.Fatalf("evaluation not recorded: %v", f.Evaluation)
	}
}

func controllerInputs() repoconfig.Inputs {
	return repoconfig.Inputs{
		RepoConfig:   domain.RepoConfigAllow,
		Resources:    domain.Resources{CPU: 4, MemoryMB: 8192},
		CPUOrigin:    domain.OriginProject,
		MemoryOrigin: domain.OriginGlobal,
	}
}

func TestResolve_WithoutFileKeepsControllerValuesAndOrigins(t *testing.T) {
	eff, err := repoconfig.Resolve(controllerInputs())
	if err != nil {
		t.Fatal(err)
	}
	if eff.Resources.CPU != 4 || eff.Resources.MemoryMB != 8192 {
		t.Fatalf("resources = %+v", eff.Resources)
	}
	if eff.Origins[repoconfig.FieldCPU] != domain.OriginProject || eff.Origins[repoconfig.FieldMemoryMB] != domain.OriginGlobal {
		t.Fatalf("origins = %v", eff.Origins)
	}
}

func TestResolve_RepoMayLowerResourcesButNotRaiseThem(t *testing.T) {
	in := controllerInputs()
	f := mustParse(t, "defaults: {resources: {cpu: 16, memory_mb: 2048}}\n")
	in.File = &f
	eff, err := repoconfig.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Resources.CPU != 4 || eff.Origins[repoconfig.FieldCPU] != domain.OriginClamped {
		t.Fatalf("cpu = %d (%s), want 4 clamped", eff.Resources.CPU, eff.Origins[repoconfig.FieldCPU])
	}
	if eff.Resources.MemoryMB != 2048 || eff.Origins[repoconfig.FieldMemoryMB] != domain.OriginRepo {
		t.Fatalf("memory = %d (%s), want 2048 repo", eff.Resources.MemoryMB, eff.Origins[repoconfig.FieldMemoryMB])
	}
}

func controllerEnvironment() *domain.EnvironmentDef {
	return &domain.EnvironmentDef{
		Provider:             domain.ProviderHost,
		RequiresCapabilities: []string{"zeta:v1", "alpha:v1"},
		Prepare:              "make setup",
		DigestInputs:         []string{"go.sum"},
	}
}

func TestResolve_RepoOverridesEnvironmentFieldsButKeepsCapabilitiesExactly(t *testing.T) {
	in := controllerInputs()
	in.Environment, in.EnvironmentOrigin = controllerEnvironment(), domain.OriginProject
	f := mustParse(t, "environment:\n  prepare: make deps\n")
	in.File = &f
	eff, err := repoconfig.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	env := eff.Environment
	if env.Prepare != "make deps" || eff.Origins[repoconfig.FieldPrepare] != domain.OriginRepo {
		t.Fatalf("prepare = %q (%s)", env.Prepare, eff.Origins[repoconfig.FieldPrepare])
	}
	if got := strings.Join(env.RequiresCapabilities, ","); got != "zeta:v1,alpha:v1" {
		t.Fatalf("capabilities = %s, want the Controller's, in its order", got)
	}
	if eff.Origins[repoconfig.FieldRequiresCapabilities] != domain.OriginProject || eff.Origins[repoconfig.FieldDigestInputs] != domain.OriginProject {
		t.Fatalf("origins = %v", eff.Origins)
	}
	if in.Environment.Prepare != "make setup" {
		t.Fatal("Resolve changed the Environment it was given")
	}
}

func TestResolve_RepoDefinesAHostEnvironmentWhenControllerHasNone(t *testing.T) {
	in := controllerInputs()
	f := mustParse(t, "environment:\n  prepare: make deps\n")
	in.File = &f
	eff, err := repoconfig.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Environment == nil || eff.Environment.Provider != domain.ProviderHost || len(eff.Environment.RequiresCapabilities) != 0 {
		t.Fatalf("environment = %+v", eff.Environment)
	}
	if eff.Origins[repoconfig.FieldPrepare] != domain.OriginRepo || eff.Origins[repoconfig.FieldProvider] != domain.OriginRepo {
		t.Fatalf("origins = %v", eff.Origins)
	}
}

func TestResolve_DenyIgnoresTheFileAndSaysSo(t *testing.T) {
	in := controllerInputs()
	in.RepoConfig = domain.RepoConfigDeny
	f := mustParse(t, "defaults: {resources: {cpu: 1}}\nenvironment: {prepare: x}\n")
	in.File = &f
	eff, err := repoconfig.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if !eff.FileIgnored || eff.Resources.CPU != 4 || eff.Environment != nil {
		t.Fatalf("effective = %+v", eff)
	}
}

func TestResolve_RejectsAnOriginTheControllerCannotHave(t *testing.T) {
	in := controllerInputs()
	in.CPUOrigin = domain.OriginRepo
	if _, err := repoconfig.Resolve(in); err == nil {
		t.Fatal("want an error for a repo origin on a Controller value")
	}
}

func digestOf(t *testing.T, yaml string, blobs map[string]string) string {
	t.Helper()
	in := controllerInputs()
	in.Environment, in.EnvironmentOrigin = controllerEnvironment(), domain.OriginGlobal
	if yaml != "" {
		f := mustParse(t, yaml)
		in.File = &f
	}
	eff, err := repoconfig.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	d, err := repoconfig.EffectiveDigest(eff, blobs)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestEffectiveDigest_ChangesWithAnInputBlobAndNotOtherwise(t *testing.T) {
	base := digestOf(t, "", map[string]string{"go.sum": "aaa"})
	if !strings.HasPrefix(base, "sha256:") {
		t.Fatalf("digest = %q", base)
	}
	if got := digestOf(t, "", map[string]string{"go.sum": "aaa"}); got != base {
		t.Fatal("same inputs, different digest")
	}
	if got := digestOf(t, "", map[string]string{"go.sum": "aaa", "README.md": "zzz"}); got != base {
		t.Fatal("a blob that is not a digest input changed the digest")
	}
	if got := digestOf(t, "", map[string]string{"go.sum": "bbb"}); got == base {
		t.Fatal("a digest input changed and the digest did not")
	}
	if got := digestOf(t, "", nil); got == base {
		t.Fatal("an absent input must differ from a present one")
	}
}

func TestEffectiveDigest_IgnoresProvenanceAndOrderOfSets(t *testing.T) {
	a := controllerInputs()
	a.Environment, a.EnvironmentOrigin = controllerEnvironment(), domain.OriginGlobal
	b := a
	b.CPUOrigin = domain.OriginTask
	env := *controllerEnvironment()
	env.RequiresCapabilities = []string{"alpha:v1", "zeta:v1"}
	b.Environment = &env
	da := mustDigest(t, a)
	if db := mustDigest(t, b); da != db {
		t.Fatal("origin or capability order changed the digest")
	}
}

func TestEffectiveDigest_ChangesWhenTheRepoChangesTheEnvironment(t *testing.T) {
	blobs := map[string]string{"go.sum": "aaa"}
	if digestOf(t, "environment: {prepare: make deps}\n", blobs) == digestOf(t, "", blobs) {
		t.Fatal("a repo override did not change the digest")
	}
}

func mustDigest(t *testing.T, in repoconfig.Inputs) string {
	t.Helper()
	eff, err := repoconfig.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	d, err := repoconfig.EffectiveDigest(eff, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDigestInputPaths_AreTheEffectiveOnesSorted(t *testing.T) {
	in := controllerInputs()
	in.Environment, in.EnvironmentOrigin = controllerEnvironment(), domain.OriginGlobal
	f := mustParse(t, "environment: {digest_inputs: [z.lock, a.lock]}\n")
	in.File = &f
	eff, _ := repoconfig.Resolve(in)
	if got := strings.Join(eff.DigestInputPaths(), ","); got != "a.lock,z.lock" {
		t.Fatalf("paths = %s", got)
	}
}
