//go:build integration && (linux || darwin)

package main

import (
	"os"
	"testing"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

// asCF makes the test binary behave as `cf`: tests that need a daemon in a
// process of its own, which they can kill, start the binary they are running
// with this variable set. The wrapper the daemon starts in tmux is this binary
// too, found by os.Executable, and the variable reaches it through tmux's
// environment; the harness it starts must not inherit it.
const asCF = "CF_TEST_AS_CF"

func TestMain(m *testing.M) {
	if os.Getenv(asCF) == "1" {
		if len(os.Args) > 2 && os.Args[1] == "worker" && os.Args[2] == "exec" {
			_ = os.Unsetenv(asCF)
		}
		os.Exit(cli.Execute(daemon.Command(), workerdaemon.Command()))
	}
	os.Exit(m.Run())
}
