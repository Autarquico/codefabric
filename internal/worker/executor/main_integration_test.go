//go:build integration && (linux || darwin)

package executor

import (
	"os"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/envwrap"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
)

const asWrapper = "CF_TEST_AS_CF"

// The Runtime starts `<this binary> worker exec --run-dir <dir>` in a tmux
// pane, and `<this binary> worker redact-pipe --run-dir <dir>` as the capture
// filter of an interactive Run with secrets. In the tests this binary is the
// test binary, which is the wrapper or the filter when it is asked to be, and
// the fakeharness the wrapper starts when that is asked: it answers before the
// testing package reads its flags.
func TestMain(m *testing.M) {
	fakeharness.RunIfRequested()
	if os.Getenv(asWrapper) == "1" && len(os.Args) == 6 && os.Args[1] == "worker" && os.Args[2] == "exec" && os.Args[3] == "env" && os.Args[4] == "--run-dir" {
		// The supervisor of the Environment of a Run (`worker exec env`).
		dir := os.Args[5]
		release, err := HoldCapture(dir)
		if err != nil {
			os.Exit(1)
		}
		err = envwrap.Run(envwrap.Options{RunDir: dir, Secrets: secretjit.Resolver(dir), Retention: func() ([]redact.Needle, error) { return secretjit.Load(dir) }})
		release()
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(asWrapper) == "1" && len(os.Args) == 5 && os.Args[1] == "worker" && os.Args[3] == "--run-dir" {
		dir := os.Args[4]
		retention := func() ([]redact.Needle, error) { return secretjit.Load(dir) }
		release, err := HoldCapture(dir)
		if err != nil {
			os.Exit(1)
		}
		switch os.Args[2] {
		case "exec":
			_ = os.Unsetenv(asWrapper) // the harness is not a wrapper
			err = execwrap.Run(execwrap.Options{
				RunDir: dir, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
				Secrets: secretjit.Resolver(dir), Retention: retention,
			})
		case "redact-pipe":
			err = execwrap.RedactPipe(execwrap.PipeOptions{RunDir: dir, Retention: retention, In: os.Stdin, Out: os.Stdout})
		default:
			os.Exit(2)
		}
		release()
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	_ = os.Setenv(asWrapper, "1") // inherited by the tmux server, and so by the panes
	os.Exit(m.Run())
}
