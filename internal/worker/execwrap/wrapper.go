package execwrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// Timings the wrapper uses, all on its clock.
const (
	// GateTimeout is how long the wrapper waits for the Runtime to wire the terminal.
	GateTimeout  = 30 * time.Second
	defaultGrace = 30 * time.Second
	poll         = 50 * time.Millisecond
	// killRounds bounds how long a group that keeps forking is chased once the
	// grace is over: killRounds polls.
	killRounds = 200
)

// Options parameterise Run. Stdin, Stdout and Stderr are the terminal of the pane.
type Options struct {
	RunDir string
	Clock  clock.Clock // defaults to clock.Real
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
}

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
	expired  bool             // the grace of a stop (or of a clean-up) is over
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
	start, _ := procident.StartOf(w.self) // empty if the platform cannot say; the daemon then cannot verify
	if err := Write(w.o.RunDir, WrapperFile, Wrapper{PID: w.self, PGID: w.pgid, Start: start, StartedAt: w.clock.Now()}); err != nil {
		return err
	}
	switch stopped, err := w.waitForGate(); {
	case err != nil:
		return w.fail("%v", err)
	case stopped:
		w.stopping = true
		return Write(w.o.RunDir, ExitFile, Exit{ExitedAt: w.clock.Now(), Cause: CauseStop, Error: "stopped before the harness started"})
	}

	log, err := os.OpenFile(filepath.Join(w.o.RunDir, OutputLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return w.fail("opening the output log: %v", err)
	}
	defer func() { _ = log.Close() }()

	cmd := exec.CommandContext(context.Background(), w.launch.Argv[0], w.launch.Argv[1:]...) //nolint:gosec // the argv is what the Run asked for
	cmd.Dir = w.launch.Dir
	cmd.Env = append(os.Environ(), w.launch.Env...)
	copied, parentEnds, err := w.wire(cmd, log)
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
		w.killOthers()
		return err
	}

	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	w.supervise(waited)
	remnants, leftover := w.cleanup()
	<-copied // every writer is gone: the pipe is at its end
	if err := log.Sync(); err != nil {
		return w.fail("syncing the output log: %v", err)
	}

	exit := Exit{ExitedAt: w.clock.Now(), Cause: CauseExit, RemnantsKilled: remnants}
	if w.stopping {
		exit.Cause = CauseStop
	}
	if leftover != nil {
		exit.Error = leftover.Error()
	}
	exit.ExitCode, exit.Signal = outcome(cmd.ProcessState)
	return Write(w.o.RunDir, ExitFile, exit)
}

// wire connects the harness to the terminal and the log. In interactive mode
// it uses the terminal of the pane itself, whose output the Runtime captures; in
// headless mode it has no input and its output goes to the log and the pane.
// copied closes when the output has been copied to its end; parentEnds are the
// files of the pipe that this process must close once the harness has started.
func (w *wrapper) wire(cmd *exec.Cmd, log *os.File) (copied <-chan struct{}, parentEnds []*os.File, err error) {
	done := make(chan struct{})
	if w.launch.Mode == ModeInteractive {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = w.o.Stdin, w.o.Stdout, w.o.Stderr
		close(done)
		return done, nil, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, pw, pw
	var sink io.Writer = log
	if w.o.Stdout != nil {
		sink = io.MultiWriter(log, w.o.Stdout)
	}
	go func() {
		defer close(done)
		_, _ = io.Copy(sink, pr)
		_ = pr.Close()
	}()
	return done, []*os.File{pw}, nil
}

// waitForGate waits until the Runtime says that the terminal is wired. stopped
// is true if a stop came first.
func (w *wrapper) waitForGate() (stopped bool, err error) {
	if w.launch.Gate == "" {
		return false, nil
	}
	ticker := w.clock.NewTicker(poll)
	defer ticker.Stop()
	timeout := w.clock.NewTimer(GateTimeout)
	defer timeout.Stop()
	for {
		if _, err := os.Stat(w.launch.Gate); err == nil {
			return false, nil
		}
		select {
		case <-w.signals:
			return true, nil
		case <-timeout.C():
			return false, fmt.Errorf("the terminal was not ready after %s", GateTimeout)
		case <-ticker.C():
		}
	}
}

// supervise waits for the harness to end, starting the stop when asked to.
func (w *wrapper) supervise(waited <-chan struct{}) {
	for {
		select {
		case <-waited:
			return
		case <-w.signals:
			w.beginStop()
		case <-w.deadline:
			w.deadline, w.expired = nil, true
			w.killOthers()
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

// cleanup makes sure that nothing of the group is left but the wrapper, after
// the harness exited. What survives is asked to end and killed after the
// grace. It returns how many processes it found and, if some survive even
// SIGKILL for killRounds polls, an error that says so.
func (w *wrapper) cleanup() (found int, leftover error) {
	others, err := w.others()
	if err != nil {
		return 0, err
	}
	if len(others) == 0 {
		return 0, nil
	}
	found = len(others)
	if !w.stopping && !w.expired && w.deadline == nil {
		_ = syscall.Kill(-w.pgid, syscall.SIGTERM)
		w.arm()
	}
	ticker := w.clock.NewTicker(poll)
	defer ticker.Stop()
	for rounds := 0; ; {
		if others, err = w.others(); err != nil {
			return found, err
		}
		if len(others) == 0 {
			return found, nil
		}
		if w.expired {
			if rounds++; rounds > killRounds {
				return found, fmt.Errorf("%d processes of the group survive SIGKILL", len(others))
			}
			w.killOthers()
		}
		select {
		case <-w.deadline:
			w.deadline, w.expired = nil, true
		case <-ticker.C():
		}
	}
}

// others lists the live processes of the group but this one.
func (w *wrapper) others() ([]int, error) {
	members, err := procident.GroupMembers(w.pgid)
	if err != nil {
		return nil, err
	}
	out := members[:0]
	for _, pid := range members {
		if pid != w.self {
			out = append(out, pid)
		}
	}
	return out, nil
}

// killOthers kills every process of the group but this one, by pid: a signal to
// the group would kill the wrapper before it said what happened.
func (w *wrapper) killOthers() {
	others, err := w.others()
	if err != nil {
		return
	}
	for _, pid := range others {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

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
