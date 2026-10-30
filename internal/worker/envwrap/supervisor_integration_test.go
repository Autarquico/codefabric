//go:build integration && (linux || darwin)

package envwrap_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/envwrap"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
)

const (
	asSupervisor = "CF_TEST_AS_ENVWRAP"
	withDelivery = "CF_TEST_WITH_DELIVERY"
	withGate     = "CF_TEST_GATE_TIMEOUT"
)

// The supervisor signals its group, and a group of the test binary would be
// the test: it runs as a process of its own, in a session of its own.
func TestMain(m *testing.M) {
	delivery := os.Getenv(withDelivery)
	_ = os.Unsetenv(withDelivery)
	if dir := os.Getenv(asSupervisor); dir != "" {
		_ = os.Unsetenv(asSupervisor)
		o := envwrap.Options{RunDir: dir, GateTimeout: 5 * time.Second, ReadyInterval: 20 * time.Millisecond}
		if g, err := time.ParseDuration(os.Getenv(withGate)); err == nil {
			o.GateTimeout = g
		}
		if delivery == "1" {
			o.Secrets = secretjit.Resolver(dir)
			o.Retention = func() ([]redact.Needle, error) { return secretjit.Load(dir) }
		}
		if err := envwrap.Run(o); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type spawned struct {
	t       *testing.T
	runDir  string
	workDir string
	cmd     *exec.Cmd
	exited  chan struct{}
}

func deliveredDir(t *testing.T, secrets ...redact.Needle) string {
	t.Helper()
	dir := t.TempDir()
	if err := secretjit.New().Deliver(dir, 1, secrets, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return dir
}

// start writes the launch and starts the supervisor, which waits at the gate.
func start(t *testing.T, runDir string, l envwrap.Launch, env ...string) *spawned {
	t.Helper()
	s := &spawned{t: t, runDir: runDir, workDir: t.TempDir(), exited: make(chan struct{})}
	l.Dir = s.workDir
	if l.RunID == "" {
		l.RunID, l.LeaseEpoch = "run-test", 1
	}
	if err := execwrap.Write(runDir, envwrap.LaunchFile, l); err != nil {
		t.Fatal(err)
	}
	s.cmd = exec.CommandContext(t.Context(), os.Args[0]) //nolint:gosec // the test binary itself
	s.cmd.Env = append(append(os.Environ(), asSupervisor+"="+runDir, "TMUX=/tmp/tmux-0/default,1,0"), env...)
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		_ = s.cmd.Process.Kill()
	})
	go func() {
		_ = s.cmd.Wait()
		close(s.exited)
	}()
	return s
}

func (s *spawned) authorize() {
	s.t.Helper()
	s.waitFor(envwrap.WrapperFile)
	var w execwrap.Wrapper
	if ok, err := execwrap.Read(s.runDir, envwrap.WrapperFile, &w); !ok || err != nil {
		s.t.Fatalf("env-wrapper.json: %v", err)
	}
	s.authorizeAs(w)
}

// authorizeAs writes the gate for the identity w, whoever it is.
func (s *spawned) authorizeAs(w execwrap.Wrapper) {
	s.t.Helper()
	a := envwrap.Authorized{RunID: "run-test", LeaseEpoch: 1, PID: w.PID, PGID: w.PGID, Start: w.Start, At: time.Now()}
	if err := execwrap.Write(s.runDir, envwrap.AuthorizedFile, a); err != nil {
		s.t.Fatal(err)
	}
}

func (s *spawned) order() {
	s.t.Helper()
	if err := execwrap.Write(s.runDir, envwrap.StopFile, envwrap.StopOrder{At: time.Now(), Reason: "test"}); err != nil {
		s.t.Fatal(err)
	}
}

func (s *spawned) waitFor(name string) {
	s.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(s.runDir, name)); err == nil {
			return
		}
	}
	s.t.Fatalf("%s never appeared", name)
}

func (s *spawned) exit() envwrap.Exit {
	s.t.Helper()
	s.waitFor(envwrap.ExitFile)
	select {
	case <-s.exited:
	case <-time.After(10 * time.Second):
		s.t.Fatal("env-exit.json was written and the supervisor did not end")
	}
	var e envwrap.Exit
	if ok, err := execwrap.Read(s.runDir, envwrap.ExitFile, &e); !ok || err != nil {
		s.t.Fatal(ok, err)
	}
	return e
}

