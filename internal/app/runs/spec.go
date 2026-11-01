package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Inherited is a Task's configuration with every field it omits resolved
// Task → Project → global defaults (§3.3). Resources is always set.
// RepoConfig is the Project's say on reading the repo's own configuration;
// the store always gives one, and "" is read as not allowing it.
type Inherited struct {
	Prompt       string
	SourceRef    string
	Mode         domain.Mode
	Workflow     string
	Environment  string
	Requirements v1.Requirements
	Completion   domain.Completion
	Retry        v1.Retry
	RepoConfig   domain.RepoConfig
}

// Origins says, for each field Inherit resolves, the layer that gave it. The
// resources come as one value, so one origin says it for the CPU and the memory.
type Origins struct {
	SourceRef, Mode, Workflow, Environment, Resources, Completion, Retry domain.Origin
}

// Inherit resolves what a Task leaves unset. The Completion and the Retry
// policy are the ones in force: frozen with the first Run, else the Task's own,
// else the Project's.
func Inherit(t store.Task, p store.Project) Inherited {
	in, _ := InheritWithOrigins(t, p)
	return in
}

// InheritWithOrigins is Inherit with the layer (task, project or global) that
// gave each field, for whoever explains a configuration.
func InheritWithOrigins(t store.Task, p store.Project) (Inherited, Origins) {
	global := domain.Defaults()
	var o Origins
	in := Inherited{Prompt: t.Prompt, Requirements: t.Requirements, RepoConfig: p.RepoConfig}
	in.SourceRef, o.SourceRef = pick(layer(t.SourceRef, domain.OriginTask), layer(p.DefaultBranch, domain.OriginProject))
	in.Mode, o.Mode = pick(layer(t.Mode, domain.OriginTask), layer(p.Defaults.Mode, domain.OriginProject), layer(global.TaskMode, domain.OriginGlobal))
	in.Workflow, o.Workflow = pick(layer(t.Workflow, domain.OriginTask), layer(p.DefaultWorkflow, domain.OriginProject))
	in.Environment, o.Environment = pick(layer(t.Environment, domain.OriginTask), layer(p.DefaultEnvironment, domain.OriginProject))
	in.Completion, o.Completion = pick(layer(t.Completion, domain.OriginTask), layer(t.CompletionOverride, domain.OriginTask),
		layer(p.Defaults.Completion, domain.OriginProject), layer(global.TaskCompletion, domain.OriginGlobal))
	in.Retry = v1.Retry{OnFailed: global.TaskRetry.OnFailed, OnLost: global.TaskRetry.OnLost, MaxAttempts: global.TaskRetry.MaxAttempts}
	o.Retry = domain.OriginGlobal
	if r, origin := pickPtr(layer(t.Retry, domain.OriginTask), layer(t.RetryOverride, domain.OriginTask), layer(p.Defaults.Retry, domain.OriginProject)); r != nil {
		in.Retry, o.Retry = *r, origin
	}
	res, origin := pickPtr(layer(t.Requirements.Resources, domain.OriginTask), layer(p.Defaults.Resources, domain.OriginProject),
		layer(&v1.Resources{CPU: global.RunResources.CPU, MemoryMB: global.RunResources.MemoryMB}, domain.OriginGlobal))
	in.Requirements.Resources, o.Resources = res, origin
	return in, o
}

// candidate is a value one layer offers for a field, and which layer it is.
type candidate[T any] struct {
	value  T
	origin domain.Origin
}

func layer[T any](value T, origin domain.Origin) candidate[T] { return candidate[T]{value, origin} }

// pick is the first candidate that is not the zero value, with its origin.
func pick[T comparable](candidates ...candidate[T]) (T, domain.Origin) {
	var zero T
	for _, c := range candidates {
		if c.value != zero {
			return c.value, c.origin
		}
	}
	return zero, ""
}

// pickPtr is the first candidate that is set, with its origin.
func pickPtr[T any](candidates ...candidate[*T]) (*T, domain.Origin) {
	for _, c := range candidates {
		if c.value != nil {
			return c.value, c.origin
		}
	}
	return nil, ""
}

func first[T comparable](values ...T) T {
	var zero T
	for _, v := range values {
		if v != zero {
			return v
		}
	}
	return zero
}

