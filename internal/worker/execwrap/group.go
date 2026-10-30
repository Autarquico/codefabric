package execwrap

import (
	"syscall"

	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// GroupOthers is the number of live processes of the group pgid but self. It
// only counts: it is never used to choose whom to signal. If it cannot tell, it
// says 1, so that the group is asked to end rather than assumed empty.
func GroupOthers(pgid, self int) int {
	members, err := procident.GroupMembers(pgid)
	if err != nil {
		return 1
	}
	n := 0
	for _, pid := range members {
		if pid != self {
			n++
		}
	}
	return n
}

// KillGroup is SIGKILL to the group of a process that leads it and keeps it
// reserved by being alive. The leader is in the group: it kills itself too.
func KillGroup(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }
