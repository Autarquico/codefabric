package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Inherited is a Task's configuration with every field it omits resolved
// Task → Project → global defaults (§3.3). Resources is always set.
type Inherited struct {
	Prompt       string
	SourceRef    string
	Mode         domain.Mode
	Workflow     string
	Environment  string
	Requirements v1.Requirements
	Completion   domain.Completion
	Retry        v1.Retry
}

// Inherit resolves what a Task leaves unset. The Completion and the Retry
// policy are the ones in force: frozen with the first Run, else the Task's own,
// else the Project's.
func Inherit(t store.Task, p store.Project) Inherited {
	global := domain.Defaults()
	in := Inherited{
		Prompt:       t.Prompt,
		SourceRef:    first(t.SourceRef, p.DefaultBranch),
		Mode:         first(t.Mode, p.Defaults.Mode, global.TaskMode),
		Workflow:     first(t.Workflow, p.DefaultWorkflow),
		Environment:  first(t.Environment, p.DefaultEnvironment),
		Requirements: t.Requirements,
		Completion:   first(t.Completion, t.CompletionOverride, p.Defaults.Completion, global.TaskCompletion),
		Retry:        v1.Retry{OnFailed: global.TaskRetry.OnFailed, OnLost: global.TaskRetry.OnLost, MaxAttempts: global.TaskRetry.MaxAttempts},
	}
	in.Requirements.Resources = firstPtr(t.Requirements.Resources, p.Defaults.Resources,
		&v1.Resources{CPU: global.RunResources.CPU, MemoryMB: global.RunResources.MemoryMB})
	if r := firstPtr(t.Retry, t.RetryOverride, p.Defaults.Retry); r != nil {
		in.Retry = *r
	}
	return in
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

func firstPtr[T any](values ...*T) *T {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
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

func newSpec(in Inherited, snapshot string, source Source) runSpec {
	r := in.Requirements
	secrets := newSecrets(r.SecretEnv)
	return runSpec{
		Schema:      domain.RunSpecSchemaFor(secrets != nil),
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
// (plus domain.WorkflowConfigCapability when it froze a Workflow),
// the mode the harness must run in and the secrets the Worker must hold. A spec
// without requirements asks for nothing.
func PlacementOf(spec []byte) (domain.Placement, error) {
	var stored struct {
		Mode         domain.Mode      `json:"mode"`
		Workflow     string           `json:"workflow"`
		Requirements specRequirements `json:"requirements"`
		Secrets      *specSecrets     `json:"secrets"`
	}
	if err := json.Unmarshal(spec, &stored); err != nil {
		return domain.Placement{}, fmt.Errorf("runs: reading the placement of a spec: %w", err)
	}
	r := stored.Requirements
	capabilities := r.Capabilities
	if stored.Workflow != "" {
		capabilities = append(slices.Clone(capabilities), domain.WorkflowConfigCapability)
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
