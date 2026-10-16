package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
)

func TestTheHostHasTheEnvironmentOfTheDaemonWithoutTmux(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,1,0")
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("CF_TEST_KEPT", "yes")
	d := &daemon{clock: clock.NewFake(time.Unix(0, 0))}
	h, err := d.host()
	if err != nil {
		t.Fatal(err)
	}
	if h.Getenv("CF_TEST_KEPT") != "yes" {
		t.Error("the Host lost the environment of the daemon")
	}
	for _, kv := range h.Environ() {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			t.Errorf("the Host has %s", kv)
		}
	}
}
