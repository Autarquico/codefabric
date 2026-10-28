package codex_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/worker/host"
)

// awkwardValues are secrets that every TOML string form writes differently.
var awkwardValues = map[string]string{
	"single":  "it's",
	"double":  `say "hi"`,
	"back":    `C:\dir\n`,
	"newline": "a\nb",
	"tab":     "a\tb",
	"unicode": "ñandú-日本-🔑",
	"triple":  "x'''y",
	"mixed":   "'\"\\\n\t'''\"\"\"",
}

// TestRender_EveryValueRoundTripsThroughConfigToml walks the whole path of a
// secret: SnapshotConfig of a home with secrets in config.toml, Materialize,
// the real render, and the parse of the config.toml the Run gets.
func TestRender_EveryValueRoundTripsThroughConfigToml(t *testing.T) {
	var cfg strings.Builder
	cfg.WriteString("[mcp_servers.docs]\ncommand = \"docs\"\n\n[mcp_servers.docs.env]\n")
	for name := range awkwardValues {
		cfg.WriteString("V_" + strings.ToUpper(name) + " = \"plain-" + name + "\"\n")
	}
	cfg.WriteString("\n[model_providers.corp]\nname = \"corp\"\nexperimental_bearer_token = \"tok\"\n")
	h := homeWith(t, map[string]string{"config.toml": cfg.String()})
	snap := snapshot(t, h, codex.ConfigComponent)

	runDir := t.TempDir()
	m := host.NewMaterializer(h)
	mat, err := m.Materialize(t.Context(), codex.Adapter{}, runDir, snap)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	refs := file(t, snap, codex.ConfigComponent).SecretRefs
	if len(refs) != len(awkwardValues)+1 {
		t.Fatalf("refs = %v, want %d", refs, len(awkwardValues)+1)
	}
	values := map[string][]byte{}
	want := map[string]string{}
	for name, v := range awkwardValues {
		ref := "codex/mcp/docs/env/V_" + strings.ToUpper(name)
		values[ref] = []byte(v)
		want[ref] = v
	}
	values["codex/model_providers/corp/experimental_bearer_token"] = []byte(`tok'"\`)
	fence := func(_ []string, write func() error) error { return write() }
	if err := m.Render(runDir, mat, values, fence); err != nil {
		t.Fatalf("Render: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(host.ConfigDir(runDir), "config.toml")) //nolint:gosec // temporary dir
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCP       map[string]struct{ Env map[string]string } `toml:"mcp_servers"`
		Providers map[string]struct {
			Token string `toml:"experimental_bearer_token"`
		} `toml:"model_providers"`
	}
	if err := toml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the rendered config.toml is not TOML: %v", err)
	}
	for name, v := range awkwardValues {
		if g := got.MCP["docs"].Env["V_"+strings.ToUpper(name)]; g != v {
			t.Errorf("%s: got %q, want %q", name, g, v)
		}
	}
	if g := got.Providers["corp"].Token; g != `tok'"\` {
		t.Errorf("bearer token: got %q", g)
	}
}

func TestApply_RejectsAMarkerOutsideABasicString(t *testing.T) {
	for name, content := range map[string]string{
		"literal":   "[env]\nA = '{{cf-secret:codex/a}}'\n",
		"multiline": "[env]\nA = \"\"\"{{cf-secret:codex/a}}\"\"\"\n",
		"key":       "[env]\n\"{{cf-secret:codex/a}}\" = \"x\"\n",
		"comment":   "# {{cf-secret:codex/a}}\n[env]\nA = \"x\"\n",
		"array":     "[env]\nA = ['{{cf-secret:codex/a}}']\n",
	} {
		t.Run(name, func(t *testing.T) {
			snap := harness.NativeSnapshot{Harness: codex.ID, SchemaVersion: codex.SchemaVersion, Components: []harness.Component{{
				Name: codex.ConfigComponent, Kind: harness.KindFile, Ownership: harness.Managed,
				Files: []harness.File{{Path: "config.toml", Content: []byte(content), SecretRefs: harness.SecretRefs([]byte(content))}},
			}}}
			h := homeWith(t, nil)
			_, err := host.NewMaterializer(h).Materialize(t.Context(), codex.Adapter{}, t.TempDir(), snap)
			if err == nil || !strings.Contains(err.Error(), harness.ErrSecretUnavailable.Error()) {
				t.Fatalf("err = %v, want ErrSecretUnavailable", err)
			}
		})
	}
}
