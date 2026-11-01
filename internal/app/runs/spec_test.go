package runs

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

func digestOf(t *testing.T, task store.Task, project store.Project, commit string) (string, string) {
	t.Helper()
	in, origins := InheritWithOrigins(task, project)
	raw, digest, err := newSpec(in, origins, task.SourceSnapshot, Source{Ref: Inherit(task, project).SourceRef, Commit: commit}, nil).encode()
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), digest
}

// The layout of the spec and its digest are a contract: a Run keeps its
// spec_digest for ever, so this fixture only changes together with domain.RunSpecSchema.
func TestSpecDigest_Fixture(t *testing.T) {
	project := store.Project{DefaultBranch: "main"}
	raw, digest := digestOf(t, store.Task{Prompt: "fix the flaky test"}, project, commit)

	const wantJSON = `{"schema":2,"prompt":"fix the flaky test","source":{"ref":"main","commit":"0123456789abcdef0123456789abcdef01234567","snapshot":""},` +
		`"mode":"headless","workflow":"","environment":"",` +
		`"requirements":{"harness":"","model":"","capabilities":[],"resources":{"cpu":2,"memory_mb":4096},` +
		`"workers":{"prefer":[],"avoid":[],"require":[]}},` +
		`"completion":"first_success","retry":{"on_failed":"none","on_lost":"ask","max_attempts":1}}`
	const wantDigest = "sha256:630b344c4d436369aae73053dc8cb3f66d0e098a5996280c2bdb7543a6ad949c"
	if raw != wantJSON {
		t.Errorf("spec JSON changed:\n got %s\nwant %s", raw, wantJSON)
	}
	if digest != wantDigest {
		t.Errorf("digest = %s, want %s", digest, wantDigest)
	}
}

// What a Task leaves unset and what it spells out with the same value are the
// same configuration, so they hash the same.
func TestSpecDigest_EquivalentConfigurationsHashTheSame(t *testing.T) {
	project := store.Project{DefaultBranch: "main"}
	_, inherited := digestOf(t, store.Task{}, project, commit)

	explicit := store.Task{
		SourceRef: "main", Mode: domain.ModeHeadless, CompletionOverride: domain.CompletionFirstSuccess,
		Retry:        &v1.Retry{OnFailed: domain.RetryNone, OnLost: domain.RetryAsk, MaxAttempts: 1},
		Requirements: v1.Requirements{Resources: &v1.Resources{CPU: 2, MemoryMB: 4096}, Capabilities: []string{}},
	}
	_, spelledOut := digestOf(t, explicit, project, commit)

	viaProject := store.Project{DefaultBranch: "main", Defaults: v1.ProjectDefaults{
		Mode: domain.ModeHeadless, Completion: domain.CompletionFirstSuccess,
		Resources: &v1.Resources{CPU: 2, MemoryMB: 4096},
	}}
	_, fromProject := digestOf(t, store.Task{}, viaProject, commit)

	if inherited != spelledOut || inherited != fromProject {
		t.Errorf("inherited %s, spelled out %s, from the Project %s", inherited, spelledOut, fromProject)
	}
	// The ref is the effective one: the Task's own or the Project's branch.
	_, viaTask := digestOf(t, store.Task{SourceRef: "dev"}, project, commit)
	_, viaBranch := digestOf(t, store.Task{}, store.Project{DefaultBranch: "dev"}, commit)
	if viaTask != viaBranch {
		t.Error("the same effective ref hashed differently")
	}
}

func TestSpecDigest_SetsIgnoreOrderAndRepeats(t *testing.T) {
	project := store.Project{DefaultBranch: "main"}
	a := store.Task{Requirements: v1.Requirements{
		Capabilities: []string{"docker", "gpu", "docker"},
		Workers:      v1.WorkerPreferences{Avoid: []string{"b", "a"}, Require: []string{"x"}, Prefer: []string{"p", "q"}},
	}}
	b := store.Task{Requirements: v1.Requirements{
		Capabilities: []string{"gpu", "docker"},
		Workers:      v1.WorkerPreferences{Avoid: []string{"a", "b"}, Require: []string{"x"}, Prefer: []string{"p", "q"}},
	}}
	_, da := digestOf(t, a, project, commit)
	_, db := digestOf(t, b, project, commit)
	if da != db {
		t.Error("the order of a set changed the digest")
	}

	// Preferences are ordered: the first is preferred.
	b.Requirements.Workers.Prefer = []string{"q", "p"}
	if _, d := digestOf(t, b, project, commit); d == da {
		t.Error("the order of preferences must count")
	}
}

