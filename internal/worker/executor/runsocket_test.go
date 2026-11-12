package executor

import (
	"slices"
	"sync"
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

type fakeRunSockets struct {
	mu     sync.Mutex
	served []string
	failed bool
}

func (f *fakeRunSockets) Serve(run string, _ uint64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, run)
	if f.failed {
		return "", errSocket
	}
	return "/s/" + run + ".sock", nil
}

func (f *fakeRunSockets) Stop(string, uint64) {}

var errSocket = &socketErr{}

type socketErr struct{}

func (*socketErr) Error() string { return "no socket" }

func launchOf(t *testing.T, f *fixture, mode string) execwrap.Launch {
	t.Helper()
	k := f.assign(specJSON("vim", "command", mode))
	f.e.decideOnce(t.Context())
	f.e.step(t.Context(), k)
	var launch execwrap.Launch
	if _, err := execwrap.Read(f.runDir(k), execwrap.LaunchFile, &launch); err != nil {
		t.Fatal(err)
	}
	return launch
}

func TestAnInteractiveRunLearnsItsSocketFromItsEnvironment(t *testing.T) {
	socks := &fakeRunSockets{}
	f := newFixture(t, func(o *Options) { o.RunSockets = socks })
	launch := launchOf(t, f, "interactive")
	if !slices.ContainsFunc(launch.Env, func(kv string) bool { return len(kv) > 14 && kv[:14] == "CF_RUN_SOCKET=" }) {
		t.Errorf("env = %v, want CF_RUN_SOCKET", launch.Env)
	}
}

func TestAHeadlessRunHasNoSocket(t *testing.T) {
	socks := &fakeRunSockets{}
	f := newFixture(t, func(o *Options) { o.RunSockets = socks })
	launch := launchOf(t, f, "headless")
	for _, kv := range launch.Env {
		if len(kv) >= 14 && kv[:14] == "CF_RUN_SOCKET=" {
			t.Errorf("a headless Run was given %s", kv)
		}
	}
	if len(socks.served) != 0 {
		t.Errorf("a socket was made for a headless Run: %v", socks.served)
	}
}

func TestARunWhoseSocketCannotBeMadeStillRuns(t *testing.T) {
	socks := &fakeRunSockets{failed: true}
	f := newFixture(t, func(o *Options) { o.RunSockets = socks })
	launch := launchOf(t, f, "interactive")
	if len(launch.Argv) == 0 {
		t.Fatal("the Run was not launched")
	}
	for _, kv := range launch.Env {
		if len(kv) >= 14 && kv[:14] == "CF_RUN_SOCKET=" {
			t.Errorf("env has %s though no socket was made", kv)
		}
	}
}
