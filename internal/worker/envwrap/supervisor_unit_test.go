//go:build linux || darwin

package envwrap

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

func newTestSupervisor(t *testing.T, clk clock.Clock, l Launch) *supervisor {
	t.Helper()
	l.Dir = t.TempDir()
	return &supervisor{clock: clk, env: os.Environ(), launch: l, stop: make(chan struct{})}
}

func blockUntil(t *testing.T, f *clock.Fake, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := f.BlockUntil(ctx, n); err != nil {
		t.Fatal(err)
	}
}

func TestAScriptThatOutlastsItsTimeoutOnTheClockIsKilled(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	s := newTestSupervisor(t, fake, Launch{})
	got := make(chan result, 1)
	go func() { got <- s.runScript("sleep 300", 10*time.Minute, nil) }()
	blockUntil(t, fake, 1)
	select {
	case r := <-got:
		t.Fatalf("ended before its time: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	fake.Advance(10 * time.Minute)
	r := <-got
	if r.end != endTimeout || r.failure("prepare") != "prepare timed out after 10m0s" {
		t.Errorf("result = %+v (%q)", r, r.failure("prepare"))
	}
}

func TestAStopEndsAScriptWithoutFailingIt(t *testing.T) {
	s := newTestSupervisor(t, clock.Real{}, Launch{})
	got := make(chan result, 1)
	go func() { got <- s.runScript("sleep 300", time.Hour, s.stop) }()
	time.Sleep(50 * time.Millisecond)
	close(s.stop)
	if r := <-got; r.end != endStopped || r.failure("prepare") != "" {
		t.Errorf("result = %+v", r)
	}
}

func TestAServiceThatIsNeverReadyFailsOnTheClockAndTheProbeIsCancelled(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	s := newTestSupervisor(t, fake, Launch{ReadyTimeout: time.Minute})
	svc := domain.EnvironmentService{Name: "db", Start: "sleep 300", Ready: "false"}
	p, err := s.spawn(svc.Start)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.kill)
	type out struct {
		e   end
		msg string
	}
	got := make(chan out, 1)
	go func() { e, m := s.awaitReady(svc, p); got <- out{e, m} }()
	blockUntil(t, fake, 2) // the deadline and the ticker of the probes
	fake.Advance(time.Minute)
	if o := <-got; o.e != endTimeout || o.msg != "not ready after 1m0s" {
		t.Errorf("outcome = %+v", o)
	}
}

func TestAServiceThatDiesWhileNotReadyFailsAtOnce(t *testing.T) {
	s := newTestSupervisor(t, clock.Real{}, Launch{ReadyTimeout: time.Hour})
	svc := domain.EnvironmentService{Name: "db", Start: "exit 3", Ready: "false"}
	p, err := s.spawn(svc.Start)
	if err != nil {
		t.Fatal(err)
	}
	if e, msg := s.awaitReady(svc, p); e != endFailed || !strings.Contains(msg, "exited with code 3") {
		t.Errorf("outcome = %v %q", e, msg)
	}
}

func TestAStartThatExitsZeroIsDetachedAndReadyDecides(t *testing.T) {
	s := newTestSupervisor(t, clock.Real{}, Launch{ReadyTimeout: 10 * time.Second})
	s.o.ReadyInterval = 10 * time.Millisecond
	dir := s.launch.Dir
	svc := domain.EnvironmentService{Name: "db", Start: "touch " + dir + "/up", Ready: "test -e " + dir + "/up"}
	p, err := s.spawn(svc.Start)
	if err != nil {
		t.Fatal(err)
	}
	if e, msg := s.awaitReady(svc, p); e != endReady {
		t.Errorf("outcome = %v %q", e, msg)
	}
}

func TestAMarkerMessageIsRedactedByShapeAndBounded(t *testing.T) {
	got := bound("failed: " + strings.Repeat("x", 3*MaxMessage) + " https://u:p@host/x?token=1")
	if len(got) > MaxMessage+len("...") || strings.Contains(got, "token=1") {
		t.Errorf("len = %d, %q", len(got), got[len(got)-40:])
	}
}

func TestTheFilesOfTheContractRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Launch{Dir: "/w", Prepare: "make", Services: []domain.EnvironmentService{{Name: "a", Start: "s"}}, SecretEnv: []execwrap.SecretEnv{{Name: "K", Ref: "r"}}, StopTimeout: time.Second}
	if err := execwrap.Write(dir, LaunchFile, in); err != nil {
		t.Fatal(err)
	}
	var out Launch
	if ok, err := execwrap.Read(dir, LaunchFile, &out); !ok || err != nil || out.Prepare != "make" || out.SecretEnv[0].Ref != "r" || out.StopTimeout != time.Second {
		t.Errorf("%+v %v %v", out, ok, err)
	}
	var p Prepared
	if ok, err := execwrap.Read(dir, PreparedFile, &p); ok || err != nil {
		t.Errorf("a missing marker is not ok: %v %v", ok, err)
	}
}

