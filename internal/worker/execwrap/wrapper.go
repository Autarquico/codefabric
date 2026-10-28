package execwrap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/envutil"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// Timings the wrapper uses, all on its clock.
const (
	// GateTimeout is how long the wrapper waits for the Runtime to wire the terminal.
	GateTimeout  = 30 * time.Second
	defaultGrace = 30 * time.Second
	// defaultDrainWait is how long the kill path lets the copies drain what the
	// survivors still write before it cuts them.
	defaultDrainWait = 2 * time.Second
	poll             = 50 * time.Millisecond
)

// Options parameterise Run. Stdin, Stdout and Stderr are the terminal of the pane.
type Options struct {
	RunDir string
	Clock  clock.Clock // defaults to clock.Real
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
	// Secrets resolves the SecretEnv of the launch: the harness's access
	// (secretjit.Resolver). Nil resolves nothing: a launch with any SecretEnv
	// then fails before the harness starts.
	Secrets ResolveSecret
	// Retention is what the output of a launch with SecretEnv is redacted of
	// (secretjit.Load). Without it such a launch fails before the harness starts.
	Retention LoadSecrets
	// GateTimeout is how long the wrapper waits at the gate for the Runtime to
	// wire the terminal, which it does once the capture filter acknowledged.
	// Zero is GateTimeout.
	GateTimeout time.Duration

	// openEvents opens events.jsonl; tests inject a sink that fails. Nil opens the file.
	openEvents func(path string) (eventsSink, error)
}

// eventsSink is where events.jsonl goes. wire's drain syncs and closes it,
// exactly once, and nothing else touches it.
type eventsSink interface {
	io.Writer
	Sync() error
	Close() error
}

func openEventsFile(path string) (eventsSink, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // inside the run dir
}

// ResolveSecret is the value of the secret ref names.
type ResolveSecret func(ref string) (string, error)

