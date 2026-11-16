package daemon_test

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"testing"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/daemon"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

// attachRuntime is a Runtime that can say which program attaches to its sessions.
type attachRuntime struct{ recordingRuntime }

func (*attachRuntime) AttachBinary() (string, string) { return "tmux", "/fake/tmux" }

func runWith(t *testing.T, paths config.Paths, mutate func(*daemon.Options)) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	opts := daemon.Options{Paths: paths, Interval: interval, Logger: slog.New(slog.DiscardHandler), Runtime: &attachRuntime{}}
	mutate(&opts)
	done := make(chan struct{})
	go func() { defer close(done); _ = daemon.Run(ctx, opts) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestRun_AnnouncesHowToAttachWhatItIsToldAndTheBinaryOfItsRuntime(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	runWith(t, paths, func(o *daemon.Options) { o.SSHTarget = "alex@box.lan:2222" })
	w := c.eventually("box", "the announcement", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
	if w.SSHTarget != "alex@box.lan:2222" || w.AttachBinaries["tmux"] != "/fake/tmux" || len(w.AttachBinaries) != 1 {
		t.Fatalf("Worker = %+v", w)
	}
}

func TestRun_TheDefaultTargetIsTheUserAtTheHostnameAndNoneAnnouncesNothing(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	runWith(t, paths, func(*daemon.Options) {})
	w := c.eventually("box", "online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
	u, _ := user.Current()
	host, _ := os.Hostname()
	if guess := u.Username + "@" + host; domain.ValidSSHTarget(guess) && w.SSHTarget != guess {
		t.Errorf("default target = %q, want %q", w.SSHTarget, guess)
	}

	other := newController(t)
	paths2 := machine(t)
	other.join("quiet", paths2)
	runWith(t, paths2, func(o *daemon.Options) { o.SSHTarget = daemon.NoSSHTarget })
	q := other.eventually("quiet", "online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
	if q.SSHTarget != "" {
		t.Errorf("none announced %q", q.SSHTarget)
	}
}

func TestRun_RefusesAnSSHTargetThatIsNotOne(t *testing.T) {
	c := newController(t)
	paths := machine(t)
	c.join("box", paths)
	for _, bad := range []string{"-oProxyCommand=x", "a b", "u@h;id"} {
		err := daemon.Run(t.Context(), daemon.Options{Paths: paths, SSHTarget: bad, Logger: slog.New(slog.DiscardHandler)})
		var coded *domain.Error
		if !asCoded(err, &coded) || coded.Code != domain.CodeInvalidRequest {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func asCoded(err error, into **domain.Error) bool {
	e, ok := err.(*domain.Error) //nolint:errorlint // Run returns it as it is
	if ok {
		*into = e
	}
	return ok
}

// relayRuntime is a Runtime that tmux-like clients can attach to from this machine.
type relayRuntime struct{ attachRuntime }

func (*relayRuntime) AttachCommand(cfruntime.Handle, bool) (cfruntime.Command, error) {
	path, _ := exec.LookPath("tmux") // never run here; a path of this machine, not a layout of one
	return cfruntime.Command{Path: path}, nil
}

// The relay of attach is announced only by a Worker that runs its loop: one
// whose Runtime can attach from this machine, and that was not told to leave it
// to ssh. Stopping the daemon ends the loop (the cleanup of runWith waits for it).
func TestRun_AnnouncesTheAttachRelayOnlyWhenItRunsIt(t *testing.T) {
	for name, tc := range map[string]struct {
		runtime cfruntime.Runtime
		off     bool
		want    bool
	}{
		"a runtime that attaches":      {runtime: &relayRuntime{}, want: true},
		"told to leave it to ssh":      {runtime: &relayRuntime{}, off: true},
		"a runtime that cannot attach": {runtime: &attachRuntime{}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newController(t)
			paths := machine(t)
			c.join("box", paths)
			runWith(t, paths, func(o *daemon.Options) { o.Runtime, o.NoAttachRelay = tc.runtime, tc.off })
			w := c.eventually("box", "online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
			if got := slices.Contains(w.Capabilities, domain.AttachRelayCapability); got != tc.want {
				t.Errorf("announces the relay = %v, want %v (capabilities %v)", got, tc.want, w.Capabilities)
			}
		})
	}
}

// The shell is announced only by a Worker that runs the relay and whose
// allow_shell is on: the Controller does not grant a shell to one that does not.
func TestRun_AnnouncesTheShellOnlyWithAllowShellAndTheRelay(t *testing.T) {
	for name, tc := range map[string]struct {
		allow, relayOff bool
		want            bool
	}{
		"allowed, with the relay": {allow: true, want: true},
		"not allowed":             {},
		"allowed but no relay":    {allow: true, relayOff: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := newController(t)
			paths := machine(t)
			c.join("box", paths)
			s, err := doctor.LoadSettings(paths)
			if err != nil {
				t.Fatal(err)
			}
			s.AllowShell = tc.allow
			if err := doctor.SaveSettings(paths, s); err != nil {
				t.Fatal(err)
			}
			runWith(t, paths, func(o *daemon.Options) { o.Runtime, o.NoAttachRelay = &relayRuntime{}, tc.relayOff })
			w := c.eventually("box", "online", func(w v1.Worker) bool { return w.State == domain.WorkerOnline })
			if got := slices.Contains(w.Capabilities, domain.AttachShellCapability); got != tc.want {
				t.Errorf("announces the shell = %v, want %v (capabilities %v)", got, tc.want, w.Capabilities)
			}
		})
	}
}
