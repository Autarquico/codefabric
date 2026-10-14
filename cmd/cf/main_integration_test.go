//go:build integration && (linux || darwin)

package main

import (
	"encoding/json"
	"os"
	"testing"
)

// asCF makes the test binary behave as `cf`: tests that need a daemon in a
// process of its own, which they can kill, start the binary they are running
// with this variable set. The wrapper the daemon starts in tmux is this binary
// too, found by os.Executable, and the variable reaches it through tmux's
// environment; the harness it starts must not inherit it.
const asCF = "CF_TEST_AS_CF"

// echoTo makes the test binary write what it was started with to the file it
// names, and exit: a service of the tests that only says what it was given.
const echoTo = "CF_TEST_ECHO"

// echoed is what a service started with echoTo writes.
type echoed struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

func TestMain(m *testing.M) {
	// A binary built with -race sleeps a second before it exits, to let its
	// reports out. The wrapper of every Run is this binary, so under -race a Run
	// would end a second after its exit marker: a cost of the test build, not of
	// cf. The children inherit it.
	if os.Getenv("GORACE") == "" {
		_ = os.Setenv("GORACE", "atexit_sleep_ms=0")
	}

	if out := os.Getenv(echoTo); out != "" {
		env := map[string]string{}
		for _, key := range []string{"CF_CONFIG_DIR", "CF_DATA_DIR", "PATH", "TMUX_TMPDIR"} {
			env[key] = os.Getenv(key)
		}
		raw, _ := json.Marshal(echoed{Args: os.Args[1:], Env: env})
		if err := os.WriteFile(out, raw, 0o600); err != nil { //nolint:gosec // a file the test names
			os.Exit(1)
		}
		os.Exit(0)
	}

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