func TestSpecDigest_EveryFrozenFieldCounts(t *testing.T) {
	project := store.Project{DefaultBranch: "main"}
	_, base := digestOf(t, store.Task{}, project, commit)
	other := "fedcba9876543210fedcba9876543210fedcba98"

	variants := map[string]func() (store.Task, store.Project, string){
		"the prompt":   func() (store.Task, store.Project, string) { return store.Task{Prompt: "p"}, project, commit },
		"the commit":   func() (store.Task, store.Project, string) { return store.Task{}, project, other },
		"the ref":      func() (store.Task, store.Project, string) { return store.Task{SourceRef: "dev"}, project, commit },
		"the snapshot": func() (store.Task, store.Project, string) { return store.Task{SourceSnapshot: "s"}, project, commit },
		"the mode": func() (store.Task, store.Project, string) {
			return store.Task{Mode: domain.ModeInteractive}, project, commit
		},
		"the harness": func() (store.Task, store.Project, string) {
			return store.Task{Requirements: v1.Requirements{Harness: "codex"}}, project, commit
		},
		"the model": func() (store.Task, store.Project, string) {
			return store.Task{Requirements: v1.Requirements{Model: "m"}}, project, commit
		},
		"the workflow":    func() (store.Task, store.Project, string) { return store.Task{Workflow: "w"}, project, commit },
		"the environment": func() (store.Task, store.Project, string) { return store.Task{Environment: "e"}, project, commit },
		"the completion": func() (store.Task, store.Project, string) {
			return store.Task{CompletionOverride: domain.CompletionManual}, project, commit
		},
		"the retry": func() (store.Task, store.Project, string) {
			return store.Task{Retry: &v1.Retry{OnFailed: domain.RetryAsk, OnLost: domain.RetryAsk, MaxAttempts: 1}}, project, commit
		},
		"the secrets": func() (store.Task, store.Project, string) {
			return store.Task{Requirements: v1.Requirements{SecretEnv: []v1.SecretEnv{{Name: "TOKEN", Ref: "gh/token"}}}}, project, commit
		},
		"the resources of the Project": func() (store.Task, store.Project, string) {
			return store.Task{}, store.Project{DefaultBranch: "main", Defaults: v1.ProjectDefaults{Resources: &v1.Resources{CPU: 4, MemoryMB: 4096}}}, commit
		},
	}
	seen := map[string]string{}
	for name, variant := range variants {
		task, p, c := variant()
		_, d := digestOf(t, task, p, c)
		if d == base {
			t.Errorf("%s does not change the digest", name)
		}
		if prev, dup := seen[d]; dup {
			t.Errorf("%s and %s collide", name, prev)
		}
		seen[d] = name
	}
}

