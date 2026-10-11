//go:build integration && (linux || darwin)

package executor

import (
	"os"
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

const asWrapper = "CF_TEST_AS_CF"

// The Runtime starts `<this binary> worker exec --run-dir <dir>` in a tmux
// pane. In the tests this binary is the test binary, which is the wrapper when
// it is asked to be: it answers before the testing package reads its flags.
func TestMain(m *testing.M) {
	if os.Getenv(asWrapper) == "1" && len(os.Args) == 5 && os.Args[1] == "worker" && os.Args[2] == "exec" && os.Args[3] == "--run-dir" {
		_ = os.Unsetenv(asWrapper) // the harness is not a wrapper
		err := execwrap.Run(execwrap.Options{RunDir: os.Args[4], Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	_ = os.Setenv(asWrapper, "1") // inherited by the tmux server, and so by the panes
	os.Exit(m.Run())
}
