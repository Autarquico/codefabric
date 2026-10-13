// Package procgroup runs a child as the leader of a process group of its own
// and ends the whole group with it.
//
// Every signal to the group goes while its leader is not reaped: the leader,
// alive or a zombie, keeps the ID of the group from being given to another
// process, so a signal can only reach the group. The leader is reaped last, and
// nothing is signalled after.
//
// The pipes of the child are this package's, not os/exec's: they are drained
// while the leader's zombie still holds the group, so a descendant that keeps
// them open can still be killed.
package procgroup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	// sweepFor bounds how long a group is swept after its kill.
	sweepFor = 3 * time.Second
	// sweepInterval is the pause between two kills of a sweep.
	sweepInterval = 10 * time.Millisecond
	// maxPoll is the longest pause between two looks at the leader.
	maxPoll = 20 * time.Millisecond
)

// leaderState is what a look at the leader says.
type leaderState int

const (
	unknown leaderState = iota // it could not be read: look again
	running
	ended // a zombie: it exited and is not reaped yet
)

// The calls that touch the group or look at it, which tests replace.
var (
	signalGroup = func(pgid int) error { return syscall.Kill(-pgid, syscall.SIGKILL) }
	reap        = (*exec.Cmd).Wait
	probe       = stateOf
)

// Options are the output of the child and how long it may outlive the leader.
type Options struct {
	// Stdout and Stderr receive what the child writes; nil discards it.
	Stdout, Stderr io.Writer
	// ReadStdout: the caller reads stdout itself, from Group.Stdout, and is done
	// with it before Wait. Wait discards what is left.
	ReadStdout bool
	// Drain is how long the pipes may stay open once the leader has ended: what
	// still holds them then is swept with the group. DefaultDrain if zero.
	Drain time.Duration
}

// DefaultDrain is the Drain of Options that give none.
const DefaultDrain = 3 * time.Second

// Group is a started child that leads its own process group.
type Group struct {
	cmd      *exec.Cmd
	ctx      context.Context
	drainFor time.Duration
	stdout   *os.File   // the read end of stdout, for ReadStdout
	reads    []*os.File // the read ends of the pipes, closed at the end
	copying  sync.WaitGroup
	stdin    *os.File // the write end of a stdin this package feeds
	stop     chan struct{}
	watching chan struct{}
}