func TestInherit_PrefersTheFrozenCompletion(t *testing.T) {
	project := store.Project{Defaults: v1.ProjectDefaults{Completion: domain.CompletionPromote}}
	for _, c := range []struct {
		name string
		task store.Task
		want domain.Completion
	}{
		{"the global default", store.Task{}, domain.CompletionFirstSuccess},
		{"the Project's", store.Task{}, domain.CompletionPromote},
		{"the Task's own", store.Task{CompletionOverride: domain.CompletionManual}, domain.CompletionManual},
		{"the frozen one", store.Task{CompletionOverride: domain.CompletionManual, Completion: domain.CompletionFirstSuccess}, domain.CompletionFirstSuccess},
	} {
		p := project
		if c.name == "the global default" {
			p = store.Project{}
		}
		if got := Inherit(c.task, p).Completion; got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// The scheduler reads the placement from the stored spec: what the Run froze is what it reserves.
func TestPlacementOf_ReadsWhatTheSpecFroze(t *testing.T) {
	task := store.Task{Prompt: "x", Requirements: v1.Requirements{
		Harness: "claude", Capabilities: []string{"gpu", "docker"},
		Resources: &v1.Resources{CPU: 6, MemoryMB: 8192},
		Workers:   v1.WorkerPreferences{Prefer: []string{"b", "a"}, Avoid: []string{"c"}, Require: []string{"a", "b"}},
	}}
	raw, _ := digestOf(t, task, store.Project{DefaultBranch: "main"}, commit)

	got, err := PlacementOf([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Placement{
		Harness: "claude", Mode: domain.ModeHeadless, Capabilities: []string{"docker", "gpu"}, Resources: domain.Capacity{CPU: 6, MemoryMB: 8192},
		Prefer: []string{"b", "a"}, Avoid: []string{"c"}, Require: []string{"a", "b"},
	}
	if jsonOf(got) != jsonOf(want) {
		t.Errorf("placement = %+v, want %+v", got, want)
	}
	if _, err := PlacementOf([]byte(`{`)); err == nil {
		t.Error("a corrupt spec was read")
	}
	if p, err := PlacementOf([]byte(`{}`)); err != nil || p.Resources != (domain.Capacity{}) {
		t.Errorf("an empty spec = %+v (%v)", p, err)
	}
}

func withSecrets(env ...v1.SecretEnv) store.Task {
	return store.Task{Prompt: "fix the flaky test", Requirements: v1.Requirements{SecretEnv: env}}
}

// A Run that uses no secret is frozen as before secrets, byte for byte, even
// when its Task spells out an empty list: Runs and Workers of before keep
// their digests and read their specs.
func TestSpecDigest_NoSecretsIsSchema2ByteForByte(t *testing.T) {
	project := store.Project{DefaultBranch: "main"}
	without, d1 := digestOf(t, store.Task{Prompt: "fix the flaky test"}, project, commit)
	empty, d2 := digestOf(t, withSecrets(), project, commit)
	if without != empty || d1 != d2 {
		t.Errorf("an empty secret_env changed the spec:\n%s\n%s", without, empty)
	}
	if strings.Contains(without, "secrets") || !strings.HasPrefix(without, `{"schema":2,`) {
		t.Errorf("spec = %s", without)
	}
}

// The layout of a spec with secrets is a contract too: env sorted by name and
// the set of refs, at the end, under schema 3.
func TestSpecDigest_SecretsFixture(t *testing.T) {
	project := store.Project{DefaultBranch: "main"}
	raw, digest := digestOf(t, withSecrets(
		v1.SecretEnv{Name: "NPM_TOKEN", Ref: "npm/token"},
		v1.SecretEnv{Name: "GH_TOKEN", Ref: "gh/token"},
		v1.SecretEnv{Name: "GITHUB_TOKEN", Ref: "gh/token"},
	), project, commit)

	const wantJSON = `{"schema":3,"prompt":"fix the flaky test","source":{"ref":"main","commit":"0123456789abcdef0123456789abcdef01234567","snapshot":""},` +
		`"mode":"headless","workflow":"","environment":"",` +
		`"requirements":{"harness":"","model":"","capabilities":[],"resources":{"cpu":2,"memory_mb":4096},` +
		`"workers":{"prefer":[],"avoid":[],"require":[]}},` +
		`"completion":"first_success","retry":{"on_failed":"none","on_lost":"ask","max_attempts":1},` +
		`"secrets":{"env":[{"name":"GH_TOKEN","ref":"gh/token"},{"name":"GITHUB_TOKEN","ref":"gh/token"},{"name":"NPM_TOKEN","ref":"npm/token"}],` +
		`"refs":["gh/token","npm/token"]}}`
	const wantDigest = "sha256:57ce42a7796093f726a425cfceacd42eaddeed71de7e39929926d60ce8645e30"
	if raw != wantJSON {
		t.Errorf("spec JSON changed:\n got %s\nwant %s", raw, wantJSON)
	}
	if digest != wantDigest {
		t.Errorf("digest = %s, want %s", digest, wantDigest)
	}
}

// The order the variables are given in is no configuration; a name or a ref is.
func TestSpecDigest_SecretsAreStable(t *testing.T) {
	project := store.Project{DefaultBranch: "main"}
	a, b := v1.SecretEnv{Name: "A", Ref: "x"}, v1.SecretEnv{Name: "B", Ref: "y"}
	_, ab := digestOf(t, withSecrets(a, b), project, commit)
	_, ba := digestOf(t, withSecrets(b, a), project, commit)
	if ab != ba {
		t.Error("the order of secret_env changed the digest")
	}
	_, renamed := digestOf(t, withSecrets(a, v1.SecretEnv{Name: "C", Ref: "y"}), project, commit)
	_, reref := digestOf(t, withSecrets(a, v1.SecretEnv{Name: "B", Ref: "z"}), project, commit)
	if renamed == ab || reref == ab || renamed == reref {
		t.Error("a name or a ref does not change the digest")
	}
}

func TestPlacementOf_ReadsTheSecrets(t *testing.T) {
	project := store.Project{DefaultBranch: "main"}
	raw, _ := digestOf(t, withSecrets(v1.SecretEnv{Name: "B", Ref: "y"}, v1.SecretEnv{Name: "A", Ref: "x"}, v1.SecretEnv{Name: "C", Ref: "x"}), project, commit)
	got, err := PlacementOf([]byte(raw))
	if err != nil || jsonOf(got.Secrets) != `["x","y"]` {
		t.Errorf("secrets = %v (%v)", got.Secrets, err)
	}
	raw, _ = digestOf(t, store.Task{}, project, commit)
	if got, err := PlacementOf([]byte(raw)); err != nil || got.Secrets != nil {
		t.Errorf("a Run without secrets places %v (%v)", got.Secrets, err)
	}
}

func jsonOf(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// A Run without a Workflow asks for no capability of its own: the placement
// of its spec is what its requirements say, and with one it adds exactly
// domain.WorkflowConfigCapability.
func TestPlacementOf_AWorkflowAddsItsCapability(t *testing.T) {
	project := store.Project{DefaultBranch: "main"}
	plain, _ := digestOf(t, store.Task{Prompt: "x", Requirements: v1.Requirements{Capabilities: []string{"docker"}}}, project, commit)
	flow, _ := digestOf(t, store.Task{Prompt: "x", Workflow: "review@sha256:" + strings.Repeat("a", 64), Requirements: v1.Requirements{Capabilities: []string{"docker"}}}, project, commit)

	p, err := PlacementOf([]byte(plain))
	if err != nil || len(p.Capabilities) != 1 {
		t.Fatalf("plain placement = %+v, %v", p.Capabilities, err)
	}
	p, err = PlacementOf([]byte(flow))
	if err != nil || len(p.Capabilities) != 2 || !slices.Contains(p.Capabilities, domain.WorkflowConfigCapability) {
		t.Fatalf("workflow placement = %+v, %v", p.Capabilities, err)
	}
	named, _ := digestOf(t, store.Task{Prompt: "x", Workflow: "review@sha256:" + strings.Repeat("a", 64), Requirements: v1.Requirements{Harness: "claude-code"}}, project, commit)
	if p, err = PlacementOf([]byte(named)); err != nil || slices.Contains(p.Capabilities, domain.HarnessConfigIsolationCapability("claude-code")) {
		t.Fatalf("a Workflow Run must not require config isolation (the config filter of the scheduler decides): %+v, %v", p.Capabilities, err)
	}
}

const environmentJSON = `{"provider":"host","requires_capabilities":["docker"],"prepare":"make deps","services":[],"digest_inputs":[]}`

func specWith(t *testing.T, repoConfig domain.RepoConfig, environment []byte) (string, string) {
	t.Helper()
	project := store.Project{DefaultBranch: "main", RepoConfig: repoConfig}
	in, origins := InheritWithOrigins(store.Task{Prompt: "x"}, project)
	raw, digest, err := newSpec(in, origins, "", Source{Ref: "main", Commit: commit}, environment).encode()
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), digest
}

// A Project that allows the repo's configuration freezes schema 4 with the
// choice spelled out; one that denies it and has no Environment keeps schema 2.
func TestSpec_Schema4FreezesTheRepoConfigChoice(t *testing.T) {
	allow, _ := specWith(t, domain.RepoConfigAllow, nil)
	if !strings.HasPrefix(allow, `{"schema":4,`) || !strings.HasSuffix(allow, `"repo_config":"allow","origins":{"resources":"global"}}`) {
		t.Errorf("allow spec = %s", allow)
	}
	deny, _ := specWith(t, domain.RepoConfigDeny, nil)
	if !strings.HasPrefix(deny, `{"schema":2,`) || strings.Contains(deny, "repo_config") {
		t.Errorf("deny spec = %s", deny)
	}
	_, a := specWith(t, domain.RepoConfigAllow, nil)
	_, b := specWith(t, domain.RepoConfigDeny, nil)
	if a == b {
		t.Error("the repo_config choice did not change the digest")
	}
}

// An Environment makes the Run schema 4 even if the Project denies the repo's
// configuration, and then the denial is frozen.
func TestSpec_AnEnvironmentIsSchema4AndFreezesItsDefinition(t *testing.T) {
	raw, _ := specWith(t, domain.RepoConfigDeny, []byte(environmentJSON))
	if !strings.HasPrefix(raw, `{"schema":4,`) || !strings.HasSuffix(raw, `"repo_config":"deny","environment_def":`+environmentJSON+`,"origins":{"resources":"global"}}`) {
		t.Errorf("spec = %s", raw)
	}
	// Secrets and schema 4 combine: the layout is 4 and the secrets stay in place.
	in, origins := InheritWithOrigins(store.Task{Requirements: v1.Requirements{SecretEnv: []v1.SecretEnv{{Name: "T", Ref: "a/b"}}}}, store.Project{RepoConfig: domain.RepoConfigAllow})
	both, _, err := newSpec(in, origins, "", Source{Ref: "main", Commit: commit}, nil).encode()
	if err != nil || !strings.HasPrefix(string(both), `{"schema":4,`) || !strings.Contains(string(both), `"secrets":{"env":[{"name":"T","ref":"a/b"}],"refs":["a/b"]},"repo_config":"allow"`) {
		t.Errorf("spec = %s (%v)", both, err)
	}
}

func TestPlacementOf_Schema4AndEnvironmentAddTheirCapabilities(t *testing.T) {
	for name, c := range map[string]struct {
		repoConfig domain.RepoConfig
		env        []byte
		want       []string
	}{
		"schema 2":             {domain.RepoConfigDeny, nil, nil},
		"schema 4":             {domain.RepoConfigAllow, nil, []string{domain.RepoConfigCapability}},
		"an environment":       {domain.RepoConfigDeny, []byte(environmentJSON), []string{"docker", domain.EnvironmentHostCapability, domain.RepoConfigCapability}},
		"environment and repo": {domain.RepoConfigAllow, []byte(environmentJSON), []string{"docker", domain.EnvironmentHostCapability, domain.RepoConfigCapability}},
	} {
		raw, _ := specWith(t, c.repoConfig, c.env)
		p, err := PlacementOf([]byte(raw))
		if err != nil || !slices.Equal(p.Capabilities, c.want) && len(p.Capabilities)+len(c.want) > 0 {
			t.Errorf("%s: capabilities = %v (%v), want %v", name, p.Capabilities, err, c.want)
		}
	}
}

// The origin of each inherited field is the layer that gave it, and Inherit
// resolves the very same values.
func TestInheritWithOrigins_NamesTheLayerOfEachField(t *testing.T) {
	task := store.Task{Mode: domain.ModeInteractive, Environment: "e"}
	project := store.Project{DefaultBranch: "main", DefaultWorkflow: "w", Defaults: v1.ProjectDefaults{Resources: &v1.Resources{CPU: 4, MemoryMB: 8192}}}
	in, o := InheritWithOrigins(task, project)
	want := Origins{
		SourceRef: domain.OriginProject, Mode: domain.OriginTask, Workflow: domain.OriginProject, Environment: domain.OriginTask,
		Resources: domain.OriginProject, Completion: domain.OriginGlobal, Retry: domain.OriginGlobal,
	}
	if o != want {
		t.Errorf("origins = %+v, want %+v", o, want)
	}
	if plain := Inherit(task, project); jsonOf(plain) != jsonOf(in) {
		t.Errorf("Inherit = %s, with origins %s", jsonOf(plain), jsonOf(in))
	}
	if _, o := InheritWithOrigins(store.Task{}, store.Project{}); o.Resources != domain.OriginGlobal || o.Mode != domain.OriginGlobal {
		t.Errorf("empty origins = %+v", o)
	}
}

// The layer that gave the resources and the Environment is frozen in schema 4,
// so the Worker records the Controller's provenance, not its own.
func TestSpec_Schema4FreezesTheOriginsOfResourcesAndEnvironment(t *testing.T) {
	res := func(cpu int) *v1.Resources { return &v1.Resources{CPU: cpu, MemoryMB: 1024} }
	for name, c := range map[string]struct {
		task    store.Task
		project store.Project
		want    string
	}{
		"global":  {store.Task{}, store.Project{}, `"origins":{"resources":"global"}`},
		"project": {store.Task{}, store.Project{DefaultEnvironment: "e", Defaults: v1.ProjectDefaults{Resources: res(2)}}, `"origins":{"resources":"project","environment":"project"}`},
		"task":    {store.Task{Environment: "e", Requirements: v1.Requirements{Resources: res(1)}}, store.Project{DefaultEnvironment: "p"}, `"origins":{"resources":"task","environment":"task"}`},
	} {
		t.Run(name, func(t *testing.T) {
			c.project.RepoConfig = domain.RepoConfigAllow
			in, origins := InheritWithOrigins(c.task, c.project)
			raw, _, err := newSpec(in, origins, "", Source{Ref: "main", Commit: commit}, []byte(environmentJSON)).encode()
			if err != nil || !strings.HasSuffix(string(raw), c.want+"}") {
				t.Errorf("spec = %s (%v), want suffix %s", raw, err, c.want)
			}
		})
	}
	plain, _ := specWith(t, domain.RepoConfigDeny, nil)
	if strings.Contains(plain, "origins") {
		t.Errorf("a schema 2 spec carries origins: %s", plain)
	}
}
