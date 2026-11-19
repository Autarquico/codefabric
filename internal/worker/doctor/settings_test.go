package doctor_test

import (
	"testing"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	"github.com/Autarquico/codeflock/internal/adapters/harness/command"
	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/worker/doctor"
)

func offeredIDs(t *testing.T, s *doctor.Settings) []string {
	t.Helper()
	p := config.Paths{ConfigDir: t.TempDir()}
	if s != nil {
		if err := doctor.SaveSettings(p, *s); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	for _, a := range doctor.Offered(p, []harness.Adapter{command.Adapter{}, codex.Adapter{}, claude.Adapter{}}) {
		ids = append(ids, a.ID())
	}
	return ids
}

func TestOffered_ConfigWithoutAChoiceOffersEverything(t *testing.T) {
	for _, s := range []*doctor.Settings{nil, {Name: "w"}} {
		if ids := offeredIDs(t, s); len(ids) != 3 {
			t.Errorf("%v", ids)
		}
	}
}

func TestOffered_AnUnchosenHarnessIsLeftOutButTheCommandStays(t *testing.T) {
	if ids := offeredIDs(t, &doctor.Settings{Harnesses: []string{claude.ID}}); len(ids) != 2 || ids[0] != "command" || ids[1] != claude.ID {
		t.Errorf("%v", ids)
	}
	if ids := offeredIDs(t, &doctor.Settings{Harnesses: []string{}}); len(ids) != 1 || ids[0] != "command" {
		t.Errorf("no harness chosen: %v", ids)
	}
}