func (s *spawned) prepared() envwrap.Prepared {
	s.t.Helper()
	s.waitFor(envwrap.PreparedFile)
	var p envwrap.Prepared
	if _, err := execwrap.Read(s.runDir, envwrap.PreparedFile, &p); err != nil {
		s.t.Fatal(err)
	}
	return p
}

func (s *spawned) services() envwrap.Services {
	s.t.Helper()
	s.waitFor(envwrap.ServicesFile)
	var v envwrap.Services
	if _, err := execwrap.Read(s.runDir, envwrap.ServicesFile, &v); err != nil {
		s.t.Fatal(err)
	}
	return v
}

func (s *spawned) work(name string) string {
	raw, _ := os.ReadFile(filepath.Join(s.workDir, name)) //nolint:gosec // a file of the test
	return string(raw)
}

func (s *spawned) exists(name string) bool {
	_, err := os.Stat(filepath.Join(s.workDir, name))
	return err == nil
}

func svc(name, start, ready, stop string) domain.EnvironmentService {
	return domain.EnvironmentService{Name: name, Start: start, Ready: ready, Stop: stop}
}

func TestNothingRunsBeforeTheAuthorization(t *testing.T) {
	s := start(t, t.TempDir(), envwrap.Launch{Prepare: "touch ran"})
	s.waitFor(envwrap.WrapperFile)
	time.Sleep(300 * time.Millisecond)
	if s.exists("ran") {
		t.Fatal("prepare ran without the authorization")
	}
	var w execwrap.Wrapper
	if ok, err := execwrap.Read(s.runDir, envwrap.WrapperFile, &w); !ok || err != nil || w.PID != s.cmd.Process.Pid || w.PGID != w.PID || w.Start == "" {
		t.Errorf("env-wrapper.json = %+v (%v)", w, err)
	}
	s.authorize()
	s.waitFor(envwrap.ServicesFile)
}

func TestAFailingPrepareLeavesAFailedMarkerWithARedactedMessage(t *testing.T) {
	const value = "s3cr3t-sentinel"
	runDir := deliveredDir(t, redact.Needle{Ref: "team/api-key", Value: []byte(value)})
	s := start(t, runDir, envwrap.Launch{
		Prepare:   `echo "installing with $API_KEY"; echo "cannot reach host" >&2; exit 7`,
		Services:  []domain.EnvironmentService{svc("db", "touch started", "", "")},
		SecretEnv: []execwrap.SecretEnv{{Name: "API_KEY", Ref: "team/api-key"}},
	}, withDelivery+"=1")
	s.authorize()
	p := s.prepared()
	if p.Status != envwrap.StatusFailed || !strings.Contains(p.Message, "prepare exited with code 7") || !strings.Contains(p.Message, "cannot reach host") {
		t.Errorf("prepared.json = %+v", p)
	}
	if e := s.exit(); e.Cause != envwrap.CausePrepareFailed || e.GroupKilled {
		t.Errorf("env-exit.json = %+v", e)
	}
	if s.exists("started") {
		t.Error("a service started after a failed prepare")
	}
	for _, name := range []string{envwrap.PreparedFile, envwrap.OutputLog, envwrap.ExitFile} {
		raw, _ := os.ReadFile(filepath.Join(runDir, name)) //nolint:gosec // a file of the test
		if strings.Contains(string(raw), value) {
			t.Errorf("%s holds the secret: %s", name, raw)
		}
	}
	if log, _ := os.ReadFile(filepath.Join(runDir, envwrap.OutputLog)); !strings.Contains(string(log), "installing with") { //nolint:gosec // a file of the test
		t.Errorf("environment.log = %q", log)
	}
}

func TestAFailedMarkerIsNeverPreparedAgain(t *testing.T) {
	runDir := t.TempDir()
	if err := execwrap.Write(runDir, envwrap.PreparedFile, envwrap.Prepared{Status: envwrap.StatusFailed, Message: "before"}); err != nil {
		t.Fatal(err)
	}
	s := start(t, runDir, envwrap.Launch{Prepare: "touch ran"})
	s.authorize()
	if e := s.exit(); e.Cause != envwrap.CausePrepareFailed || s.exists("ran") {
		t.Errorf("env-exit.json = %+v, prepare ran: %v", e, s.exists("ran"))
	}
}

