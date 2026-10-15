package harness

import (
	"context"

	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Host is the local machine an adapter acts on (§7.2): the daemon of the
// Worker for Runs and apply, the `cf` client for adopt. It has no file
// system: an adapter uses os under Home, so a test mounts Home in a
// temporary directory.
type Host interface {
	// Home is the home directory whose native configuration the adapter
	// reads and writes.
	Home() string
	// Environ is the environment of the host, as os.Environ.
	Environ() []string
	Getenv(key string) string
	// LookPath finds a binary in the PATH of the host, as exec.LookPath.
	LookPath(file string) (string, error)
	// Run runs a command to completion. It is bounded: by ctx, and by
	// MaxCmdOutput on each of stdout and stderr. A command that exits with a
	// non-zero code is a CmdResult, not an error; the error is for one that
	// could not run or was cut short.
	Run(ctx context.Context, cmd Cmd) (CmdResult, error)
	Clock() clock.Clock
}

// MaxCmdOutput is the most of stdout, and of stderr, Host.Run keeps. What
// goes beyond it is dropped and the result says so.
const MaxCmdOutput = 1 << 20

// Cmd is a command for Host.Run.
type Cmd struct {
	// Argv is the command; Argv[0] is resolved with LookPath when it is not
	// a path.
	Argv []string
	// Dir is the working directory; empty is Home.
	Dir string
	// Env is added to Environ, overriding the same keys.
	Env   []string
	Stdin []byte
}

// CmdResult is how a command ended.
type CmdResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	// Truncated says that stdout or stderr went beyond MaxCmdOutput.
	Truncated bool
}
