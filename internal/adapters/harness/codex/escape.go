package codex

import (
	"fmt"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// SecretEscape says how a secret is written in config.toml, the only file of a
// snapshot that carries markers: inside a TOML basic string.
func (Adapter) SecretEscape(path string) (harness.Escape, error) {
	if path != ConfigComponent {
		return "", fmt.Errorf("codex: %s carries no secrets", path)
	}
	return harness.EscapeTOMLBasicString, nil
}

var _ harness.SecretEscaper = Adapter{}