// Run is the wrapper (§6.4). It must lead its own process group, which it does
// in a tmux pane, where the process of the pane leads its session: the harness
// and everything it starts stay in that group, so the group is known before
// the harness exists and there is no moment in which the harness runs
// unregistered. SIGTERM, SIGINT and SIGHUP do not end the wrapper: they ask it
// to stop the group, which it does by asking, waiting stop.grace and killing.
// The wrapper is in the group it kills, so it kills the others by pid and
// stays alive to say so.
//
// It returns once exit.json is written. The error is for what stopped the
// wrapper from writing it, or for a launch that could not happen, which exit.json
// then says.
func Run(o Options) error {
	clk := o.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	w := &wrapper{o: o, clock: clk, self: os.Getpid(), pgid: syscall.Getpgrp(), signals: make(chan os.Signal, 8)}
	defer func() {
		if w.timer != nil {
			w.timer.Stop()
		}
	}()
	signal.Notify(w.signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(w.signals)
	return w.run()
}

type wrapper struct {
	o       Options
	clock   clock.Clock
	self    int
	pgid    int
	signals chan os.Signal

	launch   Launch
	stopping bool
	// copyErr is what failed in copying the output and in syncing and closing
	// events.jsonl, set before wire's channel closes: a capture that failed is
	// not a complete one. Read it only once that channel is closed.
	copyErr error
	// streams are the copies of the output of the harness, one per pipe, so that
	// the kill path can tell which of them ended and which did not.
	streams []copyPipe
	// drainWait bounds, in real time, how long the kill path waits for the copies
	// to drain before it cuts them; zero is defaultDrainWait.
	drainWait time.Duration

	deadline <-chan time.Time // its end, nil when not armed
	timer    clock.Timer
}

func (w *wrapper) grace() time.Duration {
	if w.launch.Grace > 0 {
		return w.launch.Grace
	}
	return defaultGrace
}

// fail leaves an exit.json that says why nothing ran, and returns the same cause.
func (w *wrapper) fail(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	return errors.Join(errors.New("execwrap: "+msg),
		Write(w.o.RunDir, ExitFile, Exit{ExitedAt: w.clock.Now(), Cause: CauseExit, Error: msg}))
}

func (w *wrapper) run() error {
	if ok, err := Read(w.o.RunDir, LaunchFile, &w.launch); err != nil || !ok || len(w.launch.Argv) == 0 {
		return w.fail("the launch file is missing or empty")
	}
	if w.pgid != w.self {
		// Signalling the group of a wrapper that does not lead it would reach
		// processes that are not ours.
		return w.fail("the wrapper does not lead its process group")
	}
	start, err := procident.StartOf(w.self)
	if err != nil {
		// The daemon signals this group only if it can tell this process from another
		// that gets the pid: a wrapper it could not identify must not run anything.
		return w.fail("the wrapper cannot identify itself: %v", err)
	}
	if err := Write(w.o.RunDir, WrapperFile, Wrapper{PID: w.self, PGID: w.pgid, Start: start, StartedAt: w.clock.Now()}); err != nil {
		return err
	}
	switch stopped, err := w.waitForGate(); {
	case err != nil:
		return w.fail("%v", err)
	case stopped:
		return Write(w.o.RunDir, ExitFile, Exit{ExitedAt: w.clock.Now(), Cause: CauseStop, Error: "stopped before the harness started"})
	}

	secrets, set, err := w.secretEnv()
	if err != nil {
		return w.fail("%v", err)
	}
	log, err := os.OpenFile(filepath.Join(w.o.RunDir, OutputLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return w.fail("opening the output log: %v", err)
	}
	defer func() { _ = log.Close() }()

	cmd := exec.Command(w.launch.Argv[0], w.launch.Argv[1:]...) //nolint:gosec,noctx // the argv is what the Run asked for; nothing ends the harness but the wrapper
	cmd.Dir = w.launch.Dir
	cmd.Env = append(append(envutil.WithoutTmux(os.Environ()), w.launch.Env...), secrets...)
	copied, parentEnds, err := w.wire(cmd, log, set)
	if err != nil {
		return w.fail("wiring the output: %v", err)
	}
	err = cmd.Start()
	for _, f := range parentEnds {
		_ = f.Close() // the harness has its own copy; ours would keep the pipe open
	}
	if err != nil {
		<-copied
		return w.fail("starting the harness: %v", err)
	}
	if err := Write(w.o.RunDir, StartedFile, Started{
		PID: cmd.Process.Pid, PGID: w.pgid, WrapperPID: w.self, Argv: w.launch.Argv, StartedAt: w.clock.Now(),
	}); err != nil {
		err = w.fail("recording the start of the harness: %v", err)
		w.killGroup() // the harness runs unrecorded: it goes, and so does this process
		return err
	}

	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	exit := Exit{Cause: CauseExit}
	if !w.supervise(waited) {
		// The harness ignored the stop for the whole grace: it is killed with the
		// group, and says so.
		return w.endByKillingGroup(log, copied, Exit{Cause: CauseStop, Signal: "SIGKILL", GroupKilled: true, RemnantsKilled: w.count()})
	}
	exit.ExitCode, exit.Signal = outcome(cmd.ProcessState)
	if !w.stopping && w.diedOfAStop(cmd.ProcessState) {
		// A stop to the group reaches the harness and this process at once: the
		// harness can be reaped before this process has taken the signal. It is
		// on its way; it is waited for, briefly, so that the stop is not taken
		// for an exit of the harness.
		w.awaitStop()
	}
	if w.stopping {
		exit.Cause = CauseStop
	}
	found, survivors := w.settleGroup()
	exit.RemnantsKilled = found
	if survivors {
		exit.GroupKilled = true
		return w.endByKillingGroup(log, copied, exit)
	}
	return w.finish(log, copied, exit)
}

// finish writes the exit of a group that is gone, every writer with it and the
// pipe at its end. A capture that failed is the exit's failure.
func (w *wrapper) finish(log *os.File, copied <-chan struct{}, exit Exit) error {
	<-copied
	if err := errors.Join(w.copyErr, log.Sync()); err != nil {
		return w.fail("capturing the output: %v", err)
	}
	exit.ExitedAt = w.clock.Now()
	return Write(w.o.RunDir, ExitFile, exit)
}

// envName is the name of an environment variable a secret may take: no '=',
// nothing a shell would not take as a name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// secretEnv is the environment of the SecretEnv of the launch, resolved now,
// just before the harness starts, so that their values live only in its
// environment, and set, what its output is redacted of. A launch without
// SecretEnv and without RedactOutput has neither, and runs as it always did.
//
// Nothing runs whose output would not be redacted: in headless mode this
// process redacts it, and in interactive mode the capture filter of the
// Runtime must have acknowledged that it does. Nor is a value given to the
// harness that set would not redact.
func (w *wrapper) secretEnv() (env []string, set *redact.Set, err error) {
	if len(w.launch.SecretEnv) == 0 && !w.launch.RedactOutput {
		return nil, nil, nil
	}
	for _, s := range w.launch.SecretEnv {
		if !envName.MatchString(s.Name) {
			return nil, nil, fmt.Errorf("%s: the secret %q goes to %q, which is not the name of a variable", domain.CodeSecretUnavailable, s.Ref, s.Name)
		}
	}
	if set, err = redaction(w.o.Retention); err != nil {
		return nil, nil, fmt.Errorf("%s: the output cannot be redacted: %w", domain.CodeSecretUnavailable, err)
	}
	if w.launch.Mode == ModeInteractive {
		if acked, err := cfruntime.CaptureAcked(filepath.Join(w.o.RunDir, CaptureAckFile)); !acked {
			if err == nil {
				err = errors.New("its filter never acknowledged")
			}
			return nil, nil, fmt.Errorf("%s: the capture of the terminal does not redact the output: %w", domain.CodeSecretUnavailable, err)
		}
	}
	for _, s := range w.launch.SecretEnv {
		if w.o.Secrets == nil {
			return nil, nil, fmt.Errorf("%s: the secret %q of %s cannot be resolved: this Worker has no secret store", domain.CodeSecretUnavailable, s.Ref, s.Name)
		}
		value, err := w.o.Secrets(s.Ref)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: the secret %q of %s cannot be resolved: %w", domain.CodeSecretUnavailable, s.Ref, s.Name, err)
		}
		if bytes.Contains(redact.Bytes([]byte(value), set), []byte(value)) {
			return nil, nil, fmt.Errorf("%s: the secret %q of %s would not be redacted from the output", domain.CodeSecretUnavailable, s.Ref, s.Name)
		}
		env = append(env, s.Name+"="+value)
	}
	return env, set, nil
}

// structured says whether stdout goes apart, to events.jsonl: only in headless mode.
func (w *wrapper) structured() bool {
	return w.launch.StructuredStdout && w.launch.Mode != ModeInteractive
}

// wire connects the harness to the terminal and the log. In interactive mode
// it uses the terminal of the pane itself, whose output the Runtime captures; in
// headless mode it has no input and its output goes to the log and the pane,
// with set, if any, redacted from both. With StructuredStdout, stdout goes
// instead to events.jsonl (alone, redacted line by line with set), and
// only stderr to the log and the pane. copied closes when every stream has been
// copied to its end and events.jsonl is synced and closed; parentEnds are the files of the pipe that this process
// must close once the harness has started.
func (w *wrapper) wire(cmd *exec.Cmd, log *os.File, set *redact.Set) (copied <-chan struct{}, parentEnds []*os.File, err error) {
	done := make(chan struct{})
	if w.launch.Mode == ModeInteractive {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = w.o.Stdin, w.o.Stdout, w.o.Stderr
		close(done)
		return done, nil, nil
	}
	var sink io.Writer = log
	if w.o.Stdout != nil {
		sink = io.MultiWriter(log, w.o.Stdout)
	}
	logPipe, err := startCopy("output log", sink, func(dst io.Writer, src io.Reader) error {
		if set == nil {
			_, err := io.Copy(dst, src)
			return err
		}
		red := redact.NewWriter(dst, set)
		if _, err := io.Copy(red, src); err != nil {
			// A cut: what the redactor holds may be the start of a secret
			// whose rest never came. It is dropped, never written.
			_, _ = io.WriteString(dst, captureCutMarker)
			return err
		}
		return red.Close() // the stream ended: what it held back, before the log is synced
	})
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logPipe.w, logPipe.w
	pipes := []copyPipe{logPipe}
	var events eventsSink // set when stdout goes apart
	if w.structured() {
		open := w.o.openEvents
		if open == nil {
			open = openEventsFile
		}
		var err error
		events, err = open(filepath.Join(w.o.RunDir, EventsFile))
		if err != nil {
			_ = logPipe.abort()
			return nil, nil, err
		}
		var eventsPipe copyPipe
		eventsPipe, err = startCopy("events.jsonl", events, func(dst io.Writer, src io.Reader) error {
			if set == nil {
				_, err := io.Copy(dst, src)
				return err
			}
			return copyEvents(dst, src, set)
		})
		if err != nil {
			_ = logPipe.abort()
			_ = events.Close()
			return nil, nil, err
		}
		cmd.Stdout = eventsPipe.w
		pipes = append(pipes, eventsPipe)
	}
	w.streams = pipes
	go func() {
		defer close(done)
		var errs []error
		for _, p := range pipes {
			<-p.done
			errs = append(errs, p.err())
		}
		if events != nil {
			errs = append(errs, events.Sync(), events.Close())
		}
		w.copyErr = errors.Join(errs...)
	}()
	for _, p := range pipes {
		parentEnds = append(parentEnds, p.w)
	}
	return done, parentEnds, nil
}

// copyPipe is a pipe whose read end is being copied to a destination; w is the
// end the harness writes to and done closes when the copy has reached the end.
type copyPipe struct {
	name string
	w    *os.File
	done chan struct{}
	r    *os.File
	res  *error // the result of the copy, valid once done is closed
}

// ended says whether the copy has reached its end, and so res is valid.
func (p copyPipe) ended() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// cut closes the read end: the copy gets an error instead of waiting for a
// writer that does not let go. What it held back (the redactor's tail, the last
// line without a newline) is dropped, and a marker without content is written in
// its place before it ends.
func (p copyPipe) cut() { _ = p.r.Close() }

func (p copyPipe) err() error { return *p.res }

// captureCutMarker stands in the log for what a cut stream still held.
const captureCutMarker = "[capture cut]\n"

func startCopy(name string, dst io.Writer, copyAll func(dst io.Writer, src io.Reader) error) (copyPipe, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return copyPipe{}, err
	}
	p := copyPipe{name: name, w: pw, r: pr, done: make(chan struct{}), res: new(error)}
	go func() {
		defer close(p.done)
		*p.res = copyAll(dst, pr)
		_ = pr.Close()
	}()
	return p, nil
}

