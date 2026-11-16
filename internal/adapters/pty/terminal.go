package pty

import (
	"slices"
	"syscall"
	"time"
)

const (
	// pollEvery is how often the terminal is looked at while the command runs.
	pollEvery = 100 * time.Millisecond
	// sweepFor is how long Close keeps killing what it finds on the terminal.
	sweepFor = 2 * time.Second
	// sweepEvery is the pause between two passes of the sweep.
	sweepEvery = 20 * time.Millisecond
)

// proc is a process on the terminal, with the process group it is in. How the
// processes of a terminal are listed depends on the OS (terminalProcs).
type proc struct{ pid, pgid int }

// watchTerminal keeps in seen what the terminal has, until the command ends or
// Close takes over. A command that ends by itself takes the terminal away from
// the processes it leaves, so this is the last place to know them.
func (s *Session) watchTerminal() {
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
		}
		procs, ok := s.look()
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		if ok {
			s.seen = procs
		}
		s.mu.Unlock()
	}
}

// look is a list of the processes of the terminal that can be trusted to be
// whole, or false. When the command ends, the kernel takes the terminal from the
// processes of its session one by one, so a list taken then can have the command
// and miss its jobs. A list is whole when the command was still on the terminal
// in a second one taken after it.
func (s *Session) look() ([]proc, bool) {
	procs, err := s.terminalProcs()
	if err != nil {
		return nil, false
	}
	again, err := s.terminalProcs()
	leader := func(p proc) bool { return p.pid == s.cmd.Process.Pid }
	return procs, err == nil && slices.ContainsFunc(again, leader) && slices.ContainsFunc(procs, leader)
}

// sweepTerminal kills the process group of everything that has the terminal, and
// looks again until nothing is left or sweepFor passes. The command is still
// there (not reaped), so no number it names can belong to anyone else. When ps
// cannot be run it falls back to the group of the command.
func (s *Session) sweepTerminal() {
	s.mu.Lock()
	s.closed, s.seen = true, nil
	s.mu.Unlock()
	deadline := time.Now().Add(sweepFor)
	for {
		procs, err := s.terminalProcs()
		if err != nil {
			killGroups(nil, s.cmd.Process.Pid)
			return
		}
		if len(procs) == 0 || time.Now().After(deadline) {
			return
		}
		killGroups(procs, 0)
		time.Sleep(sweepEvery)
	}
}

// killLeftBehind runs right after the command was reaped without Close: it kills
// the groups of the processes the terminal had at the last look, other than the
// command itself. A group of the command's own number is killed only for those.
func (s *Session) killLeftBehind() {
	s.mu.Lock()
	seen := s.seen
	s.seen = nil
	s.mu.Unlock()
	leader := s.cmd.Process.Pid
	others := seen[:0:0]
	for _, p := range seen {
		if p.pid != leader {
			others = append(others, p)
		}
	}
	killGroups(others, 0)
}

// killGroups kills the process group of each of procs, and group extra if it is not 0.
func killGroups(procs []proc, extra int) {
	done := map[int]bool{}
	for _, p := range procs {
		if !done[p.pgid] {
			done[p.pgid] = true
			_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
		}
	}
	if extra > 1 {
		_ = syscall.Kill(-extra, syscall.SIGKILL)
	}
}