// runSpec is what a Run freezes when it is created (§3.3). Its JSON, with the
// fields in this order, is hashed into spec_digest: it holds only logical
// configuration and inputs, never IDs, timestamps or operational state, and it
// owns its types so that a change to the API's does not move the digests. The
// prompt is an input: Runs that differ in it are not reproductions (§12.3).
// Schema versions the layout (domain.RunSpecSchemaFor): a change to it must
// raise the schema, so that digests of different layouts never compare equal
// by accident. Secrets is absent from a Run that uses none, whose spec stays
// the schema 2 one byte for byte.
type runSpec struct {
	Schema       int               `json:"schema"`
	Prompt       string            `json:"prompt"`
	Source       specSource        `json:"source"`
	Mode         domain.Mode       `json:"mode"`
	Workflow     string            `json:"workflow"`
	Environment  string            `json:"environment"`
	Requirements specRequirements  `json:"requirements"`
	Completion   domain.Completion `json:"completion"`
	Retry        specRetry         `json:"retry"`
	Secrets      *specSecrets      `json:"secrets,omitempty"`
	// RepoConfig and EnvironmentDef are schema 4 only. RepoConfig freezes
	// whether the Worker reads the repo's configuration (allow|deny);
	// EnvironmentDef is the canonical definition of the pinned Environment, so
	// that the Worker needs nothing but the spec to know what to prepare.
	RepoConfig     domain.RepoConfig `json:"repo_config,omitempty"`
	EnvironmentDef json.RawMessage   `json:"environment_def,omitempty"`
	// Origins is schema 4 only: the layer that gave the resources and the
	// Environment, which the Worker cannot know and records in the Run's
	// effective configuration.
	Origins *specOrigins `json:"origins,omitempty"`
}

type specOrigins struct {
	Resources   domain.Origin `json:"resources"`
	Environment domain.Origin `json:"environment,omitempty"`
}

type specSource struct {
	Ref      string `json:"ref"`
	Commit   string `json:"commit"`
	Snapshot string `json:"snapshot"`
}

type specRequirements struct {
	Harness      string        `json:"harness"`
	Model        string        `json:"model"`
	Capabilities []string      `json:"capabilities"`
	Resources    specResources `json:"resources"`
	Workers      specWorkers   `json:"workers"`
}

type specResources struct {
	CPU      int `json:"cpu"`
	MemoryMB int `json:"memory_mb"`
}

type specWorkers struct {
	Prefer  []string `json:"prefer"` // in order of preference
	Avoid   []string `json:"avoid"`
	Require []string `json:"require"`
}

// specSecrets is what a Run may resolve on its Worker: the environment
// variables that carry a secret, sorted by name, and Refs, the set of refs the
// Run uses, which the Worker must hold and the grant checks against.
type specSecrets struct {
	Env  []specSecretEnv `json:"env"`
	Refs []string        `json:"refs"`
}

type specSecretEnv struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

type specRetry struct {
	OnFailed    domain.RetryAction `json:"on_failed"`
	OnLost      domain.RetryAction `json:"on_lost"`
	MaxAttempts int                `json:"max_attempts"`
}

// newSpec freezes the configuration. environmentDef is the canonical definition
// of the Environment the Run pins, nil for a Run without one. A Run with an
// Environment, or whose Project allows the repo's configuration, is schema 4;
// any other keeps the layout of schemas 2 and 3 byte for byte. origins are
// frozen only in schema 4.
func newSpec(in Inherited, origins Origins, snapshot string, source Source, environmentDef []byte) runSpec {
	r := in.Requirements
	secrets := newSecrets(r.SecretEnv)
	repoConfig := in.RepoConfig == domain.RepoConfigAllow || environmentDef != nil
	spec := runSpec{
		Schema:      domain.RunSpecSchemaWith(secrets != nil, repoConfig),
		Prompt:      in.Prompt,
		Source:      specSource{Ref: source.Ref, Commit: source.Commit, Snapshot: snapshot},
		Mode:        in.Mode,
		Workflow:    in.Workflow,
		Environment: in.Environment,
		Requirements: specRequirements{
			Harness: r.Harness, Model: r.Model,
			Capabilities: set(r.Capabilities),
			Resources:    specResources{CPU: r.Resources.CPU, MemoryMB: r.Resources.MemoryMB},
			Workers: specWorkers{
				Prefer: slices.Clone(orEmpty(r.Workers.Prefer)), Avoid: set(r.Workers.Avoid), Require: set(r.Workers.Require),
			},
		},
		Completion: in.Completion,
		Retry:      specRetry{OnFailed: in.Retry.OnFailed, OnLost: in.Retry.OnLost, MaxAttempts: in.Retry.MaxAttempts},
		Secrets:    secrets,
	}
	if repoConfig {
		spec.RepoConfig = first(in.RepoConfig, domain.Defaults().ProjectRepoConfig)
		spec.EnvironmentDef = environmentDef
		spec.Origins = &specOrigins{Resources: origins.Resources}
		if environmentDef != nil {
			spec.Origins.Environment = origins.Environment
		}
	}
	return spec
}

// newSecrets is nil for a Run that uses no secret.
func newSecrets(env []v1.SecretEnv) *specSecrets {
	if len(env) == 0 {
		return nil
	}
	out := &specSecrets{Env: make([]specSecretEnv, 0, len(env))}
	refs := make([]domain.SecretEnv, 0, len(env))
	for _, e := range env {
		out.Env = append(out.Env, specSecretEnv(e))
		refs = append(refs, domain.SecretEnv(e))
	}
	slices.SortFunc(out.Env, func(a, b specSecretEnv) int { return strings.Compare(a.Name, b.Name) })
	out.Refs = domain.SecretRefsOf(refs)
	return out
}

