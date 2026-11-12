package session

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// maxAncestors bounds the walk up the process tree: a loop in a table that is read while
// processes come and go must not hang the command.
const maxAncestors = 64

// parentOf is the parent of a process: /proc where there is one (Linux), `ps` elsewhere (macOS).
func parentOf(pid int) (int, bool) {
	if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		// pid (comm) state ppid ...: comm may hold spaces and parentheses, so cut after the last ")".
		s := string(raw)
		if i := strings.LastIndexByte(s, ')'); i >= 0 {
			if fields := strings.Fields(s[i+1:]); len(fields) >= 2 {
				ppid, err := strconv.Atoi(fields[1])
				return ppid, err == nil
			}
		}
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // a child process: real time
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // pid is an int
	if err != nil {
		return 0, false
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return ppid, err == nil
}

// UseProcesses replaces the process table: the pid of the process that runs the command, who
// is above a pid, and whether a pid is alive. For tests that need a tree of processes.
func (l *Local) UseProcesses(self int, parent func(pid int) (ppid int, ok bool), alive func(int) bool) {
	l.self, l.parent, l.pidAlive = func() int { return self }, parent, alive
}

// hasAncestor says whether pid is a process above the one that runs this command.
func (l *Local) hasAncestor(pid int) bool {
	for p, i := l.self(), 0; i < maxAncestors; i++ {
		parent, ok := l.parent(p)
		if !ok || parent <= 1 {
			return false
		}
		if parent == pid {
			return true
		}
		p = parent
	}
	return false
}
