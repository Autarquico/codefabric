package daemon_test

import (
	"context"
	"log/slog"
	"os"
	"os/user"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/daemon"
)

// attachRuntime is a Runtime that can say which program attaches to its sessions.
type attachRuntime struct{ recordingRuntime }

func (*attachRuntime) AttachBinary() (string, string) { return "tmux", "/opt/homebrew/bin/tmux" }

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
	if w.SSHTarget != "alex@box.lan:2222" || w.AttachBinaries["tmux"] != "/opt/homebrew/bin/tmux" || len(w.AttachBinaries) != 1 {
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