func TestAPreparedMarkerIsNotPreparedAgainAndTheServicesStart(t *testing.T) {
	runDir := t.TempDir()
	if err := execwrap.Write(runDir, envwrap.PreparedFile, envwrap.Prepared{Status: envwrap.StatusOK}); err != nil {
		t.Fatal(err)
	}
	s := start(t, runDir, envwrap.Launch{Prepare: "touch ran", Services: []domain.EnvironmentService{svc("db", "sleep 300", "", "")}})
	s.authorize()
	if v := s.services(); v.Status != envwrap.StatusOK || s.exists("ran") {
		t.Errorf("services-ready.json = %+v, prepare ran: %v", v, s.exists("ran"))
	}
}

func TestAPrepareThatLeavesAChildWhichIgnoresTheTermGetsTheGroupKilled(t *testing.T) {
	runDir := t.TempDir()
	s := start(t, runDir, envwrap.Launch{
		Prepare: `(trap '' TERM; echo $$ > child.pid; exec sleep 300) & while [ ! -s child.pid ]; do :; done`,
		Grace:   300 * time.Millisecond,
	})
	s.authorize()
	p := s.prepared()
	if p.Status != envwrap.StatusFailed || !strings.Contains(p.Message, "left processes") {
		t.Errorf("prepared.json = %+v", p)
	}
	e := s.exit()
	if e.Cause != envwrap.CausePrepareFailed || !e.GroupKilled || e.RemnantsKilled == 0 {
		t.Errorf("env-exit.json = %+v", e)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(s.work("child.pid")))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child %d survived the group kill", pid)
		}
	}
}

func TestAStartThatExitsZeroIsDetachedAndReadyDecides(t *testing.T) {
	s := start(t, t.TempDir(), envwrap.Launch{
		Prepare:  "true",
		Services: []domain.EnvironmentService{svc("db", "sleep 0.2; touch up", "test -e up", "touch down")},
	})
	s.authorize()
	if v := s.services(); v.Status != envwrap.StatusOK || len(v.Ready) != 1 || v.Ready[0] != "db" {
		t.Errorf("services-ready.json = %+v", v)
	}
	s.order()
	if e := s.exit(); e.Cause != envwrap.CauseStop || len(e.StopFailures) != 0 || !s.exists("down") {
		t.Errorf("env-exit.json = %+v, down: %v", e, s.exists("down"))
	}
}

func TestAServiceThatIsNotReadyInTimeFailsAndTheFailingOneIsStoppedToo(t *testing.T) {
	s := start(t, t.TempDir(), envwrap.Launch{
		Prepare:      "true",
		ReadyTimeout: 300 * time.Millisecond,
		Services: []domain.EnvironmentService{
			svc("first", "sleep 300", "", "echo first >> stops"),
			svc("second", "sleep 300", "false", "echo second >> stops"),
			svc("third", "touch third-started", "", ""),
		},
	})
	s.authorize()
	v := s.services()
	if v.Status != envwrap.StatusFailed || !strings.Contains(v.Message, "service second: not ready after 300ms") {
		t.Errorf("services-ready.json = %+v", v)
	}
	e := s.exit()
	if e.Cause != envwrap.CauseServicesFailed {
		t.Errorf("env-exit.json = %+v", e)
	}
	if got := s.work("stops"); got != "second\nfirst\n" {
		t.Errorf("stops, in the order they ran = %q", got)
	}
	if s.exists("third-started") {
		t.Error("a service started after the failure")
	}
}

func TestAServiceWhoseStartFailsFailsTheEnvironment(t *testing.T) {
	s := start(t, t.TempDir(), envwrap.Launch{Prepare: "true", Services: []domain.EnvironmentService{svc("db", "echo boom >&2; exit 4", "", "")}})
	s.authorize()
	if v := s.services(); v.Status != envwrap.StatusFailed || !strings.Contains(v.Message, "exited with code 4") {
		t.Errorf("services-ready.json = %+v", v)
	}
	s.exit()
}

func TestAStopOrderedBeforeTheSpawnSpawnsNothing(t *testing.T) {
	s := start(t, t.TempDir(), envwrap.Launch{Prepare: "touch prepared-ran", Services: []domain.EnvironmentService{svc("db", "touch db-ran", "", "touch db-stopped")}})
	s.waitFor(envwrap.WrapperFile)
	s.order()
	s.authorize()
	e := s.exit()
	if e.Cause != envwrap.CauseStop {
		t.Errorf("env-exit.json = %+v", e)
	}
	for _, name := range []string{"prepared-ran", "db-ran", "db-stopped"} {
		if s.exists(name) {
			t.Errorf("%s exists: something ran after the stop order", name)
		}
	}
	for _, name := range []string{envwrap.PreparedFile, envwrap.ServicesFile} {
		if _, err := os.Stat(filepath.Join(s.runDir, name)); err == nil {
			t.Errorf("%s exists", name)
		}
	}
}

