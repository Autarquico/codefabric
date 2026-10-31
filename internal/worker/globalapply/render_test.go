package globalapply

import (
	"context"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
)

func TestRenderComponent_ARenderedFileIsForItsOwnerOnly(t *testing.T) {
	content := []byte(`{"env":{"TOKEN":"{{cf-secret:tok/a}}"}}`)
	c := harness.Component{Name: claude.SettingsComponent, Files: []harness.File{
		{Path: "settings.json", Mode: 0o644, Content: content, SecretRefs: harness.SecretRefs(content)},
		{Path: "plain.md", Mode: 0o644, Content: []byte("no secrets\n")},
	}}
	out, err := renderComponent(context.Background(), claude.Adapter{}, func(context.Context, string) ([]byte, error) { return []byte("v"), nil }, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Files[0].Mode; got != 0o600 {
		t.Errorf("rendered mode = %o, want 600", got)
	}
	if got := out.Files[1].Mode; got != 0o644 {
		t.Errorf("plain mode = %o, want 644 kept", got)
	}
}
