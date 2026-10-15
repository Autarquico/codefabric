// Package harness is the port of the programs that do a Run's work: what a
// Worker needs of one to start it. Phase 2 (harness-adapter-framework) grows it
// into the interface of §7.2; today it is the part that executing a Run needs.
package harness

import "github.com/Autarquico/codefabric/internal/domain"

// LaunchSpec is what a Launcher is given to build the command of a Run.
type LaunchSpec struct {
	// RunDir is the directory of the Run on the Worker, where a Launcher may
	// leave what its command needs.
	RunDir string
	// Worktree is where the command runs.
	Worktree string
	Prompt   string
	Mode     domain.Mode
}

// Launch is the process to start: its argv, and the environment it adds to the
// one the wrapper has.
type Launch struct {
	Argv []string
	Env  []string
	// SecretEnv are variables whose values the wrapper resolves just before it
	// starts the process. Only the refs are written down; with no resolver, a
	// Launch that has any fails before the process starts (CF_SECRET_UNAVAILABLE).
	SecretEnv []SecretEnv
	// StructuredStdout asks for stdout apart from stderr, for a harness whose
	// events are a stream on stdout. Reserved: the wrapper ignores it until a
	// structured adapter needs it.
	StructuredStdout bool
}

// Launcher builds the command of one harness.
type Launcher interface {
	// ID is the name a Run asks for in requirements.harness.
	ID() string
	// Modes are the execution modes it supports (§7.4). A Run that asks for
	// another one is refused before it starts.
	Modes() []domain.Mode
	Launch(LaunchSpec) (Launch, error)
}
