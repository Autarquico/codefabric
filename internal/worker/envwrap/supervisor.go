package envwrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/envutil"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

const (
	// GateTimeout is how long the supervisor waits for the daemon's authorization.
	GateTimeout   = 30 * time.Second
	poll          = 50 * time.Millisecond
	readyInterval = 500 * time.Millisecond
	// startupCheck is how long a service without a ready script is watched for
	// dying of an error, on the clock of the supervisor.
	startupCheck = 250 * time.Millisecond
)

// Options parameterise Run.
type Options struct {
	RunDir string
	Clock  clock.Clock // defaults to clock.Real
	// Secrets resolves the SecretEnv of the launch (secretjit.Resolver) and
	// Retention is what the output is redacted of (secretjit.Load), as for the
	// wrapper of the harness.
	Secrets   execwrap.ResolveSecret
	Retention execwrap.LoadSecrets
	// GateTimeout zero is GateTimeout. ReadyInterval zero is 500ms.
	GateTimeout   time.Duration
	ReadyInterval time.Duration
}

// Run is the supervisor of the Environment. Like the wrapper of the harness it
// must lead its process group: everything it runs stays in that group, so the
// group is known before anything runs. SIGTERM, SIGINT and SIGHUP do not end it:
// they are a stop order. It returns once env-exit.json is written; the error is
// for what stopped it from doing its job, which env-exit.json then says.
func Run(o Options) error {
	clk := o.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	s := &supervisor{o: o, clock: clk, self: os.Getpid(), pgid: syscall.Getpgrp(), signals: make(chan os.Signal, 8), stop: make(chan struct{})}
	signal.Notify(s.signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(s.signals)
	return s.run()
}

type supervisor struct {
	o       Options
	clock   clock.Clock
	self    int
	pgid    int
	signals chan os.Signal
	stop    chan struct{} // closed once a stop is ordered, by file or by signal

	identity execwrap.Wrapper // who this supervisor is, once it is known
	ran      bool             // the gate was passed: scripts may have run, so the group is this process' to settle
	launch   Launch
	env      []string // the environment of every script
	set      *redact.Set
	problem  string // what stopped a marker from being written
	log      *os.File
	capture  *capture

	spawned   []domain.EnvironmentService // those whose start was recorded (env-started.json) before it was spawned, in order
	lostStops bool                        // Recover: the stops cannot run (no secrets): each is a failure
	remnants  int
	survivors bool // some process of the group outlived the grace
}

// watchStop closes s.stop when the order file appears or a stop signal comes.
func (s *supervisor) watchStop(done <-chan struct{}) {
	ticker := s.clock.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-s.signals:
			close(s.stop)
			return
		case <-ticker.C():
			if s.stopOrdered() {
				close(s.stop)
				return
			}
		case <-done:
			return
		}
	}
}

// stopOrdered reads the order itself, so that it is seen at once and not at the
// next tick of the watcher: it is checked before every spawn.
func (s *supervisor) stopOrdered() bool {
	if s.launch.Recover {
		return false
	}
	select {
	case <-s.stop:
		return true
	default:
	}
	_, err := os.Stat(s.path(StopFile))
	return err == nil
}

func (s *supervisor) path(name string) string { return s.o.RunDir + string(os.PathSeparator) + name }

