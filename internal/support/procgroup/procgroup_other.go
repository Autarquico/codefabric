//go:build !linux && !darwin

package procgroup

import "errors"

func members(int) ([]int, error) {
	return nil, errors.New("procgroup: cannot list a process group on this platform")
}

// stateOf cannot look at a process without reaping it here: the leader counts
// as ended, and the reap waits for it. Only the kill of a ctx that ended, which
// goes before the reap, reaches the group.
func stateOf(int) leaderState { return ended }