func TestAStopOrderedDuringPrepareEndsItWithoutAMarker(t *testing.T) {
	s := start(t, t.TempDir(), envwrap.Launch{Prepare: "touch in-prepare; sleep 300", Services: []domain.EnvironmentService{svc("db", "touch db-ran", "", "")}})
	s.authorize()
	for deadline := time.Now().Add(10 * time.Second); !s.exists("in-prepare"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("prepare never started")
		}
	}
	s.order()
	if e := s.exit(); e.Cause != envwrap.CauseStop || s.exists("db-ran") {
		t.Errorf("env-exit.json = %+v", e)
	}
	if _, err := os.Stat(filepath.Join(s.runDir, envwrap.PreparedFile)); err == nil {
		t.Error("a stop was recorded as the outcome of prepare")
	}
}

func TestStopRunsInReverseWithTheSecretsAndRecordsWhatFails(t *testing.T) {
	const value = "s3cr3t-sentinel"
	runDir := deliveredDir(t, redact.Needle{Ref: "team/api-key", Value: []byte(value)})
	s := start(t, runDir, envwrap.Launch{
		Prepare: "true",
		Services: []domain.EnvironmentService{
			svc("a", "sleep 300", "", `echo "a:$API_KEY" >> stops`),
			svc("b", "sleep 300", "", `echo "b:$API_KEY" >> stops; echo "bad $API_KEY" >&2; exit 5`),
		},
		SecretEnv: []execwrap.SecretEnv{{Name: "API_KEY", Ref: "team/api-key"}},
	}, withDelivery+"=1")
	s.authorize()
	s.services()
	s.order()
	e := s.exit()
	if want := "b:" + value + "\na:" + value + "\n"; s.work("stops") != want {
		t.Errorf("stops = %q, want %q", s.work("stops"), want)
	}
	if len(e.StopFailures) != 1 || e.StopFailures[0].Service != "b" || !strings.Contains(e.StopFailures[0].Error, "stop exited with code 5") {
		t.Errorf("env-exit.json = %+v", e)
	}
	for _, name := range []string{envwrap.OutputLog, envwrap.ExitFile, envwrap.ServicesFile} {
		raw, _ := os.ReadFile(filepath.Join(runDir, name)) //nolint:gosec // a file of the test
		if strings.Contains(string(raw), value) {
			t.Errorf("%s holds the secret: %s", name, raw)
		}
	}
}

func TestAStopThatHangsTimesOutAndDoesNotHoldTheEnvironment(t *testing.T) {
	s := start(t, t.TempDir(), envwrap.Launch{
		Prepare: "true", StopTimeout: 300 * time.Millisecond, Grace: 500 * time.Millisecond,
		Services: []domain.EnvironmentService{svc("db", "sleep 300", "", "sleep 300")},
	})
	s.authorize()
	s.services()
	s.order()
	e := s.exit()
	if len(e.StopFailures) != 1 || !strings.Contains(e.StopFailures[0].Error, "stop timed out after 300ms") {
		t.Errorf("env-exit.json = %+v", e)
	}
}

func TestASecretThatCannotBeResolvedFailsPrepareWithoutRunningIt(t *testing.T) {
	s := start(t, t.TempDir(), envwrap.Launch{Prepare: "touch ran", SecretEnv: []execwrap.SecretEnv{{Name: "K", Ref: "missing"}}})
	s.authorize()
	p := s.prepared()
	if p.Status != envwrap.StatusFailed || !strings.HasPrefix(p.Message, "CF_SECRET_UNAVAILABLE: ") || s.exists("ran") {
		t.Errorf("prepared.json = %+v", p)
	}
	s.exit()
}

// A first supervisor that is late must not run under the authorization of the
// one the daemon launched after it: only the identity the file holds runs.
func TestASecondSupervisorOfTheRunEndsSilentlyAndTheAuthorizedOneRuns(t *testing.T) {
	runDir := t.TempDir()
	first := start(t, runDir, envwrap.Launch{Prepare: "touch ran-first"})
	first.waitFor(envwrap.WrapperFile)
	late := start(t, runDir, envwrap.Launch{Prepare: "touch ran-late"})
	select {
	case <-late.exited:
	case <-time.After(20 * time.Second):
		t.Fatal("a supervisor that lost the identity kept waiting")
	}
	if late.exists(envwrap.ExitFile) || late.exists("ran-late") {
		t.Error("the supervisor that lost the identity left files or ran")
	}
	var w execwrap.Wrapper
	if ok, _ := execwrap.Read(runDir, envwrap.WrapperFile, &w); !ok || w.PID != first.cmd.Process.Pid {
		t.Fatalf("env-wrapper.json = %+v, want the first supervisor's", w)
	}
	first.authorize()
	first.waitFor(envwrap.ServicesFile)
}

