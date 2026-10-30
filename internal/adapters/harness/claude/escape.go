package claude

import (
	"fmt"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// SecretEscape says how a secret is written in settings.json and .claude.json,
// the files of a snapshot that carry markers: inside a JSON string.
func (Adapter) SecretEscape(path string) (harness.Escape, error) {
	if !carriesSecrets(path) {
		return "", fmt.Errorf("claude-code: %s carries no secrets", path)
	}
	return harness.EscapeJSONString, nil
}

var _ harness.SecretEscaper = Adapter{}
