package executor

import (
	"encoding/json"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

func specWithSecret(t *testing.T, name, ref string) specView {
	t.Helper()
	var s specView
	raw := `{"schema":3,"secrets":{"env":[{"name":"` + name + `","ref":"` + ref + `"}],"refs":["` + ref + `"]}}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A harness that requires a secret the spec already froze names it once.
func TestLaunchSecretEnvTakesTheSameSecretOnce(t *testing.T) {
	spec := specWithSecret(t, "CLAUDE_CODE_OAUTH_TOKEN", "claude/oauth-token")
	env, extra, err := launchSecretEnv(spec, []harness.SecretEnv{{Name: "CLAUDE_CODE_OAUTH_TOKEN", Ref: "claude/oauth-token"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || len(extra) != 0 {
		t.Errorf("env = %v, extra = %v", env, extra)
	}
}

// The same variable from two refs is refused: one of them would be lost.
func TestLaunchSecretEnvRefusesOneNameForTwoRefs(t *testing.T) {
	spec := specWithSecret(t, "TOKEN", "a/one")
	if _, _, err := launchSecretEnv(spec, []harness.SecretEnv{{Name: "TOKEN", Ref: "a/two"}}); err == nil {
		t.Error("two refs for one variable were taken")
	}
}
