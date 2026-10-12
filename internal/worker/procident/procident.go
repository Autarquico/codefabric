// Package procident tells one process from another that got its pid later. A
// pid is reused, so the daemon never signals a pid, or the group it leads, on
// the strength of the number alone: it signals it when the process started at
// the instant the wrapper recorded (§10.7).
package procident

import (
	"errors"
	"strings"
	"syscall"
)

// Verdict is what comparing a recorded identity with the process that has the
// pid now says.
type Verdict int

// Verdicts.
const (
	// Gone: no process has the pid.
	Gone Verdict = iota
	// Same: a process has the pid and started when it was recorded.
	Same
	// Different: a process has the pid but started at another time. The pid
	// was reused, so the process recorded is gone.
	Different
	// Unknown: the platform or the system would not say, or there is no usable token.
	Unknown
	// Rebooted: the token is of another boot of the machine. No process survives
	// a reboot, so the process is gone and, whoever has the pid now, nothing in
	// the machine is its group: nothing may be signalled on its account.
	Rebooted
)

func (v Verdict) String() string {
	return [...]string{"gone", "same", "different", "unknown", "rebooted"}[v]
}

// ErrUnverifiable is returned where the platform has no way to read when a
// process started.
var ErrUnverifiable = errors.New("procident: this platform cannot tell when a process started")

// ErrGone is returned for a pid that no process has.
var ErrGone = errors.New("procident: no such process")

// StartOf is an opaque token of when the process with the pid started, and of
// the boot of the machine it started in. Two processes that had the pid at
// different times never have the same token.
func StartOf(pid int) (string, error) {
	start, err := startOf(pid)
	if err != nil {
		return "", err
	}
	boot, err := bootOf()
	if err != nil {
		return "", err
	}
	return boot + "|" + start, nil
}

// Verify compares the process that has the pid now with the token recorded
// when it was known. Only a token that is whole says anything: an empty or
// malformed one is Unknown, never Gone, so that nothing is killed on its account.
// Gone means that, in the boot the token is of, no process has the pid.
func Verify(pid int, token string) Verdict {
	boot, start, ok := strings.Cut(token, "|")
	if !ok || boot == "" || start == "" {
		return Unknown
	}
	now, err := bootOf()
	if err != nil {
		return Unknown
	}
	if now != boot {
		return Rebooted
	}
	current, err := startOf(pid)
	switch {
	case errors.Is(err, ErrGone):
		return Gone
	case err != nil:
		return Unknown
	case current == start:
		return Same
	}
	return Different
}

// GroupMembers lists the live processes of a process group, leaving out those
// that already exited and wait for their parent.
func GroupMembers(pgid int) ([]int, error) { return groupMembers(pgid) }

// GroupEmpty says whether the group has no process left. kill(-pgid, 0)
// reports ESRCH for an empty group; a group that holds only zombies is empty
// too.
func GroupEmpty(pgid int) (bool, error) {
	err := syscall.Kill(-pgid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, err
	}
	members, err := groupMembers(pgid)
	if err != nil {
		return false, err
	}
	return len(members) == 0, nil
}
