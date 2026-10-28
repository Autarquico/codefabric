package harness

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
}
