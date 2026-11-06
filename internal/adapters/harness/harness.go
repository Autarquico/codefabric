package harness

import "context"

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
	// events are a stream on stdout. In headless mode the wrapper writes stdout
	// to RunSpec.EventsPath (events.jsonl in the run directory), redacted line
	// by line, and stderr to OutputPath. Interactive mode ignores it: the
	// terminal is a single stream.
	StructuredStdout bool
	// InputPath, when set, makes the stdin of a headless process a FIFO at that
	// path, which the wrapper creates, holds open for writing for as long as the
	// Run lives (the process never reads EOF because a daemon went away) and
	// closes when the Run says its input is over.
	InputPath string
	// InitialInput is what the wrapper writes to the FIFO as soon as the process
	// starts: the first lines of its input (the prompt, as the harness reads it).
	InitialInput string
}

// StructuredInputChecker is implemented by an adapter whose structured input
// depends on the harness installed in the host: the Worker announces
// structured-input:v1 for it only if the check passes.
type StructuredInputChecker interface {
	// CheckStructuredInput is nil when the harness of the host has what the
	// structured input needs, and says what is missing otherwise.
	CheckStructuredInput(ctx context.Context, host Host) error
}
