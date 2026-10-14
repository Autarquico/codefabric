package tmux

import "testing"

// Only the absence of a server means there are no sessions: any other failure
// leaves them unknown.
func TestNoServer(t *testing.T) {
	for stderr, want := range map[string]bool{
		"no server running on /tmp/tmux-501/cf-abc\n":                                 true,
		"error connecting to /tmp/tmux-501/cf-abc (No such file or directory)\n":      true,
		"error connecting to /tmp/tmux-501/cf-abc (Permission denied)\n":              false,
		"error connecting to /tmp/tmux-501/cf-abc (Socket operation on non-socket)\n": false,
		"directory /tmp/tmux-501 has unsafe permissions\n":                            false,
		"command list-sessions: unknown flag -Z\n":                                    false,
		"": false,
	} {
		if got := noServer(stderr); got != want {
			t.Errorf("%q: %t", stderr, got)
		}
	}
}
