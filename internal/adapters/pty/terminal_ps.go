//go:build !linux

package pty

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// psTimeout bounds one look at the terminal.
const psTimeout = 2 * time.Second

// terminalProcs lists the processes whose controlling terminal is the pty, as
// ps -t does (macOS and the other BSDs; Linux reads /proc, see terminal_linux.go).
func (s *Session) terminalProcs() ([]proc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-t", strings.TrimPrefix(s.tty, "/dev/"), "-o", "pid=,pgid=").Output() //nolint:gosec // the name is that of the pty this process opened
	if err != nil {
		return nil, err
	}
	var procs []proc
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pgid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil && pid > 1 && pgid > 1 {
			procs = append(procs, proc{pid, pgid})
		}
	}
	return procs, nil
}
