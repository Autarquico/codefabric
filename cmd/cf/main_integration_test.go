//go:build integration && (linux || darwin)

package main

import (
	"os"
	"testing"
)

// asCF makes the test binary behave as `cf`: tests that need a daemon in a
// process of its own, which they can kill, start the binary they are running
// with this variable set. The wrapper the daemon starts in tmux is this binary
// too, found by os.Executable, and the variable reaches it through tmux's
// environment; the harness it starts must not inherit it.
const asCF = "CF_TEST_AS_CF"

func TestMain(m *testing.M) {
	// A Controller in a process of its own runs the hook of the relay as itself,
	// with an environment built from nothing: the arguments say it is cf.
	hook := len(os.Args) > 3 && os.Args[1] == "relay" && os.Args[2] == "hook" && os.Args[3] == "pre-receive"
	if os.Getenv(asCF) == "1" || hook {
		if len(os.Args) > 2 && os.Args[1] == "worker" && os.Args[2] == "exec" {
			_ = os.Unsetenv(asCF)
		}
		os.Exit(run())
	}
	os.Exit(m.Run())
}
