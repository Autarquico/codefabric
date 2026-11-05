package actions_test

import (
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/cli/actions"
)

func TestParseLine_SplitsLikeAShellAndFindsTheAction(t *testing.T) {
	for _, c := range []struct {
		text, action string
		args         []string
	}{
		{"task cancel x", "task.cancel", []string{"x"}},
		{`  worker   drain   "my worker" `, "worker.drain", []string{"my worker"}},
		{`task new 'fix the "bug"' --name=a\ b`, "task.new", []string{`fix the "bug"`, "--name=a b"}},
		{"project secrets allow p env:TOKEN", "project.secrets.allow", []string{"p", "env:TOKEN"}},
		{"attach r1 --read-only", "run.attach", []string{"r1", "--read-only"}},
		{`task new ""`, "task.new", []string{""}},
	} {
		line, failure := actions.ParseLine(c.text)
		if failure != nil {
			t.Errorf("%q: %v", c.text, failure)
			continue
		}
		if line.Action.ID != c.action || !slices.Equal(line.Args, c.args) {
			t.Errorf("%q = %s %q, want %s %q", c.text, line.Action.ID, line.Args, c.action, c.args)
		}
	}
}

func TestParseLine_Refuses(t *testing.T) {
	for _, text := range []string{"", "   ", "task cancel 'x", `task cancel x\`, "controller run", "task list", "nope", "task cancel x --controller other:1", "task cancel x --controller=other:1"} {
		if _, failure := actions.ParseLine(text); failure == nil {
			t.Errorf("%q was accepted", text)
		}
	}
	// After "--" the same text is a value, not the flag.
	if _, failure := actions.ParseLine("task cancel -- --controller"); failure != nil {
		t.Errorf("a value after -- was refused: %v", failure)
	}
}
