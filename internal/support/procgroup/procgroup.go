// Package procgroup runs a child as the leader of a process group of its own
// and ends the whole group with it.
//
// Every signal to the group goes while its leader is not reaped: the leader,
// alive or a zombie, keeps the ID of the group from being given to another
// process, so a signal can only reach the group. The leader is reaped last.
package procgroup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

const (
	// sweepFor bounds how long a group is swept after its kill.
	sweepFor = 3 * time.Second
	// sweepInterval is the pause between two kills of a sweep.
	sweepInterval = 10 * time.Millisecond
	// maxPoll is the longest pause between two looks at a leader that has not ended.
	maxPoll = 20 * time.Millisecond
)

// The calls that touch the group, which tests replace to see their order.
var (
	signalGroup = func(pgid int) error { return syscall.Kill(-pgid, syscall.SIGKILL) }
	reap        = (*exec.Cmd).Wait
)

// Group is a started child that leads its own process group.
type Group struct {
	cmd      *exec.Cmd
	ctx      context.Context
	stop     chan struct{}
	watching chan struct{}
}

// Start starts cmd as the leader of a new process group. If ctx ends before
// Wait has reaped it, the whole group is killed. cmd must not have a Context of
// its own: its Cancel could signal the group after the leader is reaped.
func Start(ctx context.Context, cmd *exec.Cmd) (*Group, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	g := &Group{cmd: cmd, ctx: ctx, stop: make(chan struct{}), watching: make(chan struct{})}
	go g.watch()
	return g, nil
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

// Wait waits for the leader to end, then, if ctx ended, sweeps what is left of
// the group, and only then reaps the leader. It returns what cmd.Wait returns,
// joined with a sweep that left a process alive.
func (g *Group) Wait() error {
	pgid := g.cmd.Process.Pid
	for pause := time.Millisecond; alive(pgid); pause = min(2*pause, maxPoll) {
		time.Sleep(pause)
	}
	close(g.stop)
	<-g.watching
	var swept error
	if g.ctx.Err() != nil {
		swept = sweep(pgid)
	}
	return errors.Join(reap(g.cmd), swept)
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

// alive says whether the process is running, not a zombie. A process that
// cannot be read counts as ended.
func alive(pid int) bool {
	ok, err := running(pid)
	return err == nil && ok
}

// Members lists the live processes of a process group, leaving out those that
// already exited and wait for their parent.
func Members(pgid int) ([]int, error) { return members(pgid) }