// abort closes both ends of a pipe that will not be used and waits for its copy.
func (p copyPipe) abort() error {
	err := p.w.Close()
	<-p.done
	return err
}

// waitForGate waits until the Runtime says that the terminal is wired. stopped
// is true if a stop came first.
func (w *wrapper) waitForGate() (stopped bool, err error) {
	if w.launch.Gate == "" {
		return false, nil
	}
	ticker := w.clock.NewTicker(poll)
	defer ticker.Stop()
	limit := w.o.GateTimeout
	if limit <= 0 {
		limit = GateTimeout
	}
	timeout := w.clock.NewTimer(limit)
	defer timeout.Stop()
	for {
		if _, err := os.Stat(w.launch.Gate); err == nil {
			return false, nil
		}
		select {
		case sig := <-w.signals:
			if w.stops(sig) {
				return true, nil
			}
		case <-timeout.C():
			return false, fmt.Errorf("the terminal was not ready after %s", limit)
		case <-ticker.C():
		}
	}
}

// supervise waits for the harness to end, starting the stop when asked to. It
// returns false if the grace of a stop ran out with the harness alive.
func (w *wrapper) supervise(waited <-chan struct{}) bool {
	for {
		select {
		case <-waited:
			return true
		case sig := <-w.signals:
			if w.stops(sig) {
				w.beginStop()
			}
		case <-w.deadline:
			w.deadline = nil
			return false
		}
	}
}