func TestAnAuthorizationOfAnotherIdentityRunsNothing(t *testing.T) {
	for name, a := range map[string]envwrap.Authorized{
		"another pid":   {RunID: "run-test", LeaseEpoch: 1, PID: 1, PGID: 1, Start: "x"},
		"another Run":   {RunID: "other", LeaseEpoch: 1},
		"another epoch": {RunID: "run-test", LeaseEpoch: 2},
	} {
		t.Run(name, func(t *testing.T) {
			s := start(t, t.TempDir(), envwrap.Launch{Prepare: "touch ran"})
			var w execwrap.Wrapper
			s.waitFor(envwrap.WrapperFile)
			if ok, err := execwrap.Read(s.runDir, envwrap.WrapperFile, &w); !ok || err != nil {
				t.Fatal(err)
			}
			if a.PID == 0 {
				a.PID, a.PGID, a.Start = w.PID, w.PGID, w.Start
			}
			if err := execwrap.Write(s.runDir, envwrap.AuthorizedFile, a); err != nil {
				t.Fatal(err)
			}
			if e := s.exit(); e.Cause != envwrap.CauseError || s.exists("ran") {
				t.Errorf("env-exit.json = %+v, prepare ran: %v", e, s.exists("ran"))
			}
		})
	}
}

// A recovery supervisor goes through the same identity and gate as any other,
// ignores the stop order that is already there, runs the stops of the services
// the lost supervisor recorded in reverse order and nothing else, and leaves
// nothing of its group behind.
func TestARecoverySupervisorStopsWhatALostOneStartedAndNothingElse(t *testing.T) {
	runDir := t.TempDir()
	s := start(t, runDir, envwrap.Launch{
		Recover: true,
		Prepare: "touch prepare-ran",
		Services: []domain.EnvironmentService{
			svc("db", "touch db-start-ran", "", "echo db >> order"),
			svc("cache", "touch cache-start-ran", "", "echo cache >> order; sleep 600 & echo $! > leftover"),
			svc("unrecorded", "touch x", "", "echo unrecorded >> order"),
		},
	})
	if err := execwrap.Write(runDir, envwrap.StartedFile, envwrap.Started{Services: []string{"db", "cache"}}); err != nil {
		t.Fatal(err)
	}
	if err := execwrap.Write(runDir, envwrap.StopFile, envwrap.StopOrder{At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s.waitFor(envwrap.WrapperFile)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(runDir, envwrap.ExitFile)); err == nil {
		t.Fatal("a recovery supervisor ended at its gate on a stop order")
	}
	s.authorize()
	exit := s.exit()
	if exit.Cause != envwrap.CauseLost || len(exit.StopFailures) != 0 {
		t.Errorf("exit = %+v", exit)
	}
	if got := s.work("order"); got != "cache\ndb\n" {
		t.Errorf("stops ran as %q", got)
	}
	for _, name := range []string{"prepare-ran", "db-start-ran", "cache-start-ran"} {
		if s.exists(name) {
			t.Errorf("%s: a recovery runs no prepare and no start", name)
		}
	}
	pid, err := strconv.Atoi(strings.TrimSpace(s.work("leftover")))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("what a stop left behind outlived the recovery supervisor")
		}
	}
}

// A recovery supervisor whose gate times out has attempted no stop: its exit is
// not a final one, and it leaves the stops to the next recovery.
func TestARecoverySupervisorThatTimesOutAtItsGateSaysItRanNothing(t *testing.T) {
	runDir := t.TempDir()
	s := start(t, runDir, envwrap.Launch{
		Recover:  true,
		Services: []domain.EnvironmentService{svc("db", "x", "", "touch db-stopped")},
	}, withGate+"=200ms")
	if err := execwrap.Write(runDir, envwrap.StartedFile, envwrap.Started{Services: []string{"db"}}); err != nil {
		t.Fatal(err)
	}
	exit := s.exit()
	if exit.Cause != envwrap.CauseRecoveryNotRun || len(exit.StopFailures) != 0 {
		t.Errorf("exit = %+v", exit)
	}
	if s.exists("db-stopped") {
		t.Error("a stop ran without the authorization of the daemon")
	}
}