// Start starts cmd as the leader of a new process group. If ctx ends before
// Wait has reaped it, the whole group is killed. cmd must not have a Context of
// its own (its Cancel could signal the group after the leader is reaped), nor
// Stdout or Stderr: they go in opts. A Stdin that is not a file is fed by this
// package from a goroutine that Wait does not wait for: it must end on its own
// without blocking (a bytes or strings Reader), or it outlives the Group.
func Start(ctx context.Context, cmd *exec.Cmd, opts Options) (_ *Group, err error) {
	if cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, errors.New("procgroup: the output of the child goes in Options")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if opts.Drain <= 0 {
		opts.Drain = DefaultDrain
	}
	g := &Group{cmd: cmd, ctx: ctx, drainFor: opts.Drain, stop: make(chan struct{}), watching: make(chan struct{})}
	var parentEnds []*os.File // the child's ends, which the parent closes once it started
	defer func() {
		for _, f := range parentEnds {
			_ = f.Close()
		}
		if err != nil {
			for _, f := range g.reads {
				_ = f.Close()
			}
			if g.stdin != nil {
				_ = g.stdin.Close()
			}
		}
	}()
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	g.reads, parentEnds = append(g.reads, outR), append(parentEnds, outW)
	errR, errW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	g.reads, parentEnds = append(g.reads, errR), append(parentEnds, errW)
	cmd.Stdout, cmd.Stderr = outW, errW
	var feed io.Reader
	if _, isFile := cmd.Stdin.(*os.File); cmd.Stdin != nil && !isFile {
		inR, inW, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		feed, g.stdin, parentEnds = cmd.Stdin, inW, append(parentEnds, inR)
		cmd.Stdin = inR
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if opts.ReadStdout {
		g.stdout = outR
	} else {
		g.copy(opts.Stdout, outR)
	}
	g.copy(opts.Stderr, errR)
	if feed != nil {
		go func() {
			_, _ = io.Copy(g.stdin, feed)
			_ = g.stdin.Close()
		}()
	}
	go g.watch()
	return g, nil
}

// Stdout is the output of the child, for Options.ReadStdout.
func (g *Group) Stdout() io.Reader { return g.stdout }

// copy drains a pipe into w until EOF; what w refuses is discarded, so the
// child is never blocked on it.
func (g *Group) copy(w io.Writer, r io.Reader) {
	g.copying.Add(1)
	go func() {
		defer g.copying.Done()
		if w != nil {
			if _, err := io.Copy(w, r); err == nil {
				return
			}
		}
		_, _ = io.Copy(io.Discard, r)
	}()
}

// watch kills the group when ctx ends, which is what wakes a caller that reads
// the pipes of the child; Wait sweeps it again before it reaps the leader.
func (g *Group) watch() {
	defer close(g.watching)
	select {
	case <-g.ctx.Done():
		_ = sweep(g.cmd.Process.Pid)
	case <-g.stop:
	}
}

// Wait ends the child: it waits for the leader to exit without reaping it,
// drains the pipes (sweeping the group if they stay open Drain after the
// leader ended, or if ctx ends), sweeps the group if ctx ended, and only then
// reaps the leader. It returns what cmd.Wait returns, joined with a sweep that
// left a process alive.
func (g *Group) Wait() error {
	pgid := g.cmd.Process.Pid
	g.awaitLeader()
	var swept error
	if !g.drain() {
		swept = sweep(pgid)
		if !g.drain() {
			// A holder outside the group: the pipes are closed under it.
			g.closeReads()
			g.copying.Wait()
		}
	}
	close(g.stop)
	<-g.watching
	if g.ctx.Err() != nil {
		swept = sweep(pgid)
	}
	err := reap(g.cmd)
	g.closeReads()
	if g.stdin != nil {
		_ = g.stdin.Close() // a feed nobody reads ends here
	}
	return errors.Join(err, swept)
}

// awaitLeader returns once the leader has exited and is a zombie, or once ctx
// ended and the group was killed: the leader cannot outlive that SIGKILL, and
// the reap waits for it. A look that fails is tried again.
func (g *Group) awaitLeader() {
	pid := g.cmd.Process.Pid
	for pause := time.Millisecond; probe(pid) != ended; pause = min(2*pause, maxPoll) {
		select {
		case <-g.watching:
			return
		case <-time.After(pause):
		}
	}
}

// drain waits for the pipes to reach EOF, for at most Drain, and says whether
// they did.
func (g *Group) drain() bool {
	if g.stdout != nil {
		g.copy(nil, g.stdout)
		g.stdout = nil
	}
	done := make(chan struct{})
	go func() {
		g.copying.Wait()
		close(done)
	}()
	timer := time.NewTimer(g.drainFor)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (g *Group) closeReads() {
	for _, f := range g.reads {
		_ = f.Close()
	}
}

// sweep kills the group until no live process is left in it, the zombie of the
// leader aside. One kill is not enough: a member that is forking when it lands
// may give birth to a child the signal missed (macOS does).
func sweep(pgid int) error {
	deadline := time.Now().Add(sweepFor)
	for {
		_ = signalGroup(pgid) // EPERM when only zombies are left
		members, err := Members(pgid)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("procgroup: %d processes of group %d outlived its kill", len(members), pgid)
		}
		time.Sleep(sweepInterval)
	}
}

// Members lists the live processes of a process group, leaving out those that
// already exited and wait for their parent.
func Members(pgid int) ([]int, error) { return members(pgid) }
