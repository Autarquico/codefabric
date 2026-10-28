package codex_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/host"
)

// TestRender_ConfigTomlInAnEventIsRedactedInEveryForm walks a secret from
// SnapshotConfig through the real Render, then carries the text of the
// rendered config.toml as the string of a JSON event, as a harness that reads
// the file would, and redacts the line as the wrapper does: neither the value
// nor any form of it may remain.
func TestRender_ConfigTomlInAnEventIsRedactedInEveryForm(t *testing.T) {
	values := map[string]string{
		"quote":     `say "hi"-quote-1`,
		"backslash": `C:\dir\n-back-2`,
		"newline":   "first-line-of-secret\nsecond-line-of-secret-3",
	}
	var cfg strings.Builder
	cfg.WriteString("[mcp_servers.docs]\ncommand = \"docs\"\n\n[mcp_servers.docs.env]\n")
	for name := range values {
		cfg.WriteString("V_" + strings.ToUpper(name) + " = \"plain-" + name + "\"\n")
	}
	h := homeWith(t, map[string]string{"config.toml": cfg.String()})
	snap := snapshot(t, h, codex.ConfigComponent)

	runDir := t.TempDir()
	m := host.NewMaterializer(h)
	mat, err := m.Materialize(t.Context(), codex.Adapter{}, runDir, snap)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	byRef := map[string][]byte{}
	var needles []redact.Needle
	for name, v := range values {
		ref := "codex/mcp/docs/env/V_" + strings.ToUpper(name)
		byRef[ref] = []byte(v)
		needles = append(needles, redact.Needle{Ref: ref, Value: []byte(v)})
	}
	fence := func(_ []string, write func() error) error { return write() }
	if err := m.Render(runDir, mat, byRef, fence); err != nil {
		t.Fatalf("Render: %v", err)
	}
	rendered, err := os.ReadFile(filepath.Join(host.ConfigDir(runDir), "config.toml")) //nolint:gosec // temporary dir
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]string{"type": "item.completed", "text": string(rendered)})
	if err != nil {
		t.Fatal(err)
	}

	set, err := redact.NewEscapedSet(needles)
	if err != nil {
		t.Fatal(err)
	}
	got := string(redact.Bytes(line, set))
	if !json.Valid([]byte(got)) {
		t.Errorf("the redacted event is not JSON: %s", got)
	}
	for name, v := range values {
		toml, _ := redact.TOMLBasicString([]byte(v))
		twice := jsonInner(t, string(toml))
		if !strings.Contains(string(line), twice) {
			t.Fatalf("%s: the event does not carry the doubly escaped value %q: %s", name, twice, line)
		}
		for _, form := range []string{v, string(toml), jsonInner(t, v), twice} {
			if strings.Contains(got, form) {
				t.Errorf("%s: the event still holds %q: %s", name, form, got)
			}
		}
	}
}

// jsonInner is s as the inside of a JSON string.
func jsonInner(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Trim(string(b), `"`)
}