// A supervisor that does not lead its group fails without signalling it: the
// group is somebody else's (here, the test's).
func TestAFailureBeforeLeadingTheGroupSignalsNothing(t *testing.T) {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	dir := t.TempDir()
	s := &supervisor{o: Options{RunDir: dir}, clock: clock.Real{}, self: syscall.Getpgrp() + 1, pgid: syscall.Getpgrp(), stop: make(chan struct{})}
	if err := s.run(); err == nil {
		t.Fatal("a supervisor that does not lead its group ran")
	}
	var exit Exit
	if ok, err := execwrap.Read(dir, ExitFile, &exit); err != nil || !ok || exit.Cause != CauseError || exit.GroupKilled {
		t.Errorf("env-exit.json = %+v (%v, %v)", exit, ok, err)
	}
	select {
	case sig := <-signals:
		t.Errorf("the group was signalled: %v", sig)
	case <-time.After(100 * time.Millisecond):
	}
}

// A supervisor that fails before claiming its identity leaves the files alone
// when another supervisor already holds the Run.
func TestAFailureBeforeTheClaimWritesNothingWhenAnotherSupervisorHoldsTheRun(t *testing.T) {
	dir := t.TempDir()
	if err := execwrap.Write(dir, WrapperFile, execwrap.Wrapper{PID: 1, Start: "other"}); err != nil {
		t.Fatal(err)
	}
	s := &supervisor{o: Options{RunDir: dir}, clock: clock.Real{}, self: syscall.Getpgrp() + 1, pgid: syscall.Getpgrp(), stop: make(chan struct{})}
	if err := s.run(); err == nil {
		t.Fatal("a supervisor that does not lead its group ran")
	}
	if ok, err := execwrap.Read(dir, ExitFile, &Exit{}); err != nil || ok {
		t.Errorf("env-exit.json written (%v, %v) over the supervisor that holds the Run", ok, err)
	}
}

func TestADrainThatIsHeldIsCutOnTheClockOfTheSupervisor(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	c := newCapture(io.Discard, nil, fake)
	p, err := c.attach(exec.CommandContext(t.Context(), "true")) // its ends stay open: something holds the pipe
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() { got <- p.drain() }()
	blockUntil(t, fake, 1)
	select {
	case err := <-got:
		t.Fatalf("cut before its time: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	fake.Advance(defaultDrainWait)
	if err := <-got; err == nil || !strings.Contains(err.Error(), "capture incomplete") {
		t.Errorf("drain = %v", err)
	}
	p.release()
}

func TestRecoverWithoutSecretsAttemptsNoStopAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	ran := dir + "/ran"
	l := Launch{Dir: dir, RedactOutput: true, Services: []domain.EnvironmentService{
		{Name: "db", Start: "x", Stop: "touch " + ran},
		{Name: "cache", Start: "x"},
	}}
	for name, v := range map[string]any{LaunchFile: l, StartedFile: Started{Services: []string{"db", "cache"}}} {
		if err := execwrap.Write(dir, name, v); err != nil {
			t.Fatal(err)
		}
	}
	revoked := func() ([]redact.Needle, error) { return nil, errors.New("no access") }
	if err := Recover(Options{RunDir: dir, Retention: revoked}); err != nil {
		t.Fatal(err)
	}
	var exit Exit
	if ok, err := execwrap.Read(dir, ExitFile, &exit); !ok || err != nil {
		t.Fatalf("exit: %v, %v", ok, err)
	}
	want := []StopFailure{{Service: "db", Error: lostStopError}}
	if exit.Cause != CauseLost || !slices.Equal(exit.StopFailures, want) {
		t.Errorf("exit = %+v", exit)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("a stop ran without the environment of the supervisor")
	}
}