// set is a sorted copy without repeats: the order of a set does not make a
// different configuration. It is never nil, so it always encodes as [].
func set(values []string) []string {
	out := slices.Clone(orEmpty(values))
	slices.Sort(out)
	return slices.Compact(out)
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// encode returns the JSON that is stored and the digest that identifies it.
func (s runSpec) encode() (raw []byte, digest string, err error) {
	raw, err = json.Marshal(s)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// PlacementOf reads the placement out of a Run's stored spec: its requirements
// (plus domain.WorkflowConfigCapability when it froze a Workflow,
// domain.RepoConfigCapability for schema 4, and domain.EnvironmentHostCapability
// with those the Environment requires when it froze one),
// the mode the harness must run in and the secrets the Worker must hold. A spec
// without requirements asks for nothing.
func PlacementOf(spec []byte) (domain.Placement, error) {
	var stored struct {
		Schema         int                    `json:"schema"`
		Mode           domain.Mode            `json:"mode"`
		Workflow       string                 `json:"workflow"`
		Requirements   specRequirements       `json:"requirements"`
		Secrets        *specSecrets           `json:"secrets"`
		EnvironmentDef *domain.EnvironmentDef `json:"environment_def"`
	}
	if err := json.Unmarshal(spec, &stored); err != nil {
		return domain.Placement{}, fmt.Errorf("runs: reading the placement of a spec: %w", err)
	}
	r := stored.Requirements
	capabilities := r.Capabilities
	if stored.Workflow != "" {
		// A harness without config isolation gets the Workflow as its global
		// configuration: the scheduler's config filter checks that (§8.12).
		capabilities = append(slices.Clone(capabilities), domain.WorkflowConfigCapability)
	}
	if stored.Schema >= domain.RunSpecSchemaRepoConfig {
		capabilities = append(slices.Clone(capabilities), domain.RepoConfigCapability)
	}
	if def := stored.EnvironmentDef; def != nil {
		capabilities = append(slices.Clone(capabilities), domain.EnvironmentHostCapability)
		capabilities = append(capabilities, def.RequiresCapabilities...)
	}
	if len(capabilities) > len(r.Capabilities) {
		capabilities = set(capabilities)
	}
	placement := domain.Placement{
		Harness: r.Harness, Mode: stored.Mode, Capabilities: capabilities,
		Resources: domain.Capacity{CPU: r.Resources.CPU, MemoryMB: r.Resources.MemoryMB},
		Prefer:    r.Workers.Prefer, Avoid: r.Workers.Avoid, Require: r.Workers.Require,
	}
	if stored.Secrets != nil {
		placement.Secrets = stored.Secrets.Refs
	}
	return placement, nil
}

// withRequiredSecrets adds to the Run's secrets those its harness always needs
// (domain.HarnessRequiredSecrets), so that they are frozen, authorised and
// placed like any other. A variable the Task already binds to the same ref is
// kept once; one bound to another ref is a conflict.
func withRequiredSecrets(in Inherited) (Inherited, error) {
	required := domain.HarnessRequiredSecrets(in.Requirements.Harness)
	if len(required) == 0 {
		return in, nil
	}
	env := slices.Clone(in.Requirements.SecretEnv)
	for _, r := range required {
		i := slices.IndexFunc(env, func(e v1.SecretEnv) bool { return e.Name == r.Name })
		switch {
		case i < 0:
			env = append(env, v1.SecretEnv(r))
		case env[i].Ref != r.Ref:
			return in, input.Invalid("requirements.secret_env: %s is the variable of %s for harness %s", r.Name, r.Ref, in.Requirements.Harness)
		}
	}
	vars := make([]domain.SecretEnv, 0, len(env))
	for _, e := range env {
		vars = append(vars, domain.SecretEnv(e))
	}
	if err := domain.CheckSecretEnv(vars); err != nil {
		return in, input.Invalid("requirements.secret_env: %s", strings.TrimPrefix(err.Error(), domain.ErrInvalidSecret.Error()+": "))
	}
	in.Requirements.SecretEnv = env
	return in, nil
}

// requireAllowed fails when the Project's allowlist lacks a ref the harness
// always needs: such a Run could never be placed, so it is not created.
func requireAllowed(harness string, allowlist []string) error {
	var missing []string
	for _, r := range domain.HarnessRequiredSecrets(harness) {
		if !slices.Contains(allowlist, r.Ref) {
			missing = append(missing, r.Ref)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return domain.NewError(domain.CodePolicyDenied,
		fmt.Sprintf("harness %s needs the secrets %s and the Project does not allow them: run `cf project secrets allow <project> %s`",
			harness, strings.Join(missing, ", "), strings.Join(missing, " ")),
		map[string]any{"not_allowed": missing})
}
