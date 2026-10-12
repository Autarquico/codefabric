package envutil_test

import (
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/envutil"
)

func TestWithoutTmuxDropsOnlyTheTmuxVariables(t *testing.T) {
	env := []string{"A=1", "TMUX=/tmp/x,1,0", "TMUX_PANE=%1", "TMUXISH=keep", "B=2"}
	got := envutil.WithoutTmux(env)
	if !slices.Equal(got, []string{"A=1", "TMUXISH=keep", "B=2"}) {
		t.Errorf("got %v", got)
	}
	if len(env) != 5 || env[1] != "TMUX=/tmp/x,1,0" {
		t.Error("the input was changed")
	}
}
