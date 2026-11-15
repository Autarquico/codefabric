package procident

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const zombie = 5 // SZOMB in <sys/proc.h>

func startOf(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A pid with no process answers with no data.
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return "", ErrGone
		}
		return "", err
	}
	if info.Proc.P_stat == zombie {
		return "", ErrGone
	}
	t := info.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", t.Sec, t.Usec), nil
}

// bootOf is the boot session uuid. kern.boottime is not an identity of the boot:
// macOS moves it by milliseconds when it adjusts the clock.
func bootOf() (string, error) { return unix.Sysctl("kern.bootsessionuuid") }

// bootDrift is how far kern.boottime may move within one boot.
const bootDrift = 5 * time.Second

func sameBoot(recorded string) (bool, error) {
	if legacy, ok := parseBootTime(recorded); ok {
		now, err := unix.SysctlTimeval("kern.boottime")
		if err != nil {
			return false, err
		}
		return withinDrift(legacy, time.Unix(now.Unix())), nil
	}
	now, err := bootOf()
	return recorded == now, err
}

// parseBootTime reads a boot token of the earlier format, kern.boottime as
// "sec.usec", which wrappers started before the uuid still carry.
func parseBootTime(token string) (time.Time, bool) {
	sec, usec, ok := strings.Cut(token, ".")
	if !ok {
		return time.Time{}, false
	}
	s, errS := strconv.ParseInt(sec, 10, 64)
	u, errU := strconv.ParseInt(usec, 10, 64)
	if errS != nil || errU != nil {
		return time.Time{}, false
	}
	return time.Unix(s, u*int64(time.Microsecond)), true
}

func withinDrift(recorded, now time.Time) bool {
	return now.Sub(recorded).Abs() <= bootDrift
}