// stops says whether a signal asks the wrapper to stop. In interactive mode the
// harness has the terminal of the pane: a Ctrl-C of the user reaches it through
// the foreground group, and it is the harness's to handle, not a reason to end
// the Run. SIGTERM and SIGHUP (the session closing) always stop.
func (w *wrapper) stops(sig os.Signal) bool {
	return sig != syscall.SIGINT || w.launch.Mode != ModeInteractive
}

// diedOfAStop says whether the harness was killed by a signal that asks this
// process to stop too.
func (w *wrapper) diedOfAStop(ps *os.ProcessState) bool {
	status, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return false
	}
	switch sig := status.Signal(); sig {
	case syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT:
		return w.stops(sig)
	}
	return false
}

// stopSignalWait bounds how long a stop signal that killed the harness takes to
// reach this process too; one sent to someone else only is not waited for longer.
const stopSignalWait = time.Second

// awaitStop takes a stop signal that is on its way, if one comes within
// stopSignalWait, and begins the stop with it.
func (w *wrapper) awaitStop() {
	timer := w.clock.NewTimer(stopSignalWait)
	defer timer.Stop()
	for {
		select {
		case sig := <-w.signals:
			if w.stops(sig) {
				w.beginStop()
				return
			}
		case <-timer.C():
			return
		}
	}
}

// beginStop asks the group to end and starts the grace.
func (w *wrapper) beginStop() {
	if w.stopping {
		return
	}
	w.stopping = true
	_ = syscall.Kill(-w.pgid, syscall.SIGTERM) // this process catches it too
	w.arm()
}

func (w *wrapper) arm() {
	w.timer = w.clock.NewTimer(w.grace())
	w.deadline = w.timer.C()
}

// settleGroup is what happens to the group once the harness exited. If other
// processes are left, the group is asked to end and given the grace; it returns
// how many were found and whether some are still there when the grace is over.
// Nothing is signalled by pid: a pid that was listed can end, be reaped and be
// reused outside the group, and only a signal to the group, sent while this
// process is alive and so keeps the group's ID, cannot reach a stranger.
func (w *wrapper) settleGroup() (found int, survivors bool) {
	found = w.count()
	if found == 0 {
		return 0, false
	}
	if !w.stopping && w.deadline == nil {
		_ = syscall.Kill(-w.pgid, syscall.SIGTERM) // this process catches it too
		w.arm()
	}
	ticker := w.clock.NewTicker(poll)
	defer ticker.Stop()
	for w.count() > 0 {
		select {
		case <-w.deadline:
			w.deadline = nil
			return found, w.count() > 0
		case <-ticker.C():
		}
	}
	return found, false
}

// count is the number of live processes of the group but this one. It only
// counts: it is never used to choose whom to signal. If it cannot tell, it
// says 1, so that the group is asked to end rather than assumed empty.
func (w *wrapper) count() int {
	members, err := procident.GroupMembers(w.pgid)
	if err != nil {
		return 1
	}
	n := 0
	for _, pid := range members {
		if pid != w.self {
			n++
		}
	}
	return n
}

