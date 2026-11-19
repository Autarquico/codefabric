package procident_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Autarquico/codeflock/internal/worker/procident"
)

// A token recorded before the boot session uuid carries kern.boottime, which
// macOS moves by milliseconds when it adjusts the clock. Within that drift it is
// only likely the same boot: the process is the same if its start matches, and
// otherwise nothing may be signalled on its account.
func TestVerifyReadsAKernBoottimeTokenWithItsDrift(t *testing.T) {
	start, err := procident.StartOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	_, own, _ := strings.Cut(start, "|")
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		t.Fatal(err)
	}
	legacyOf := func(shift time.Duration, start string) string {
		at := time.Unix(boot.Unix()).Add(shift)
		return fmt.Sprintf("%d.%06d|%s", at.Unix(), at.Nanosecond()/1000, start)
	}
	legacy := func(shift time.Duration) string { return legacyOf(shift, own) }
	if v := procident.Verify(os.Getpid(), legacy(50*time.Millisecond)); v != procident.Same {
		t.Errorf("kern.boottime moved by 50ms: %v, want same", v)
	}
	if v := procident.Verify(os.Getpid(), legacy(-3*time.Second)); v != procident.Same {
		t.Errorf("kern.boottime moved by 3s: %v, want same", v)
	}
	if v := procident.Verify(os.Getpid(), legacyOf(50*time.Millisecond, "another-start")); v != procident.Rebooted {
		t.Errorf("within the drift, another start: %v, want rebooted (nothing to signal)", v)
	}
	if v := procident.Verify(os.Getpid(), legacy(-time.Hour)); v != procident.Rebooted {
		t.Errorf("kern.boottime an hour before: %v, want rebooted", v)
	}
	for _, bad := range []string{
		fmt.Sprintf("%d.-1000000|%s", boot.Sec+1, own),
		fmt.Sprintf("%d.5|%s", boot.Sec, own),
		fmt.Sprintf("-%d.000000|%s", boot.Sec, own),
		"99999999999999999999.000000|" + own,
	} {
		if v := procident.Verify(os.Getpid(), bad); v != procident.Unknown {
			t.Errorf("malformed boot %q: %v, want unknown", bad, v)
		}
	}
}

func TestStartOfNamesTheBootByItsSessionUUID(t *testing.T) {
	start, err := procident.StartOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	uuid, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		t.Fatal(err)
	}
	if boot, _, _ := strings.Cut(start, "|"); boot != uuid {
		t.Errorf("boot of the token = %q, want the boot session uuid %q", boot, uuid)
	}
}
