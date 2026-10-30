package executor

import (
	"encoding/json"

	"github.com/Autarquico/codefabric/internal/domain"
)

// specView is the part of the frozen spec the Worker acts on. It reads the
// schemas domain.ReadableRunSpecSchema allows: 2; 3, which adds Secrets; and 4,
// which adds RepoConfig and the EnvironmentDef of the Controller.
type specView struct {
	Schema int    `json:"schema"`
	Prompt string `json:"prompt"`
	Mode   string `json:"mode"`
	Source struct {
		Ref string `json:"ref"`
	} `json:"source"`
	Requirements struct {
		Harness   string `json:"harness"`
		Model     string `json:"model"`
		Resources struct {
			CPU      int `json:"cpu"`
			MemoryMB int `json:"memory_mb"`
		} `json:"resources"`
	} `json:"requirements"`
	Secrets *specSecrets `json:"secrets"`
	// RepoConfig is the policy of the Project frozen with the Run (schema 4);
	// EnvironmentDef the Environment it pinned, nil if it has none.
	RepoConfig     domain.RepoConfig      `json:"repo_config"`
	EnvironmentDef *domain.EnvironmentDef `json:"environment_def"`
	// Origins is the layer that gave the resources and the Environment (schema 4).
	Origins struct {
		Resources   domain.Origin `json:"resources"`
		Environment domain.Origin `json:"environment"`
	} `json:"origins"`
}

// specSecrets is what a Run may resolve on its Worker: the environment of its
// harness that carries a secret, and Refs, every ref the Run uses.
type specSecrets struct {
	Env []struct {
		Name string `json:"name"`
		Ref  string `json:"ref"`
	} `json:"env"`
	Refs []string `json:"refs"`
}

// unknownSpec is the spec of a record rebuilt from an order that did not
// carry it (a stop of a Controller before #23): what the Run declared is not
// known, secrets included.
const unknownSpec = `{}`

// known says whether the spec is one the Worker can tell the Run by: one it
// cannot read, or none, has schema 0.
func (s specView) known() bool { return s.Schema != 0 }

// refs are the refs the Run declared; none for one without secrets.
func (s specView) refs() []string {
	if s.Secrets == nil {
		return nil
	}
	return s.Secrets.Refs
}

// env is the secret environment the Run declared.
func (s specView) env() []domain.SecretEnv {
	if s.Secrets == nil {
		return nil
	}
	env := make([]domain.SecretEnv, len(s.Secrets.Env))
	for i, e := range s.Secrets.Env {
		env[i] = domain.SecretEnv{Name: e.Name, Ref: e.Ref}
	}
	return env
}

// parseSpec reads the spec of an assignment. A spec that cannot be read has
// schema 0, which is refused as an incompatible version.
func parseSpec(raw []byte) specView {
	var s specView
	if err := json.Unmarshal(raw, &s); err != nil {
		return specView{}
	}
	return s
}
