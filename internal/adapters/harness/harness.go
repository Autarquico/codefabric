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
	// events are a stream on stdout. Reserved: the wrapper ignores it until a
	// structured adapter needs it.
	StructuredStdout bool
}