// endByKillingGroup writes the exit and then, as the last thing this process
// does, kills the whole group, itself included. The exit is on disk before the
// kill, so the daemon finds it and finds the wrapper gone. The group is killed
// even when the exit cannot be written: the daemon then settles a lost wrapper.
func (w *wrapper) endByKillingGroup(log *os.File, copied <-chan struct{}, exit Exit) error {
	if err := Write(w.o.RunDir, ExitFile, w.sealed(log, copied, exit)); err != nil {
		fmt.Fprintf(os.Stderr, "cf worker exec: writing the exit: %v; killing the group all the same\n", err)
	}
	w.killGroup()
	return nil
}

// sealed is exit with the time and what failed in the capture. Survivors may
// still hold the pipes, so the drain is waited for only so long (see drained):
// a capture that did not end by itself is cut, and exit.json says so and which
// stream it was. The log is synced after the drain and the cut, so that the
// marker of a cut is on disk too. A capture is never presented as complete when it is not.
func (w *wrapper) sealed(log *os.File, copied <-chan struct{}, exit Exit) Exit {
	exit.ExitedAt = w.clock.Now()
	drained := w.drained(copied)
	err := errors.Join(drained, log.Sync())
	if err != nil {
		exit.Error = strings.TrimSpace(exit.Error + " capturing the output: " + err.Error())
	}
	return exit
}

// drained is what failed in the capture, for an exit written while the group
// may still hold the pipes: it waits drainWait for the copies to end by
// themselves. The ones that did not are cut, which makes each drop what
// it held (it could be the start of a secret) and write a marker, and the error says the capture is incomplete and where. If a
// copy does not end even cut (its destination is stuck), the error says which
// streams ended and how, and that events.jsonl may not be synced: the exit is
// written all the same, because the wrapper has nothing else to wait for and
// is about to be killed with the group. The invariant "drain and close before
// exit.json" holds whenever the group lets go of the pipes; for a group that
// does not, this is the documented exception.
func (w *wrapper) drained(copied <-chan struct{}) error {
	wait := w.drainWait
	if wait <= 0 {
		wait = defaultDrainWait
	}
	if waitFor(copied, wait) {
		return w.copyErr
	}
	var held []string
	for _, p := range w.streams {
		if !p.ended() {
			held = append(held, p.name)
			p.cut()
		}
	}
	incomplete := errors.New("capture incomplete: events.jsonl was still being synced and closed")
	if len(held) > 0 {
		incomplete = fmt.Errorf("capture incomplete: the group still held open the %s", strings.Join(held, " and the "))
	}
	if waitFor(copied, wait) {
		if len(held) == 0 {
			return w.copyErr // every stream ended by itself; only the sync was slow
		}
		return errors.Join(incomplete, w.copyErr)
	}
	errs := []error{incomplete, errors.New("the copy of the output did not end after being cut; events.jsonl may not be synced")}
	for _, p := range w.streams {
		if p.ended() {
			errs = append(errs, p.err())
		}
	}
	return errors.Join(errs...)
}

// waitFor says whether ch closed within d of real time: it bounds I/O, not logic.
func waitFor(ch <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

// killGroup is SIGKILL to the group this process leads and keeps reserved by
// being alive. It is the only way this process kills anything.
func (w *wrapper) killGroup() { _ = syscall.Kill(-w.pgid, syscall.SIGKILL) }

// outcome reads how the harness ended: its exit code, or the signal that killed it.
func outcome(ps *os.ProcessState) (code *int, sig string) {
	if ps == nil {
		return nil, ""
	}
	status, ok := ps.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		return nil, signalName(status.Signal())
	}
	n := ps.ExitCode()
	return &n, ""
}

func signalName(s syscall.Signal) string {
	names := map[syscall.Signal]string{
		syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT", syscall.SIGABRT: "SIGABRT",
		syscall.SIGKILL: "SIGKILL", syscall.SIGSEGV: "SIGSEGV", syscall.SIGPIPE: "SIGPIPE", syscall.SIGTERM: "SIGTERM",
		syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE", syscall.SIGILL: "SIGILL",
	}
	if name, ok := names[s]; ok {
		return name
	}
	return fmt.Sprintf("SIG%d", int(s))
}
