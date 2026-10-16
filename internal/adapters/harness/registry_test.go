package harness_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

func TestRegistryGetsByIDAndListsThemSorted(t *testing.T) {
	r, err := harness.NewRegistry(minimal{id: "codex"}, minimal{id: "claude-code"}, minimal{id: "command"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.IDs(), []string{"claude-code", "codex", "command"}; !slices.Equal(got, want) {
		t.Errorf("IDs() = %v, want %v", got, want)
	}
	if a, ok := r.Get("codex"); !ok || a.ID() != "codex" {
		t.Errorf("Get(codex) = %v, %v", a, ok)
	}
	if _, ok := r.Get("pi"); ok {
		t.Error("Get(pi) found an adapter that was never registered")
	}
}

func TestRegistryWithoutAdaptersIsEmpty(t *testing.T) {
	r, err := harness.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if ids := r.IDs(); len(ids) != 0 {
		t.Errorf("IDs() = %v, want none", ids)
	}
}

func TestRegistryAcceptsTheIDsOfTheGrammar(t *testing.T) {
	for _, id := range []string{"a", "fake", "claude-code", "pi2", "a" + strings.Repeat("-", 31)} {
		if _, err := harness.NewRegistry(minimal{id: id}); err != nil {
			t.Errorf("NewRegistry(%q): %v", id, err)
		}
	}
}

func TestRegistryRefusesInvalidIDs(t *testing.T) {
	for _, id := range []string{"", "Claude", "1codex", "-x", "claude_code", "claude code", "x.y", "a" + strings.Repeat("b", 32), "códex"} {
		if _, err := harness.NewRegistry(minimal{id: id}); err == nil {
			t.Errorf("NewRegistry(%q) accepted an invalid ID", id)
		}
	}
}

func TestRegistryRefusesDuplicates(t *testing.T) {
	_, err := harness.NewRegistry(minimal{id: "codex"}, minimal{id: "codex"})
	if err == nil || !strings.Contains(err.Error(), `"codex"`) {
		t.Errorf("NewRegistry with a duplicate: %v", err)
	}
}

func TestRegistryRefusesNil(t *testing.T) {
	if _, err := harness.NewRegistry(nil); err == nil {
		t.Error("NewRegistry(nil) accepted a nil adapter")
	}
}
