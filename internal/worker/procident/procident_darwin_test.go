package procident_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// A token recorded before the boot session uuid carries kern.boottime, which
// macOS moves by milliseconds when it adjusts the clock: that is the same boot.
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
	legacy := func(shift time.Duration) string {
		at := time.Unix(boot.Unix()).Add(shift)
		return fmt.Sprintf("%d.%06d|%s", at.Unix(), at.Nanosecond()/1000, own)
	}
	if v := procident.Verify(os.Getpid(), legacy(50*time.Millisecond)); v != procident.Same {
		t.Errorf("kern.boottime moved by 50ms: %v, want same", v)
	}
	if v := procident.Verify(os.Getpid(), legacy(-time.Hour)); v != procident.Rebooted {
		t.Errorf("kern.boottime an hour before: %v, want rebooted", v)
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
