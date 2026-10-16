package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
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

// undetectable is a harness that Start would run but whose Detect fails.
type undetectable struct{ fakeharness.Adapter }

func (undetectable) Detect(context.Context, harness.Host) (harness.Detection, error) {
	return harness.Detection{}, errors.New("detect broke")
}

func TestTheExecutorKeepsEveryAdapterAndWithholdsThoseNotOffered(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	headless := harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}}
	d := &daemon{log: slog.New(slog.DiscardHandler)}
	reg, withheld, capabilities, err := d.announce(t.Context(), h, []harness.Adapter{
		fakeharness.Adapter{Binary: self, Caps: headless},
		undetectable{fakeharness.Adapter{Name: "broken", Binary: self, Caps: headless}},
		fakeharness.Adapter{Name: "absent", Binary: "/nonexistent/fakeharness", Caps: headless},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.IDs(); !slices.Equal(slices.Sorted(slices.Values(got)), []string{"absent", "broken", "fake"}) {
		t.Errorf("the executor has %v", got)
	}
	if !slices.Equal(slices.Sorted(slices.Values(withheld)), []string{"absent", "broken"}) {
		t.Errorf("withheld = %v", withheld)
	}
	if slices.ContainsFunc(capabilities, func(c string) bool { return strings.Contains(c, "broken") || strings.Contains(c, "absent") }) {
		t.Errorf("capabilities = %v", capabilities)
	}
}