func (s *supervisor) timeout(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

func (s *supervisor) run() error {
	// Before anything is read or written: a process that does not lead its group
	// has no group of its own to answer for, and signals none.
	if s.pgid != s.self {
		return s.fail("the supervisor does not lead its process group")
	}
	if ok, err := execwrap.Read(s.o.RunDir, LaunchFile, &s.launch); err != nil || !ok {
		return s.fail("the launch file is missing or unreadable")
	}
	if !s.launch.Recover {
		// A recovery supervisor does not heed a stop order or a signal: the order
		// is what it was launched for, and it ends when the stops are done.
		done := make(chan struct{})
		defer close(done)
		go s.watchStop(done)
	}
	start, err := procident.StartOf(s.self)
	if err != nil {
		return s.fail("the supervisor cannot identify itself: %v", err)
	}
	s.identity = execwrap.Wrapper{PID: s.self, PGID: s.pgid, Start: start, StartedAt: s.clock.Now()}
	switch claimed, err := s.claimIdentity(); {
	case err != nil:
		return err
	case !claimed:
		// Another supervisor of this Run registered first: it is the one the daemon
		// knows, and this one runs nothing and writes nothing.
		return errors.New("envwrap: another supervisor holds the identity of this Run")
	}
	switch stopped, err := s.waitForGate(); {
	case err != nil:
		return s.fail("%v", err)
	case stopped:
		return s.finish(CauseStop, "")
	}
	s.ran = true
	if s.launch.Recover {
		return s.recover()
	}

	var prepared Prepared
	hasPrepared, err := execwrap.Read(s.o.RunDir, PreparedFile, &prepared)
	if err != nil {
		return s.fail("%v", err)
	}
	if hasPrepared && prepared.Status == StatusFailed {
		return s.finish(CausePrepareFailed, "")
	}
	if err := s.resolveEnv(); err != nil {
		if hasPrepared {
			return s.fail("%v", err)
		}
		// Nothing runs whose output would not be redacted: that is prepare failing.
		if !s.write(PreparedFile, Prepared{Status: StatusFailed, Message: bound(err.Error()), At: s.clock.Now()}) {
			return s.finish(CauseError, "")
		}
		return s.finish(CausePrepareFailed, "")
	}
	if err := s.openOutput(); err != nil {
		return s.fail("%v", err)
	}
	defer func() { _ = s.log.Close() }()
	if err := s.loadStarted(); err != nil {
		return s.fail("%v", err)
	}

	if !hasPrepared {
		if s.stopOrdered() {
			return s.finish(CauseStop, "")
		}
		if cause := s.prepare(); cause != "" {
			return s.finish(cause, "")
		}
	}
	if cause := s.startServices(); cause != "" {
		return s.finish(cause, "")
	}
	<-s.stop
	return s.finish(CauseStop, "")
}

// resolveEnv resolves the secrets, as the wrapper of the harness does, and
// builds the environment of every script.
func (s *supervisor) resolveEnv() error {
	s.env = envutil.WithoutTmux(os.Environ())
	if len(s.launch.SecretEnv) == 0 && !s.launch.RedactOutput {
		return nil
	}
	if err := execwrap.CheckSecretNames(s.launch.SecretEnv); err != nil {
		return err
	}
	set, err := execwrap.Redaction(s.o.Retention)
	if err != nil {
		return err
	}
	secrets, err := execwrap.ResolveSecrets(s.launch.SecretEnv, s.o.Secrets, set)
	if err != nil {
		return err
	}
	s.set = set
	s.env = append(s.env, secrets...)
	return nil
}

// openOutput opens the log and wires the redaction of what the scripts write.
func (s *supervisor) openOutput() error {
	log, err := os.OpenFile(s.path(OutputLog), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600) //nolint:gosec // inside the run dir
	if err != nil {
		return fmt.Errorf("opening the output log: %w", err)
	}
	s.log, s.capture = log, newCapture(log, s.set, s.clock)
	return nil
}

// write stores a marker; if it cannot, the supervisor ends with that as its error.
func (s *supervisor) write(name string, v any) bool {
	if err := execwrap.Write(s.o.RunDir, name, v); err != nil {
		s.problem = err.Error()
		return false
	}
	return true
}

// bound is a message safe to leave in a marker: redacted by shape and cut to
// its end, where the cause is.
func bound(msg string) string {
	return redact.Tail(strings.TrimSpace(redact.Text(msg)), MaxMessage)
}

// fail ends a supervisor that could not do its job, saying why in env-exit.json.
// Before the gate it only writes env-exit.json: it signals nothing, as the
// group is not known to be this process' own.
func (s *supervisor) fail(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	return errors.Join(errors.New("envwrap: "+msg), s.finish(CauseError, msg))
}

// claimIdentity publishes who this supervisor is, unless another supervisor
// already did: the file is created whole and linked into place, so that it is
// either absent, the claim of another, or complete. claimed is false when it
// belongs to another process.
func (s *supervisor) claimIdentity() (claimed bool, err error) {
	raw, err := json.Marshal(s.identity)
	if err != nil {
		return false, fmt.Errorf("envwrap: encoding the identity: %w", err)
	}
	tmp, err := os.CreateTemp(s.o.RunDir, WrapperFile+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("envwrap: claiming the identity: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.Write(raw)
	if serr := tmp.Sync(); werr == nil {
		werr = serr
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false, fmt.Errorf("envwrap: claiming the identity: %w", werr)
	}
	switch err := os.Link(tmp.Name(), s.path(WrapperFile)); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrExist):
		return false, nil
	default:
		return false, fmt.Errorf("envwrap: claiming the identity: %w", err)
	}
}

// holdsIdentity says whether env-wrapper.json is still this supervisor's.
func (s *supervisor) holdsIdentity() bool {
	var w execwrap.Wrapper
	ok, err := execwrap.Read(s.o.RunDir, WrapperFile, &w)
	return err == nil && ok && w.PID == s.identity.PID && w.Start == s.identity.Start
}

// identityClaimed: some supervisor registered itself for this Run.
func (s *supervisor) identityClaimed() bool {
	_, err := os.Lstat(s.path(WrapperFile))
	return err == nil
}

// errNotAuthorized: the gate names another process or another Run.
var errNotAuthorized = errors.New("the authorization is not for this supervisor")

// waitForGate waits until the daemon authorizes this supervisor, by name. stopped
// is true if a stop was ordered first.
func (s *supervisor) waitForGate() (stopped bool, err error) {
	ticker := s.clock.NewTicker(poll)
	defer ticker.Stop()
	limit := s.timeout(s.o.GateTimeout, GateTimeout)
	timer := s.clock.NewTimer(limit)
	defer timer.Stop()
	for {
		var a Authorized
		switch ok, rerr := execwrap.Read(s.o.RunDir, AuthorizedFile, &a); {
		case rerr != nil:
			return false, rerr
		case ok && !a.AuthorizesWrapper(s.launch.RunID, s.launch.LeaseEpoch, s.identity):
			return false, errNotAuthorized
		case ok:
			return s.stopOrdered(), nil
		}
		if s.stopOrdered() {
			return true, nil
		}
		select {
		case <-s.stop:
			return true, nil
		case <-timer.C():
			return false, fmt.Errorf("the daemon did not authorize after %s", limit)
		case <-ticker.C():
		}
	}
}

// prepare runs the prepare script and records how it went. It returns a cause
// to end with, or "" when the services are next.
func (s *supervisor) prepare() string {
	mark := s.logSize()
	res := s.runScript(s.launch.Prepare, s.timeout(s.launch.PrepareTimeout, DefaultPrepareTimeout), s.stop)
	if res.end == endStopped {
		return CauseStop // a stop is not a failure of prepare, and leaves no marker
	}
	msg := res.failure("prepare")
	found, survivors := s.settleGroup()
	s.remnants += found
	if survivors {
		s.survivors = true
		if msg == "" {
			msg = "prepare"
		}
		msg += " left processes that did not end after the grace"
	}
	if msg != "" {
		if tail := s.tailSince(mark); tail != "" {
			msg += ": " + tail
		}
		if !s.write(PreparedFile, Prepared{Status: StatusFailed, Message: bound(msg), At: s.clock.Now()}) {
			return CauseError
		}
		return CausePrepareFailed
	}
	if !s.write(PreparedFile, Prepared{Status: StatusOK, At: s.clock.Now()}) {
		return CauseError
	}
	return ""
}

// startServices starts each service in order, waits for it to be ready and
// records the outcome. It returns a cause to end with, or "" when all are ready.
func (s *supervisor) startServices() string {
	var ready []string
	for _, svc := range s.launch.Services {
		if s.stopOrdered() {
			return CauseStop // checked before every spawn; what was spawned as the order landed is stopped with the rest
		}
		if !s.noteStarted(svc) {
			return CauseError // without the record, a lost supervisor would leave this service unstopped
		}
		p, err := s.spawn(svc.Start)
		if err != nil {
			return s.servicesFailed(fmt.Sprintf("service %s: %v", svc.Name, err))
		}
		switch outcome, msg := s.awaitReady(svc, p); outcome {
		case endStopped:
			return CauseStop
		case endReady:
			ready = append(ready, svc.Name)
		default:
			return s.servicesFailed(fmt.Sprintf("service %s: %s", svc.Name, msg))
		}
	}
	if !s.write(ServicesFile, Services{Status: StatusOK, Ready: ready, At: s.clock.Now()}) {
		return CauseError
	}
	return ""
}

// noteStarted records, durably and before it is spawned, that the start of svc
// is about to run: the record may say a service started that did not, never
// the opposite.
func (s *supervisor) noteStarted(svc domain.EnvironmentService) bool {
	if slices.ContainsFunc(s.spawned, func(o domain.EnvironmentService) bool { return o.Name == svc.Name }) {
		return true
	}
	s.spawned = append(s.spawned, svc)
	names := make([]string, len(s.spawned))
	for i, o := range s.spawned {
		names[i] = o.Name
	}
	return s.write(StartedFile, Started{Services: names})
}

// loadStarted takes the services a supervisor before this one recorded as
// started: their stop is this one's to run too.
func (s *supervisor) loadStarted() error {
	var st Started
	if _, err := execwrap.Read(s.o.RunDir, StartedFile, &st); err != nil {
		return err
	}
	for _, name := range st.Services {
		if i := slices.IndexFunc(s.launch.Services, func(o domain.EnvironmentService) bool { return o.Name == name }); i >= 0 {
			s.spawned = append(s.spawned, s.launch.Services[i])
		}
	}
	return nil
}

func (s *supervisor) servicesFailed(msg string) string {
	if !s.write(ServicesFile, Services{Status: StatusFailed, Message: bound(msg), At: s.clock.Now()}) {
		return CauseError
	}
	return CauseServicesFailed
}

// awaitReady decides when a service is ready: endReady, or endStopped for a
// stop; anything else is a failure and msg says why. A service without a ready script is ready
// once spawned, unless it has already died of an error. A start that exited 0
// is detached: allowed, and ready decides.
func (s *supervisor) awaitReady(svc domain.EnvironmentService, p *proc) (end, string) {
	if svc.Ready == "" {
		// Nothing says when it is ready: a start that fails within startupCheck
		// is a failure, and one that is still running by then is taken as up.
		timer := s.clock.NewTimer(startupCheck)
		defer timer.Stop()
		select {
		case <-p.waited:
			if code := p.code(); code != 0 {
				return endFailed, fmt.Sprintf("start exited with code %d", code)
			}
		case <-timer.C():
		case <-s.stop:
			return endStopped, ""
		}
		return endReady, ""
	}
	limit := s.timeout(s.launch.ReadyTimeout, DefaultReadyTimeout)
	deadline := s.clock.NewTimer(limit)
	defer deadline.Stop()
	ticker := s.clock.NewTicker(s.timeout(s.o.ReadyInterval, readyInterval))
	defer ticker.Stop()
	svcDied := p.waited // nil once the start exited 0: detached
	for {
		probe, err := s.spawn(svc.Ready)
		if err != nil {
			return endFailed, fmt.Sprintf("ready: %v", err)
		}
		switch s.await(probe, deadline.C(), s.stop, svcDied) {
		case endExited:
			if probe.code() == 0 {
				return endReady, ""
			}
		case endStopped:
			probe.kill()
			return endStopped, ""
		case endTimeout:
			probe.kill()
			return endTimeout, fmt.Sprintf("not ready after %s", limit)
		case endOther:
			probe.kill()
			if code := p.code(); code != 0 {
				return endFailed, fmt.Sprintf("start exited with code %d before it was ready", code)
			}
			svcDied = nil
			continue
		}
		select { // the probe said not yet
		case <-ticker.C():
		case <-deadline.C():
			return endTimeout, fmt.Sprintf("not ready after %s", limit)
		case <-s.stop:
			return endStopped, ""
		case <-svcDied:
			if code := p.code(); code != 0 {
				return endFailed, fmt.Sprintf("start exited with code %d before it was ready", code)
			}
			svcDied = nil
		}
	}
}

// finish ends the supervisor: it stops the services that were started, in
// reverse order, leaves nothing of the group alive but itself, and writes
// env-exit.json. A stop that fails is recorded and does not hold anything back.
func (s *supervisor) finish(cause, msg string) error {
	if s.identity.PID != 0 && !s.holdsIdentity() {
		// The identity of this Run is another supervisor's, whose files these are.
		return errNotAuthorized
	}
	if s.identity.PID == 0 && s.identityClaimed() {
		// Failed before claiming: if another supervisor already did, the files are its.
		return errNotAuthorized
	}
	if msg == "" {
		msg = s.problem
	}
	exit := Exit{Cause: cause, Error: msg}
	for i := len(s.spawned) - 1; i >= 0; i-- {
		svc := s.spawned[i]
		switch {
		case svc.Stop == "":
		case s.lostStops:
			exit.StopFailures = append(exit.StopFailures, StopFailure{Service: svc.Name, Error: lostStopError})
		default:
			if f := s.runStop(svc); f != nil {
				exit.StopFailures = append(exit.StopFailures, *f)
			}
		}
	}
	if s.ran && !s.survivors {
		found, survivors := s.settleGroup()
		s.remnants += found
		s.survivors = survivors
	}
	exit.RemnantsKilled, exit.GroupKilled = s.remnants, s.survivors
	var errs []error
	if s.capture != nil {
		if err := s.capture.drainAll(); err != nil {
			exit.Error = strings.TrimSpace(exit.Error + " " + err.Error())
		}
		errs = append(errs, s.log.Sync())
	}
	exit.Error = bound(exit.Error)
	exit.ExitedAt = s.clock.Now()
	errs = append(errs, execwrap.Write(s.o.RunDir, ExitFile, exit))
	if s.ran && s.survivors {
		// As the last thing this process does, so that the daemon finds the
		// file and finds the supervisor gone. Killed even if the file failed.
		execwrap.KillGroup(s.pgid)
	}
	return errors.Join(errs...)
}

// runStop runs the stop script of a service, with the same environment as the
// scripts that started it.
func (s *supervisor) runStop(svc domain.EnvironmentService) *StopFailure {
	mark := s.logSize()
	res := s.runScript(svc.Stop, s.timeout(s.launch.StopTimeout, DefaultStopTimeout), nil)
	msg := res.failure("stop")
	if msg == "" {
		return nil
	}
	if tail := s.tailSince(mark); tail != "" {
		msg += ": " + tail
	}
	return &StopFailure{Service: svc.Name, Error: bound(msg)}
}

func (s *supervisor) grace() time.Duration { return s.timeout(s.launch.Grace, DefaultGrace) }

// settleGroup asks the group to end if anything but this process is in it, and
// gives it the grace. Nothing is signalled by pid (see execwrap): only the
// group, whose ID this process keeps reserved by being alive.
func (s *supervisor) settleGroup() (found int, survivors bool) {
	found = execwrap.GroupOthers(s.pgid, s.self)
	if found == 0 {
		return 0, false
	}
	// This process is in the group: ignoring SIGTERM keeps its own copy of the
	// signal from being taken for a stop order. It listens again after.
	signal.Ignore(syscall.SIGTERM)
	defer signal.Notify(s.signals, syscall.SIGTERM)
	_ = syscall.Kill(-s.pgid, syscall.SIGTERM)
	ticker := s.clock.NewTicker(poll)
	defer ticker.Stop()
	timer := s.clock.NewTimer(s.grace())
	defer timer.Stop()
	for execwrap.GroupOthers(s.pgid, s.self) > 0 {
		select {
		case <-timer.C():
			return found, execwrap.GroupOthers(s.pgid, s.self) > 0
		case <-ticker.C():
		}
	}
	return found, false
}

// logSize is where the next script's output starts in the log.
func (s *supervisor) logSize() int64 {
	if s.log == nil {
		return 0
	}
	fi, err := s.log.Stat()
	if err != nil {
		return 0
	}
	return fi.Size()
}

// tailSince is the end of what the log got since mark, already redacted by the
// capture, cut to what a marker may hold.
func (s *supervisor) tailSince(mark int64) string {
	if s.log == nil {
		return ""
	}
	size := s.logSize()
	from := max(mark, size-MaxMessage)
	if size <= from {
		return ""
	}
	buf := make([]byte, size-from)
	n, err := s.log.ReadAt(buf, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return strings.TrimSpace(string(buf[:n]))
}

// spawn starts a script as a child of this process, in its group.
func (s *supervisor) spawn(script string) (*proc, error) {
	cmd := exec.Command("/bin/sh", "-c", script) //nolint:gosec,noctx // the scripts are what the repo's configuration asks for; the supervisor ends them
	cmd.Dir = s.launch.Dir
	cmd.Env = s.env
	var pp *pipes
	if s.capture != nil {
		var err error
		if pp, err = s.capture.attach(cmd); err != nil {
			return nil, fmt.Errorf("wiring the output: %w", err)
		}
	}
	err := cmd.Start()
	if pp != nil {
		pp.release()
	}
	if err != nil {
		if pp != nil {
			_ = pp.drain()
		}
		return nil, fmt.Errorf("starting: %w", err)
	}
	p := &proc{cmd: cmd, pipes: pp, waited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.waited)
	}()
	return p, nil
}

// proc is a script that was spawned.
type proc struct {
	cmd    *exec.Cmd
	pipes  *pipes
	waited chan struct{}
}

// code is the exit code once waited is closed; -1 for a signal.
func (p *proc) code() int { return p.cmd.ProcessState.ExitCode() }

// kill ends the script itself (we hold its handle, so the pid is ours) and
// waits for it. What it left behind is for the group to settle.
func (p *proc) kill() {
	_ = p.cmd.Process.Kill()
	<-p.waited
	if p.pipes != nil {
		_ = p.pipes.drain()
	}
}

// end is how waiting for a script ended.
type end int

const (
	endExited  end = iota // it ended
	endReady              // (awaitReady only) the service is ready
	endTimeout            // its time ran out
	endStopped            // a stop was ordered
	endOther              // the extra channel closed
	endFailed             // (awaitReady only) it failed; the message says how
	endSpawn              // (runScript only) it could not be spawned
)

// await waits for p to end, for deadline, for the stop or for other.
func (s *supervisor) await(p *proc, deadline <-chan time.Time, stop, other <-chan struct{}) end {
	select {
	case <-p.waited:
		if p.pipes != nil {
			_ = p.pipes.drain()
		}
		return endExited
	case <-deadline:
		return endTimeout
	case <-stop:
		return endStopped
	case <-other:
		return endOther
	}
}

// result is how a script ended.
type result struct {
	end     end
	code    int
	limit   time.Duration
	spawnEr error
}

// failure is why the script failed, or "" if it did not. A stop is not one.
func (r result) failure(what string) string {
	switch r.end {
	case endSpawn:
		return fmt.Sprintf("%s could not start: %v", what, r.spawnEr)
	case endTimeout:
		return fmt.Sprintf("%s timed out after %s", what, r.limit)
	case endExited:
		if r.code != 0 {
			return fmt.Sprintf("%s exited with code %d", what, r.code)
		}
	}
	return ""
}

// runScript spawns script and waits for it up to limit of the clock, or for
// stop, which may be nil. A script that outlasts its time or a stop is killed.
func (s *supervisor) runScript(script string, limit time.Duration, stop <-chan struct{}) result {
	p, err := s.spawn(script)
	if err != nil {
		return result{end: endSpawn, spawnEr: err}
	}
	timer := s.clock.NewTimer(limit)
	defer timer.Stop()
	e := s.await(p, timer.C(), stop, nil)
	if e != endExited {
		p.kill()
		return result{end: e, limit: limit}
	}
	return result{end: e, code: p.code(), limit: limit}
}

// lostStopError is the failure of a stop that a recovery supervisor could not attempt.
const lostStopError = "stop not attempted: supervisor lost"

// recover is what a recovery supervisor (Launch.Recover) does once the daemon
// authorized it: the stops of the services a lost supervisor recorded as started
// (env-started.json), in reverse order, as finish does for any supervisor, and
// env-exit.json with CauseLost. Prepare and start never run again. The scripts
// get the environment the lost supervisor's had if the secrets can still be
// resolved; if they cannot (the harness started, so the access was revoked), no
// stop is attempted and each is recorded as a failure.
func (s *supervisor) recover() error {
	if err := s.loadStarted(); err != nil {
		return s.fail("%v", err)
	}
	if err := s.resolveEnv(); err != nil {
		s.lostStops = true
		return s.finish(CauseLost, "")
	}
	if err := s.openOutput(); err != nil {
		return s.fail("%v", err)
	}
	defer func() { _ = s.log.Close() }()
	return s.finish(CauseLost, "")
}
