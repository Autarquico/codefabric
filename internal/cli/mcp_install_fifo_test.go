//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMCPInstall_ASkillThatIsAFIFOIsAnErrorNotAHang(t *testing.T) {
	for _, args := range [][]string{{"--claude"}, {"--claude", "--dry-run"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			m := newMCPMachine(t)
			if err := os.MkdirAll(filepath.Dir(m.skill("claude")), 0o755); err != nil { //nolint:gosec // a test directory
				t.Fatal(err)
			}
			if err := unix.Mkfifo(m.skill("claude"), 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var code int
			var errOut string
			go func() {
				defer close(done)
				code, _, errOut = run(t, append([]string{"mcp", "install"}, args...)...)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("install blocked on the FIFO")
			}
			if code == 0 || !strings.Contains(errOut, "regular file") {
				t.Fatalf("exit %d: %s", code, errOut)
			}
		})
	}
}
