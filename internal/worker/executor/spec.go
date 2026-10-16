package executor

import (
	"encoding/json"

	"github.com/Autarquico/codefabric/internal/domain"
)

// specView is the part of the frozen spec the Worker acts on. It reads the
// schemas domain.ReadableRunSpecSchema allows: 2, and 3, which adds Secrets.
type specView struct {
	Schema int    `json:"schema"`
	Prompt string `json:"prompt"`
	Mode   string `json:"mode"`
	Source struct {
		Ref string `json:"ref"`
	} `json:"source"`
	Requirements struct {
		Harness string `json:"harness"`
		Model   string `json:"model"`
	} `json:"requirements"`
	Secrets *specSecrets `json:"secrets"`
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
