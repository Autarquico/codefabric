//go:build integration && (linux || darwin)

package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
	workerdaemon "github.com/Autarquico/codeflock/internal/worker/daemon"
)

// lingeringHarness is a headless harness fed by a stream that gives its result
// and then does not exit, as Claude Code can: it sleeps until it is stopped.
type lingeringHarness struct {
	fakeharness.Adapter
	result string
}

func (a lingeringHarness) Start(ctx context.Context, h harness.Host, run harness.RunSpec) (harness.Launch, error) {
	l, err := a.Adapter.Start(ctx, h, run)
	l.Argv = []string{"/bin/sh", "-c", "printf '%s\\n' " + shellQuote(a.result) + "; exec sleep 3600"}
	l.StructuredStdout = true
	return l, err
}

// lingeringRun runs a headless Task whose harness never exits after its result,
// on a Worker that acts as if the Controller lacked the features hidden.
func lingeringRun(t *testing.T, answer string, hidden ...string) (v1.Run, string) {
	t.Helper()
	requireTmux(t)
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")

	t.Setenv(asCF, "1")
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- workerdaemon.Run(ctx, workerdaemon.Options{
			Paths: box, Interval: 200 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(logs, nil)),
			IdleAfterResult: time.Second, HideControllerFeatures: hidden, StopGrace: 2 * time.Second,
			Harnesses: []harness.Adapter{lingeringHarness{
				Adapter: fakeharness.Adapter{Binary: "/bin/sh", Caps: harness.Capabilities{
					Modes: []domain.Mode{domain.ModeHeadless}, StructuredInput: true, NeedsInputSignal: true, IdleAfterResult: true,
				}},
				result: resultLine(t, answer),
			}},
		})
	}()
	t.Cleanup(func() {
		cancel()
		rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(box.WorkerDataDir())})
		if err == nil {
			_ = rt.KillServer(context.WithoutCancel(t.Context()))
		}
		if err := <-done; err != nil {
			t.Errorf("worker daemon: %v", err)
		}
	})
	soon(t, "the Worker to be online", func() bool { return showWorker(t, host, "alpha").State == domain.WorkerOnline })

	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := ok[v1.CreateTaskResponse](t, "do", "say it", "--harness", fakeharness.DefaultID)
	var run v1.Run
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Run %s/%s %q\nWorker:\n%s", run.State, run.EndReason, run.FailureCode, logs.String())
		}
	})
	eventually(t, "the Run to end", func() bool {
		run = ok[v1.Run](t, "run", "show", created.Run.ID)
		return run.State.IsTerminal()
	})
	return run, logs.String()
}

func TestScenario_IdleAfterResult_ARunWhoseHarnessStaysIdleIsCompletedWithItsAnswer(t *testing.T) {
	run, _ := lingeringRun(t, "all done")
	if run.State != domain.RunCompleted || run.EndReason != domain.EndIdleAfterResult || run.Answer != "all done" {
		t.Fatalf("Run = %s/%s, answer %q", run.State, run.EndReason, run.Answer)
	}
}

func TestScenario_IdleAfterResult_AControllerWithoutTheFeatureSettlesTheStoppedRunAsBefore(t *testing.T) {
	run, logs := lingeringRun(t, "all done", v1.FeatureIdleAfterResult)
	if run.State != domain.RunFailed || run.EndReason == domain.EndIdleAfterResult {
		t.Fatalf("Run = %s/%s", run.State, run.EndReason)
	}
	if run.Answer != "all done" {
		t.Errorf("answer = %q: it is still reported", run.Answer)
	}
	if !strings.Contains(logs, "idle_after_result") {
		t.Errorf("the Worker log does not say the harness was stopped for idleness:\n%s", logs)
	}
}
