package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Inherited is a Task's configuration with every field it omits resolved
// Task → Project → global defaults (§3.3). Resources is always set.
type Inherited struct {
	SourceRef    string
	Mode         domain.Mode
	Workflow     string
	Environment  string
	Requirements v1.Requirements
	Completion   domain.Completion
	Retry        v1.Retry
}

// Inherit resolves what a Task leaves unset. The Completion is the one in
// force: frozen with the first Run, else the Task's own, else the Project's.
func Inherit(t store.Task, p store.Project) Inherited {
	global := domain.Defaults()
	in := Inherited{
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
	if r := firstPtr(t.Retry, p.Defaults.Retry); r != nil {
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

// specSchema versions the layout of runSpec. A change to the layout must raise
// it, so that digests of different layouts never compare equal by accident.
const specSchema = 1

// runSpec is what a Run freezes when it is created (§3.3). Its JSON, with the
// fields in this order, is hashed into spec_digest: it holds only logical
// configuration, never IDs, timestamps or operational state, and it owns its
// types so that a change to the API's does not move the digests.
type runSpec struct {
	Schema       int               `json:"schema"`
	Source       specSource        `json:"source"`
	Mode         domain.Mode       `json:"mode"`
	Workflow     string            `json:"workflow"`
	Environment  string            `json:"environment"`
	Requirements specRequirements  `json:"requirements"`
	Completion   domain.Completion `json:"completion"`
	Retry        specRetry         `json:"retry"`
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

type specRetry struct {
	OnFailed    domain.RetryAction `json:"on_failed"`
	OnLost      domain.RetryAction `json:"on_lost"`
	MaxAttempts int                `json:"max_attempts"`
}

func newSpec(in Inherited, snapshot, commit string) runSpec {
	r := in.Requirements
	return runSpec{
		Schema:      specSchema,
		Source:      specSource{Ref: in.SourceRef, Commit: commit, Snapshot: snapshot},
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
	}
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
